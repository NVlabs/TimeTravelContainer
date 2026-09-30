// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package ebpf provides tools for working with extended Berkely Packet Filter (eBPF) programs.
//
// gVisor currently does not support running eBPF programs: they, and the
// maps they would use, exist for the bpf(2) system call alone, which loads,
// pins, queries, and attaches programs, and reads and writes maps.
package ebpf

import (
	"crypto/sha1"
	"encoding/binary"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/atomicbitops"
)

// BPFID is a sequential, globally-unique (though unloaded
// programs' IDs are reused) ID for an eBPF program.
type BPFID uint32

// ProgramOptions describes a program to load.
type ProgramOptions struct {
	// Type is the program's type.
	Type linux.BPFProgramType

	// ExpectedAttachType is the attachment type that the program expects.
	ExpectedAttachType linux.BPFAttachType

	// Name is the program's name.
	Name string

	// Instructions are the program's instructions.
	Instructions []linux.EBPFInstruction

	// GPLCompatible is whether the program's license is compatible with the
	// GPL.
	GPLCompatible bool

	// UID is the KUID of the program's creator.
	UID uint32

	// LoadTime is when the program was loaded, in nanoseconds since boot.
	LoadTime int64

	// Maps are the maps that the program uses. The program takes a
	// reference on each.
	Maps []*Map
}

// Program represents an eBPF program. It is never run.
//
// All fields of Program, but refs, are immutable.
//
// +stateify savable
type Program struct {
	// refs is the number of references on the program: from file
	// descriptions, pins, and attachments.
	refs atomicbitops.Int64

	// registry is the registry that the program's memory is charged to.
	registry *Registry

	// cost is the memory charged for the program.
	cost int64

	// instructions is a list of eBPF instructions.
	instructions []linux.EBPFInstruction

	// id is the program's ID.
	id BPFID

	// progType is the program's type.
	progType linux.BPFProgramType

	// expectedAttachType is the attachment type that the program expects.
	expectedAttachType linux.BPFAttachType

	// name is the program's name.
	name string

	// tag is the program's tag: a digest of its instructions.
	tag [linux.BPF_TAG_SIZE]byte

	// gplCompatible is whether the program's license is compatible with the
	// GPL.
	gplCompatible bool

	// uid is the KUID of the program's creator.
	uid uint32

	// loadTime is when the program was loaded, in nanoseconds since boot.
	loadTime int64

	// maps are the maps that the program uses.
	maps []*Map
}

// NewProgram returns a program, which holds a reference, as the caller's.
//
// Programs are not verified, since they never run.
func (r *Registry) NewProgram(opts ProgramOptions) (*Program, error) {
	cost := int64(len(opts.Instructions))*linux.BPF_INSTRUCTION_SIZE + objectOverhead
	if err := r.charge(cost); err != nil {
		return nil, err
	}
	p := &Program{
		registry:           r,
		cost:               cost,
		instructions:       opts.Instructions,
		id:                 r.newID(&r.nextProgID),
		progType:           opts.Type,
		expectedAttachType: opts.ExpectedAttachType,
		name:               opts.Name,
		tag:                programTag(opts.Instructions),
		gplCompatible:      opts.GPLCompatible,
		uid:                opts.UID,
		loadTime:           opts.LoadTime,
		maps:               opts.Maps,
	}
	for _, m := range p.maps {
		m.IncRef()
	}
	p.refs.Store(1)
	return p, nil
}

// programTag returns the tag of a program with instructions insns, as
// Linux's bpf_prog_calc_tag does: the first bytes of the SHA-1 of the
// instructions, less the file descriptors of maps that they load.
func programTag(insns []linux.EBPFInstruction) [linux.BPF_TAG_SIZE]byte {
	buf := make([]byte, 0, len(insns)*linux.BPF_INSTRUCTION_SIZE)
	wasLdMap := false
	for _, insn := range insns {
		imm := insn.Immediate
		src := insn.Registers >> 4
		switch {
		case !wasLdMap && insn.Code == ldImm64 && (src == linux.BPF_PSEUDO_MAP_FD || src == linux.BPF_PSEUDO_MAP_VALUE):
			wasLdMap = true
			imm = 0
		case wasLdMap && insn.Code == 0 && insn.Registers == 0 && insn.Offset == 0:
			wasLdMap = false
			imm = 0
		default:
			wasLdMap = false
		}
		buf = append(buf, insn.Code, insn.Registers)
		buf = binary.LittleEndian.AppendUint16(buf, uint16(insn.Offset))
		buf = binary.LittleEndian.AppendUint32(buf, uint32(imm))
	}
	sum := sha1.Sum(buf)
	var tag [linux.BPF_TAG_SIZE]byte
	copy(tag[:], sum[:])
	return tag
}

// ldImm64 is the opcode of BPF_LD | BPF_IMM | BPF_DW, whose immediate is
// 64 bits, in two instructions.
const ldImm64 = 0x18

// MapFDs returns the file descriptors of the maps that insns load, as
// immediates of BPF_LD_IMM64 instructions, as Linux's
// resolve_pseudo_ldimm64 finds them. It fails with EINVAL if a BPF_LD_IMM64
// instruction lacks its second half.
func MapFDs(insns []linux.EBPFInstruction) ([]int32, bool) {
	var fds []int32
	for i := 0; i < len(insns); i++ {
		insn := insns[i]
		if insn.Code != ldImm64 {
			continue
		}
		if i+1 == len(insns) {
			return nil, false
		}
		next := insns[i+1]
		if next.Code != 0 || next.Registers != 0 || next.Offset != 0 {
			return nil, false
		}
		if src := insn.Registers >> 4; src == linux.BPF_PSEUDO_MAP_FD || src == linux.BPF_PSEUDO_MAP_VALUE {
			fds = append(fds, insn.Immediate)
		}
		i++
	}
	return fds, true
}

// IncRef takes a reference on p.
func (p *Program) IncRef() {
	p.refs.Add(1)
}

// DecRef drops a reference on p, and releases it if it was the last.
func (p *Program) DecRef() {
	if p.refs.Add(-1) != 0 {
		return
	}
	for _, m := range p.maps {
		m.DecRef()
	}
	p.maps = nil
	p.registry.uncharge(p.cost)
}

// ID returns the unique identifier for the eBPF program.
func (p *Program) ID() BPFID {
	return p.id
}

// ProgType returns the type of the eBPF program.
func (p *Program) ProgType() linux.BPFProgramType {
	return p.progType
}

// ExpectedAttachType returns the attachment type that the program expects.
func (p *Program) ExpectedAttachType() linux.BPFAttachType {
	return p.expectedAttachType
}

// Name returns the program's name.
func (p *Program) Name() string {
	return p.name
}

// Tag returns the program's tag.
func (p *Program) Tag() [linux.BPF_TAG_SIZE]byte {
	return p.tag
}

// Instructions returns the program's instructions, which the caller must
// not modify.
func (p *Program) Instructions() []linux.EBPFInstruction {
	return p.instructions
}

// GPLCompatible returns whether the program's license is compatible with the
// GPL.
func (p *Program) GPLCompatible() bool {
	return p.gplCompatible
}

// UID returns the KUID of the program's creator.
func (p *Program) UID() uint32 {
	return p.uid
}

// LoadTime returns when the program was loaded, in nanoseconds since boot.
func (p *Program) LoadTime() int64 {
	return p.loadTime
}

// MapIDs returns the IDs of the maps that the program uses.
func (p *Program) MapIDs() []BPFID {
	ids := make([]BPFID, 0, len(p.maps))
	for _, m := range p.maps {
		ids = append(ids, m.id)
	}
	return ids
}
