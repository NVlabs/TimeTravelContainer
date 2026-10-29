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

package linux

import (
	"bytes"
	"strings"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/ebpf"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/fspath"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/marshal/primitive"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/bpffs"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/ebpffd"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

// bpfCapable returns whether t may use privileged eBPF features, as Linux's
// bpf_capable does.
func bpfCapable(t *kernel.Task) bool {
	creds := t.Credentials()
	return creds.HasRootCapability(linux.CAP_BPF) || creds.HasRootCapability(linux.CAP_SYS_ADMIN)
}

// bpfTokenCapable returns whether t has capability cp for eBPF, as Linux's
// bpf_token_capable does: CAP_SYS_ADMIN implies all.
func bpfTokenCapable(t *kernel.Task, cp linux.Capability) bool {
	creds := t.Credentials()
	return creds.HasRootCapability(cp) || creds.HasRootCapability(linux.CAP_SYS_ADMIN)
}

// bpfObjName returns the name of an object, as Linux's bpf_obj_name_cpy
// validates it: alphanumerics, '_', and '.', NUL-terminated.
func bpfObjName(b []byte) (string, error) {
	i := bytes.IndexByte(b, 0)
	if i < 0 {
		return "", linuxerr.EINVAL
	}
	for _, c := range b[:i] {
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '_' || c == '.') {
			return "", linuxerr.EINVAL
		}
	}
	return string(b[:i]), nil
}

// bpfFileFlags returns the file flags of a map's file, from flags of
// BPF_MAP_CREATE or BPF_OBJ_GET, as Linux's bpf_get_file_flag does.
func bpfFileFlags(flags uint32) (uint32, error) {
	switch {
	case flags&linux.BPF_F_RDONLY != 0 && flags&linux.BPF_F_WRONLY != 0:
		return 0, linuxerr.EINVAL
	case flags&linux.BPF_F_RDONLY != 0:
		return linux.O_RDONLY, nil
	case flags&linux.BPF_F_WRONLY != 0:
		return linux.O_WRONLY, nil
	default:
		return linux.O_RDWR, nil
	}
}

// cgroupBPFAttachCheckAncestors checks that the attachment is not blocked by eBPF flags in parent cgroups.
//
// It is analogous to kernel/bpf/cgroup.c:hierarchy_allows_attach() in Linux.
func cgroupBPFAttachCheckAncestors(ancestors []*kernel.Cgroup2BPF, attachType ebpf.CgroupAttachType) error {
	for _, a := range ancestors {
		var flags uint8
		var numProgs int
		if a != nil {
			flags = a.Slots[attachType].Flags
			numProgs = len(a.Slots[attachType].Progs)
		}

		// Parent must either allow multi...
		if (flags & linux.BPF_F_ALLOW_MULTI) == linux.BPF_F_ALLOW_MULTI {
			return nil
		}
		// or, for the first parent we see with a program, it must allow override.
		if numProgs > 0 {
			if (flags & linux.BPF_F_ALLOW_OVERRIDE) == linux.BPF_F_ALLOW_OVERRIDE {
				return nil
			}
			return linuxerr.EPERM
		}
	}

	return nil
}

// Flags of BPF_PROG_LOAD that Linux accepts: BPF_F_STRICT_ALIGNMENT,
// BPF_F_ANY_ALIGNMENT, BPF_F_TEST_RND_HI32, BPF_F_TEST_STATE_FREQ,
// BPF_F_SLEEPABLE, BPF_F_XDP_HAS_FRAGS, BPF_F_XDP_DEV_BOUND_ONLY, and
// BPF_F_TEST_REG_INVARIANTS. Programs never run, so they have no effect.
const bpfProgLoadFlags = 1<<0 | 1<<1 | 1<<2 | 1<<3 | 1<<4 | 1<<5 | 1<<6 | 1<<7

// bpfLogMask is BPF_LOG_MASK, the valid bits of log_level.
const bpfLogMask = 1 | 2 | 4 | 8

// licenseIsGPLCompatible returns whether license is compatible with the GPL,
// as Linux's license_is_gpl_compatible does.
func licenseIsGPLCompatible(license string) bool {
	switch license {
	case "GPL", "GPL v2", "GPL and additional rights", "Dual BSD/GPL", "Dual MIT/GPL", "Dual MPL/GPL":
		return true
	}
	return false
}

func bpfProgLoad(t *kernel.Task, attr *linux.BPFAttrProgLoad) (uintptr, error) {
	reg := t.Kernel().EBPF()
	bpfCap := bpfCapable(t)
	progType := linux.BPFProgramType(attr.ProgType)

	if attr.ProgFlags&^bpfProgLoadFlags != 0 {
		return 0, linuxerr.EINVAL
	}
	expectedAttachType := ebpf.FixupExpectedAttachType(progType, linux.BPFAttachType(attr.ExpectedAttachType))

	if reg.UnprivilegedDisabled.Load() != 0 && !bpfCap {
		return 0, linuxerr.EPERM
	}
	maxInsns := uint32(linux.BPF_MAXINSNS)
	if bpfCap {
		maxInsns = linux.BPF_COMPLEXITY_LIMIT_INSNS
	}
	if attr.InstructionCount == 0 || attr.InstructionCount > maxInsns {
		// Instruction count out of range
		return 0, linuxerr.E2BIG
	}
	if progType != linux.BPF_PROG_TYPE_SOCKET_FILTER && progType != linux.BPF_PROG_TYPE_CGROUP_SKB && !bpfCap {
		return 0, linuxerr.EPERM
	}
	if ebpf.IsNetAdminProgType(progType) && !bpfTokenCapable(t, linux.CAP_NET_ADMIN) {
		return 0, linuxerr.EPERM
	}
	if ebpf.IsPerfmonProgType(progType) && !bpfTokenCapable(t, linux.CAP_PERFMON) {
		return 0, linuxerr.EPERM
	}

	if attr.AttachBTFID != 0 || attr.AttachFD != 0 || attr.ProgTokenFD != 0 {
		// Programs that attach to BTF types, or to other programs, and
		// tokens, are not supported.
		return 0, linuxerr.EINVAL
	}
	if !ebpf.SupportedProgType(progType) {
		// Unsupported program type
		return 0, linuxerr.EINVAL
	}
	if err := ebpf.CheckExpectedAttachType(progType, expectedAttachType); err != nil {
		return 0, err
	}
	if attr.ProgInterfaceIndex != 0 {
		// There are no devices to offload programs to.
		return 0, linuxerr.EINVAL
	}
	if attr.ProgBTFFD != 0 || attr.FuncInfoCount != 0 || attr.LineInfoCount != 0 || attr.CoreReloCount != 0 || attr.FDArrayCount != 0 || attr.SignatureSize != 0 {
		// BTF, relocations, and signatures are not supported.
		return 0, linuxerr.EINVAL
	}

	// Load the program
	instructionsAddr := hostarch.Addr(attr.Instructions)
	instructions := make([]linux.EBPFInstruction, attr.InstructionCount)
	if _, err := linux.CopyEBPFInstructionSliceIn(t, instructionsAddr, instructions); err != nil {
		return 0, err
	}
	license, err := t.CopyInString(hostarch.Addr(attr.License), 127)
	if err != nil && !linuxerr.Equals(linuxerr.ENAMETOOLONG, err) {
		return 0, err
	}
	name, err := bpfObjName(attr.ProgName[:])
	if err != nil {
		return 0, err
	}

	// Log attributes must be consistent, as bpf_verifier_log_attr_valid
	// checks. There is no verifier, so there is never anything to log.
	if (attr.LogBuf != 0) != (attr.LogSize != 0) || (attr.LogBuf != 0 && attr.LogLevel == 0) ||
		attr.LogLevel&^bpfLogMask != 0 || attr.LogSize > (1<<32-1)>>2 {
		return 0, linuxerr.EINVAL
	}

	// Resolve the maps that the program uses, as the verifier's
	// resolve_pseudo_ldimm64 does.
	mapFDs, ok := ebpf.MapFDs(instructions)
	if !ok {
		return 0, linuxerr.EINVAL
	}
	var maps []*ebpf.Map
	for _, fd := range mapFDs {
		m, err := bpfMapFromFD(t, fd)
		if err != nil {
			return 0, err
		}
		maps = append(maps, m)
	}

	prog, err := reg.NewProgram(ebpf.ProgramOptions{
		Type:               progType,
		ExpectedAttachType: expectedAttachType,
		Name:               name,
		Instructions:       instructions,
		GPLCompatible:      licenseIsGPLCompatible(license),
		UID:                uint32(t.Credentials().EffectiveKUID),
		LoadTime:           t.Kernel().MonotonicClock().Now().Nanoseconds(),
		Maps:               maps,
	})
	if err != nil {
		return 0, err
	}
	defer prog.DecRef()

	// Make a file descriptor referring to the newly-loaded program
	file, err := ebpffd.New(t, t.Kernel().VFS(), uint32(linux.O_RDWR), prog)
	if err != nil {
		return 0, err
	}
	defer file.DecRef(t)

	fd, err := t.NewFDFrom(0, file, kernel.FDFlags{CloseOnExec: true})
	if err != nil {
		return 0, err
	}

	return uintptr(fd), nil
}

// bpfMapFromFD returns the map of file descriptor fd, without a reference:
// the program being loaded takes one.
func bpfMapFromFD(t *kernel.Task, fd int32) (*ebpf.Map, error) {
	file := t.GetFile(fd)
	if file == nil {
		return nil, linuxerr.EBADF
	}
	defer file.DecRef(t)
	mfd, ok := file.Impl().(*ebpffd.MapFD)
	if !ok {
		return nil, linuxerr.EINVAL
	}
	return mfd.Map(), nil
}

func bpfProgQuery(t *kernel.Task, attr *linux.BPFAttrProgQuery, attrAddr hostarch.Addr, size int) (uintptr, error) {
	// BPF_PROG_QUERY requires either CAP_NET_ADMIN or CAP_SYS_ADMIN
	if !t.Credentials().HasRootCapability(linux.CAP_NET_ADMIN) && !t.Credentials().HasRootCapability(linux.CAP_SYS_ADMIN) {
		return 0, linuxerr.EPERM
	}

	if attr.QueryFlags != 0 {
		// We currently support no flags.
		return 0, linuxerr.EINVAL
	}

	if attr.ProgAttachFlags != 0 {
		// We currently support no prog attach flags.
		return 0, linuxerr.EINVAL
	}

	attachType := ebpf.ParseAttachmentType(linux.BPFAttachType(attr.AttachType))
	if attachType == nil {
		// Invalid attach type
		return 0, linuxerr.EINVAL
	}

	switch attachType := attachType.(type) {
	case ebpf.CgroupAttachType:
		// cgroup v2 eBPF program: first, fetch the cgroup
		cgroup, err := t.GetCgroup2NodeFromFD(uint64(attr.Target))
		if err != nil {
			return 0, err
		}

		count := attr.Count

		// then, access the filters
		var accessErr error
		var outProgs []*kernel.Cgroup2BPFProgram
		var flags uint32
		var revision uint64
		var progCount uint32
		cgroup.IfBPF(func(bpf *kernel.Cgroup2BPF) {
			slot := &bpf.Slots[attachType]
			flags = uint32(slot.Flags)
			revision = slot.Revision
			progCount = uint32(len(slot.Progs))

			if count > progCount {
				// User requested too many programs: clamp
				count = progCount
			} else if count < progCount && (count != 0 && attr.ProgIDs != 0) {
				// User requested too few programs: indicate that there were more
				// (but we will still copy out what we can)
				accessErr = linuxerr.ENOSPC
			}

			outProgs = append(outProgs, slot.Progs[:count]...)
		})

		// Update the attach flags, count, and revision for userspace
		attr.AttachFlags = flags
		attr.Count = progCount
		attr.Revision = revision + 1
		if _, err := attr.CopyOutN(t, attrAddr, int(size)); err != nil {
			return 0, err
		}

		if attr.ProgIDs != 0 {
			// If requested, copy out the program IDs
			progIDsAddr := hostarch.Addr(attr.ProgIDs)
			for i, prog := range outProgs {
				if _, err := primitive.CopyUint32Out(t, progIDsAddr+hostarch.Addr(i*4), uint32(prog.Prog.ID())); err != nil {
					return 0, err
				}
			}
		}

		return 0, accessErr
	default:
		// Unsupported attach type
		return 0, linuxerr.EINVAL
	}
}

// bpfProgFromFD returns the program of file descriptor fd, without a
// reference.
func bpfProgFromFD(t *kernel.Task, fd int32) (*ebpf.Program, error) {
	file := t.GetFile(fd)
	if file == nil {
		return nil, linuxerr.EBADF
	}
	defer file.DecRef(t)
	pfd, ok := file.Impl().(*ebpffd.ProgramFD)
	if !ok {
		// Not an eBPF program fd
		return nil, linuxerr.EINVAL
	}
	return pfd.Program(), nil
}

func bpfProgAttach(t *kernel.Task, attr *linux.BPFAttrProgAttach) (uintptr, error) {
	// Note the lack of a capability check. A caller needs privilege to open a
	// BPF program fd, but attaching one does not require privilege.

	flags := attr.AttachFlags
	slotFlags := uint8(flags & (linux.BPF_F_ALLOW_OVERRIDE | linux.BPF_F_ALLOW_MULTI))

	attachType := ebpf.ParseAttachmentType(linux.BPFAttachType(attr.AttachType))
	if attachType == nil {
		// Invalid attach type
		return 0, linuxerr.EINVAL
	}

	if attr.ExpectedRevision != 0 {
		// ExpectedRevision currently unsupported
		return 0, linuxerr.EINVAL
	}

	if attr.Relative != 0 {
		// relative_fd and relative_id are currently unsupported
		return 0, linuxerr.EINVAL
	}

	// Per-attachment type attach flags validation
	switch attachType.(type) {
	case ebpf.CgroupAttachType:
		if flags&^(linux.BPF_F_ALLOW_MULTI|linux.BPF_F_ALLOW_OVERRIDE) != 0 {
			// Only BPF_F_ALLOW_MULTI and BPF_F_ALLOW_OVERRIDE are supported.
			return 0, linuxerr.EINVAL
		}
		if flags&linux.BPF_F_ALLOW_MULTI != 0 && flags&linux.BPF_F_ALLOW_OVERRIDE != 0 {
			// BPF_F_ALLW_MULTI and BPF_F_ALLOW_OVERRIDE are mutually exclusive.
			return 0, linuxerr.EINVAL
		}
	default:
		// Unsupported attach type
		return 0, linuxerr.EINVAL
	}

	// Fetch the eBPF program from the specified file descriptor
	prog, err := bpfProgFromFD(t, int32(attr.AttachBPFFD))
	if err != nil {
		return 0, err
	}

	matchingProgType := attachType.MatchingProgType()
	if prog.ProgType() != matchingProgType {
		// Program type not valid for this attachment type
		return 0, linuxerr.EINVAL
	}
	// As Linux's bpf_prog_attach_check_attach_type.
	switch prog.ProgType() {
	case linux.BPF_PROG_TYPE_CGROUP_SOCK, linux.BPF_PROG_TYPE_CGROUP_SOCK_ADDR, linux.BPF_PROG_TYPE_CGROUP_SOCKOPT:
		if prog.ExpectedAttachType() != linux.BPFAttachType(attr.AttachType) {
			return 0, linuxerr.EINVAL
		}
	case linux.BPF_PROG_TYPE_CGROUP_SKB:
		// Unprivileged users may load cgroup skb programs, but not attach
		// them.
		if !bpfTokenCapable(t, linux.CAP_NET_ADMIN) {
			return 0, linuxerr.EPERM
		}
	}

	switch attachType := attachType.(type) {
	case ebpf.CgroupAttachType:
		// cgroup v2 eBPF program: first, fetch the referenced cgroup
		cgroup, err := t.GetCgroup2NodeFromFD(uint64(attr.Target))
		if err != nil {
			return 0, err
		}

		// Take the cgroup tree lock
		err = cgroup.WriteBPF(func(bpf *kernel.Cgroup2BPF, ancestors []*kernel.Cgroup2BPF) error {
			// check if the cgroup's parent(s) allow the attach
			if err := cgroupBPFAttachCheckAncestors(ancestors, attachType); err != nil {
				return err
			}

			slot := &bpf.Slots[attachType]

			if len(slot.Progs) > 0 && slot.Flags != slotFlags {
				// Slot-level flags don't match what userspace requested
				return linuxerr.EPERM
			}

			if len(slot.Progs) >= linux.BPF_CGROUP_MAX_PROGS {
				// Slot is already full
				return linuxerr.E2BIG
			}

			// Disallow attaching the same program twice at the same attachment slot
			if flags&linux.BPF_F_ALLOW_MULTI != 0 {
				for _, existingProg := range slot.Progs {
					if prog == existingProg.Prog {
						return linuxerr.EINVAL
					}
				}
			}

			newProg := &kernel.Cgroup2BPFProgram{
				Prog:  prog,
				Flags: flags,
			}
			prog.IncRef()
			if flags&linux.BPF_F_ALLOW_MULTI == 0 && len(slot.Progs) != 0 {
				// Without BPF_F_ALLOW_MULTI, a slot holds one program,
				// which a new one replaces, as Linux's __cgroup_bpf_attach
				// does.
				slot.Progs[0].Prog.DecRef()
				slot.Progs[0] = newProg
			} else {
				slot.Progs = append(slot.Progs, newProg)
			}
			slot.Flags = slotFlags
			slot.Revision++

			return nil
		})
		return 0, err

	default:
		// Unsupported attachment type
		return 0, linuxerr.EINVAL

	}
}

// bpfProgDetach implements BPF_PROG_DETACH, as Linux's bpf_prog_detach and
// __cgroup_bpf_detach do.
func bpfProgDetach(t *kernel.Task, attr *linux.BPFAttrProgAttach) (uintptr, error) {
	attachType, ok := ebpf.ParseAttachmentType(linux.BPFAttachType(attr.AttachType)).(ebpf.CgroupAttachType)
	if !ok {
		return 0, linuxerr.EINVAL
	}
	if attr.AttachFlags != 0 || attr.Relative != 0 {
		return 0, linuxerr.EINVAL
	}
	cgroup, err := t.GetCgroup2NodeFromFD(uint64(attr.Target))
	if err != nil {
		return 0, err
	}
	// The program is optional, unless the slot holds several.
	prog, err := bpfProgFromFD(t, int32(attr.AttachBPFFD))
	if err != nil || prog.ProgType() != attachType.MatchingProgType() {
		prog = nil
	}
	return 0, cgroup.WriteBPF(func(bpf *kernel.Cgroup2BPF, _ []*kernel.Cgroup2BPF) error {
		slot := &bpf.Slots[attachType]
		i := -1
		if slot.Flags&linux.BPF_F_ALLOW_MULTI == 0 {
			if len(slot.Progs) == 0 {
				return linuxerr.ENOENT
			}
			i = 0
		} else {
			if prog == nil {
				return linuxerr.EINVAL
			}
			for j, p := range slot.Progs {
				if p.Prog == prog {
					i = j
					break
				}
			}
			if i < 0 {
				return linuxerr.ENOENT
			}
		}
		slot.Progs[i].Prog.DecRef()
		slot.Progs = append(slot.Progs[:i], slot.Progs[i+1:]...)
		slot.Revision++
		if len(slot.Progs) == 0 {
			slot.Flags = 0
		}
		return nil
	})
}

// bpfMapCreate implements BPF_MAP_CREATE, as Linux's map_create does.
func bpfMapCreate(t *kernel.Task, attr *linux.BPFAttrMapCreate) (uintptr, error) {
	reg := t.Kernel().EBPF()
	mapType := linux.BPFMapType(attr.MapType)
	if attr.MapFlags&(linux.BPF_F_TOKEN_FD|linux.BPF_F_VTYPE_BTF_OBJ_FD) != 0 || attr.MapTokenFD != 0 || attr.ValueTypeBTFObjFD != 0 {
		// Tokens, and BTF, are not supported.
		return 0, linuxerr.EINVAL
	}
	if attr.BTFKeyTypeID != 0 || attr.BTFValueTypeID != 0 || attr.BTFVmlinuxValueTypeID != 0 {
		// There is no BTF object that BTFFD could refer to.
		if file := t.GetFile(int32(attr.BTFFD)); file != nil {
			file.DecRef(t)
			return 0, linuxerr.EINVAL
		}
		return 0, linuxerr.EBADF
	}
	if attr.MapExtra != 0 || attr.MapIfindex != 0 {
		return 0, linuxerr.EINVAL
	}
	if attr.MapFlags&linux.BPF_F_NUMA_NODE != 0 && attr.NumaNode != 0 {
		// The sentry has one NUMA node.
		return 0, linuxerr.EINVAL
	}
	name, err := bpfObjName(attr.MapName[:])
	if err != nil {
		return 0, err
	}
	fileFlags, err := bpfFileFlags(attr.MapFlags)
	if err != nil {
		return 0, err
	}

	// Unprivileged users may create some types of maps, unless eBPF is
	// disabled for them.
	bpfCap := bpfCapable(t)
	if reg.UnprivilegedDisabled.Load() != 0 && !bpfCap {
		return 0, linuxerr.EPERM
	}
	switch mapType {
	case linux.BPF_MAP_TYPE_LRU_HASH, linux.BPF_MAP_TYPE_LRU_PERCPU_HASH:
		if !bpfCap {
			return 0, linuxerr.EPERM
		}
	}

	m, err := reg.NewMap(ebpf.MapOptions{
		Type:       mapType,
		KeySize:    attr.KeySize,
		ValueSize:  attr.ValueSize,
		MaxEntries: attr.MaxEntries,
		Flags:      attr.MapFlags &^ linux.BPF_F_NUMA_NODE,
		Name:       name,
		NumCPUs:    uint32(t.Kernel().ApplicationCores()),
	})
	if err != nil {
		return 0, err
	}
	defer m.DecRef()
	file, err := ebpffd.NewMap(t, t.Kernel().VFS(), fileFlags, m)
	if err != nil {
		return 0, err
	}
	defer file.DecRef(t)
	fd, err := t.NewFDFrom(0, file, kernel.FDFlags{CloseOnExec: true})
	if err != nil {
		return 0, err
	}
	return uintptr(fd), nil
}

// bpfCopyInBytes copies in n bytes at addr.
func bpfCopyInBytes(t *kernel.Task, addr uint64, n uint32) ([]byte, error) {
	b := make([]byte, n)
	if _, err := t.CopyInBytes(hostarch.Addr(addr), b); err != nil {
		return nil, err
	}
	return b, nil
}

// bpfMapElem implements the BPF_MAP_*_ELEM commands, and BPF_MAP_FREEZE, as
// Linux's map_lookup_elem, map_update_elem, map_delete_elem,
// map_get_next_key, map_lookup_and_delete_elem, and map_freeze do.
func bpfMapElem(t *kernel.Task, cmd int32, attr *linux.BPFAttrMapElem) error {
	// The fields past each command's last must be zero.
	switch cmd {
	case linux.BPF_MAP_DELETE_ELEM:
		if attr.Value != 0 || attr.Flags != 0 {
			return linuxerr.EINVAL
		}
	case linux.BPF_MAP_GET_NEXT_KEY:
		if attr.Flags != 0 {
			return linuxerr.EINVAL
		}
	case linux.BPF_MAP_FREEZE:
		if attr.Key != 0 || attr.Value != 0 || attr.Flags != 0 {
			return linuxerr.EINVAL
		}
	case linux.BPF_MAP_LOOKUP_ELEM, linux.BPF_MAP_LOOKUP_AND_DELETE_ELEM:
		if attr.Flags&^linux.BPF_F_LOCK != 0 {
			return linuxerr.EINVAL
		}
	}

	file := t.GetFile(int32(attr.MapFD))
	if file == nil {
		return linuxerr.EBADF
	}
	defer file.DecRef(t)
	mfd, ok := file.Impl().(*ebpffd.MapFD)
	if !ok {
		return linuxerr.EINVAL
	}
	m := mfd.Map()
	canRead := file.IsReadable()
	canWrite := file.IsWritable() && !m.Frozen()

	// No map has a spin lock, which BPF_F_LOCK needs.
	lock := attr.Flags&linux.BPF_F_LOCK != 0 && cmd != linux.BPF_MAP_GET_NEXT_KEY

	switch cmd {
	case linux.BPF_MAP_LOOKUP_ELEM:
		if !canRead {
			return linuxerr.EPERM
		}
		if lock {
			return linuxerr.EINVAL
		}
		key, err := bpfCopyInBytes(t, attr.Key, m.KeySize())
		if err != nil {
			return err
		}
		value, err := m.Lookup(key)
		if err != nil {
			return err
		}
		_, err = t.CopyOutBytes(hostarch.Addr(attr.Value), value)
		return err

	case linux.BPF_MAP_UPDATE_ELEM:
		if !canWrite {
			return linuxerr.EPERM
		}
		if lock {
			return linuxerr.EINVAL
		}
		key, err := bpfCopyInBytes(t, attr.Key, m.KeySize())
		if err != nil {
			return err
		}
		value, err := bpfCopyInBytes(t, attr.Value, m.UserValueSize())
		if err != nil {
			return err
		}
		return m.Update(key, value, attr.Flags)

	case linux.BPF_MAP_DELETE_ELEM:
		if !canWrite {
			return linuxerr.EPERM
		}
		key, err := bpfCopyInBytes(t, attr.Key, m.KeySize())
		if err != nil {
			return err
		}
		return m.Delete(key)

	case linux.BPF_MAP_GET_NEXT_KEY:
		if !canRead {
			return linuxerr.EPERM
		}
		var key []byte
		if attr.Key != 0 {
			var err error
			if key, err = bpfCopyInBytes(t, attr.Key, m.KeySize()); err != nil {
				return err
			}
		}
		next, err := m.NextKey(key)
		if err != nil {
			return err
		}
		_, err = t.CopyOutBytes(hostarch.Addr(attr.Value), next)
		return err

	case linux.BPF_MAP_LOOKUP_AND_DELETE_ELEM:
		if !canRead || !canWrite {
			return linuxerr.EPERM
		}
		if lock {
			return linuxerr.EINVAL
		}
		key, err := bpfCopyInBytes(t, attr.Key, m.KeySize())
		if err != nil {
			return err
		}
		// Check that the value can be copied out before deleting.
		if _, err := t.CopyOutBytes(hostarch.Addr(attr.Value), make([]byte, m.UserValueSize())); err != nil {
			return err
		}
		value, err := m.LookupAndDelete(key)
		if err != nil {
			return err
		}
		_, err = t.CopyOutBytes(hostarch.Addr(attr.Value), value)
		return err

	case linux.BPF_MAP_FREEZE:
		if !canWrite {
			return linuxerr.EPERM
		}
		return m.Freeze()
	}
	return linuxerr.EINVAL
}

// bpfPinObject returns the object of file descriptor fd, with a reference.
func bpfPinObject(t *kernel.Task, fd int32) (bpffs.Object, error) {
	file := t.GetFile(fd)
	if file == nil {
		return nil, linuxerr.EBADF
	}
	defer file.DecRef(t)
	switch impl := file.Impl().(type) {
	case *ebpffd.ProgramFD:
		impl.Program().IncRef()
		return impl.Program(), nil
	case *ebpffd.MapFD:
		impl.Map().IncRef()
		return impl.Map(), nil
	default:
		return nil, linuxerr.EINVAL
	}
}

// bpfObjPin implements BPF_OBJ_PIN, as Linux's bpf_obj_pin does.
func bpfObjPin(t *kernel.Task, attr *linux.BPFAttrObj) error {
	if attr.Pad != 0 || attr.FileFlags&^linux.BPF_F_PATH_FD != 0 {
		return linuxerr.EINVAL
	}
	dirfd := int32(linux.AT_FDCWD)
	if attr.FileFlags&linux.BPF_F_PATH_FD != 0 {
		dirfd = attr.PathFD
	} else if attr.PathFD != 0 {
		return linuxerr.EINVAL
	}
	obj, err := bpfPinObject(t, int32(attr.BPFFD))
	if err != nil {
		return err
	}
	defer obj.DecRef()

	pathname, err := t.CopyInString(hostarch.Addr(attr.Pathname), linux.PATH_MAX)
	if err != nil {
		return err
	}
	// Split the path into its directory, which must be a bpf filesystem's,
	// and the name to create in it.
	dir, name := ".", pathname
	if i := strings.LastIndexByte(pathname, '/'); i >= 0 {
		dir, name = pathname[:i+1], pathname[i+1:]
	}
	switch name {
	case "":
		return linuxerr.ENOENT
	case ".", "..":
		return linuxerr.EEXIST
	}
	tpop, err := getTaskPathOperation(t, dirfd, fspath.Parse(dir), disallowEmptyPath, followFinalSymlink, 0)
	if err != nil {
		return err
	}
	defer tpop.Release(t)
	creds := t.Credentials()
	parent, err := t.Kernel().VFS().GetDentryAt(t, creds, &tpop.pop, &vfs.GetDentryOptions{
		CheckSearchable: true,
	})
	if err != nil {
		return err
	}
	defer parent.DecRef(t)
	perm := linux.FileMode(0600) &^ linux.FileMode(t.FSContext().Umask())
	return bpffs.Pin(t, creds, parent, name, perm, obj)
}

// bpfObjGet implements BPF_OBJ_GET, as Linux's bpf_obj_get does.
func bpfObjGet(t *kernel.Task, attr *linux.BPFAttrObj) (uintptr, error) {
	if attr.Pad != 0 || attr.BPFFD != 0 || attr.FileFlags&^(linux.BPF_F_RDONLY|linux.BPF_F_WRONLY|linux.BPF_F_PATH_FD) != 0 {
		return 0, linuxerr.EINVAL
	}
	dirfd := int32(linux.AT_FDCWD)
	if attr.FileFlags&linux.BPF_F_PATH_FD != 0 {
		dirfd = attr.PathFD
	} else if attr.PathFD != 0 {
		return 0, linuxerr.EINVAL
	}
	fileFlags, err := bpfFileFlags(attr.FileFlags)
	if err != nil {
		return 0, err
	}
	path, err := copyInPath(t, hostarch.Addr(attr.Pathname))
	if err != nil {
		return 0, err
	}
	tpop, err := getTaskPathOperation(t, dirfd, path, disallowEmptyPath, followFinalSymlink, 0)
	if err != nil {
		return 0, err
	}
	defer tpop.Release(t)
	creds := t.Credentials()
	vd, err := t.Kernel().VFS().GetDentryAt(t, creds, &tpop.pop, &vfs.GetDentryOptions{})
	if err != nil {
		return 0, err
	}
	defer vd.DecRef(t)
	var ats vfs.AccessTypes
	switch fileFlags {
	case linux.O_RDONLY:
		ats = vfs.MayRead
	case linux.O_WRONLY:
		ats = vfs.MayWrite
	default:
		ats = vfs.MayRead | vfs.MayWrite
	}
	obj, err := bpffs.Get(t, creds, vd, ats)
	if err != nil {
		return 0, err
	}
	defer obj.DecRef()

	var file *vfs.FileDescription
	switch o := obj.(type) {
	case *ebpf.Program:
		file, err = ebpffd.New(t, t.Kernel().VFS(), linux.O_RDWR, o)
	case *ebpf.Map:
		file, err = ebpffd.NewMap(t, t.Kernel().VFS(), fileFlags, o)
	default:
		return 0, linuxerr.ENOENT
	}
	if err != nil {
		return 0, err
	}
	defer file.DecRef(t)
	fd, err := t.NewFDFrom(0, file, kernel.FDFlags{CloseOnExec: true})
	if err != nil {
		return 0, err
	}
	return uintptr(fd), nil
}

// bpfInfoLen returns the length of the info that BPF_OBJ_GET_INFO_BY_FD
// copies, of a struct of size bytes, as Linux's bpf_check_uarg_tail_zero
// checks it: past size, the user's info must be zero.
func bpfInfoLen(t *kernel.Task, addr uint64, size int, infoLen uint32) (int, error) {
	if int(infoLen) <= size {
		return int(infoLen), nil
	}
	if infoLen > hostarch.PageSize {
		return 0, linuxerr.E2BIG
	}
	tail, err := bpfCopyInBytes(t, addr+uint64(size), infoLen-uint32(size))
	if err != nil {
		return 0, err
	}
	for _, b := range tail {
		if b != 0 {
			return 0, linuxerr.E2BIG
		}
	}
	return size, nil
}

// bpfObjGetInfo implements BPF_OBJ_GET_INFO_BY_FD, as Linux's
// bpf_map_get_info_by_fd and bpf_prog_get_info_by_fd do.
func bpfObjGetInfo(t *kernel.Task, attr *linux.BPFAttrObjGetInfo, attrAddr hostarch.Addr) error {
	file := t.GetFile(int32(attr.BPFFD))
	if file == nil {
		return linuxerr.EBADF
	}
	defer file.DecRef(t)

	var buf []byte
	switch impl := file.Impl().(type) {
	case *ebpffd.MapFD:
		var info linux.BPFMapInfo
		n, err := bpfInfoLen(t, attr.Info, info.SizeBytes(), attr.InfoLen)
		if err != nil {
			return err
		}
		m := impl.Map()
		info.Type = uint32(m.Type())
		info.ID = uint32(m.ID())
		info.KeySize = m.KeySize()
		info.ValueSize = m.ValueSize()
		info.MaxEntries = m.MaxEntries()
		info.MapFlags = m.Flags()
		copy(info.Name[:], m.Name())
		buf = make([]byte, info.SizeBytes())
		info.MarshalBytes(buf)
		buf = buf[:n]

	case *ebpffd.ProgramFD:
		var info linux.BPFProgInfo
		n, err := bpfInfoLen(t, attr.Info, info.SizeBytes(), attr.InfoLen)
		if err != nil {
			return err
		}
		// The user's info holds the buffers to fill.
		user := make([]byte, info.SizeBytes())
		if _, err := t.CopyInBytes(hostarch.Addr(attr.Info), user[:n]); err != nil {
			return err
		}
		info.UnmarshalBytes(user)
		if err := bpfProgInfo(t, impl.Program(), &info); err != nil {
			return err
		}
		buf = make([]byte, info.SizeBytes())
		info.MarshalBytes(buf)
		buf = buf[:n]

	default:
		return linuxerr.EINVAL
	}

	if _, err := t.CopyOutBytes(hostarch.Addr(attr.Info), buf); err != nil {
		return err
	}
	attr.InfoLen = uint32(len(buf))
	_, err := primitive.CopyUint32Out(t, attrAddr+4, attr.InfoLen)
	return err
}

// bpfProgInfo fills info, whose buffers the user gave, with p's.
func bpfProgInfo(t *kernel.Task, p *ebpf.Program, info *linux.BPFProgInfo) error {
	info.Type = uint32(p.ProgType())
	info.ID = uint32(p.ID())
	info.Tag = p.Tag()
	info.LoadTime = uint64(p.LoadTime())
	info.CreatedByUID = uint32(auth.KUID(p.UID()).In(t.UserNamespace()).OrOverflow())
	info.GPLCompatible = 0
	if p.GPLCompatible() {
		info.GPLCompatible = 1
	}
	info.Name = [linux.BPF_OBJ_NAME_LEN]byte{}
	copy(info.Name[:], p.Name())
	info.Ifindex = 0
	info.NetnsDev = 0
	info.NetnsIno = 0
	info.BTFID = 0
	info.AttachBTFID = 0
	info.AttachBTFObjID = 0
	info.RunTimeNs = 0
	info.RunCnt = 0
	info.RecursionMisses = 0
	info.VerifiedInsns = uint32(len(p.Instructions()))

	mapIDs := p.MapIDs()
	ulen := min(info.NrMapIDs, uint32(len(mapIDs)))
	info.NrMapIDs = uint32(len(mapIDs))
	for i := uint32(0); i < ulen; i++ {
		if _, err := primitive.CopyUint32Out(t, hostarch.Addr(info.MapIDs)+hostarch.Addr(4*i), uint32(mapIDs[i])); err != nil {
			return err
		}
	}

	// Programs are never JIT-compiled, nor have BTF.
	info.JitedProgLen = 0
	info.NrJitedKsyms = 0
	info.NrJitedFuncLens = 0
	info.NrFuncInfo = 0
	info.NrLineInfo = 0
	info.NrJitedLineInfo = 0
	info.FuncInfoRecSize = 0
	info.LineInfoRecSize = 0
	info.JitedLineInfoRecSize = 0

	if !bpfCapable(t) {
		info.XlatedProgLen = 0
		return nil
	}
	insns := p.Instructions()
	ulen = info.XlatedProgLen
	info.XlatedProgLen = uint32(len(insns) * linux.BPF_INSTRUCTION_SIZE)
	if ulen != 0 && info.XlatedProgLen != 0 {
		n := min(ulen, info.XlatedProgLen) / linux.BPF_INSTRUCTION_SIZE
		if _, err := linux.CopyEBPFInstructionSliceOut(t, hostarch.Addr(info.XlatedProgInsns), insns[:n]); err != nil {
			return err
		}
	}
	ulen = info.NrProgTags
	info.NrProgTags = 1
	if ulen != 0 {
		tag := p.Tag()
		if _, err := t.CopyOutBytes(hostarch.Addr(info.ProgTags), tag[:]); err != nil {
			return err
		}
	}
	return nil
}

// Bpf implements Linux syscall bpf(2).
func Bpf(t *kernel.Task, sysno uintptr, args arch.SyscallArguments) (uintptr, *kernel.SyscallControl, error) {
	cmd := args[0].Int()
	attrAddr := args[1].Pointer()
	size := args[2].Uint()

	// Copy in BPF attr struct from userspace
	attr, err := copyInBPFAttr(t, cmd, attrAddr, int(size))
	if err != nil {
		return 0, nil, err
	}

	switch attr := attr.(type) {
	case *linux.BPFAttrProgLoad:
		fd, err := bpfProgLoad(t, attr)
		return fd, nil, err

	case *linux.BPFAttrProgQuery:
		size := min(int(size), attr.SizeBytes())
		ret, err := bpfProgQuery(t, attr, attrAddr, size)
		return ret, nil, err

	case *linux.BPFAttrProgAttach:
		if cmd == linux.BPF_PROG_DETACH {
			ret, err := bpfProgDetach(t, attr)
			return ret, nil, err
		}
		ret, err := bpfProgAttach(t, attr)
		return ret, nil, err

	case *linux.BPFAttrMapCreate:
		fd, err := bpfMapCreate(t, attr)
		return fd, nil, err

	case *linux.BPFAttrMapElem:
		return 0, nil, bpfMapElem(t, cmd, attr)

	case *linux.BPFAttrObj:
		if cmd == linux.BPF_OBJ_PIN {
			return 0, nil, bpfObjPin(t, attr)
		}
		fd, err := bpfObjGet(t, attr)
		return fd, nil, err

	case *linux.BPFAttrObjGetInfo:
		return 0, nil, bpfObjGetInfo(t, attr, attrAddr)

	default:
		// Unsupported command.
		if !t.HasRootCapability(linux.CAP_SYS_ADMIN) {
			return 0, nil, linuxerr.EPERM
		}
		return 0, nil, linuxerr.EINVAL
	}
}

// copyInBPFAttr fetches and returns a BPFAttr of the type specified by cmd from userspace.
//
// If cmd is invalid or unsupported, the nil BPFAttr is returned (along with no error).
func copyInBPFAttr(t *kernel.Task, cmd int32, addr hostarch.Addr, size int) (linux.BPFAttr, error) {
	// Invalid size. Negative size here would indicate overflow
	if size < 0 || size > int(hostarch.PageSize) {
		return nil, linuxerr.E2BIG
	}

	var bpfAttr linux.BPFAttr
	switch cmd {
	case linux.BPF_PROG_LOAD:
		bpfAttr = &linux.BPFAttrProgLoad{}
	case linux.BPF_PROG_QUERY:
		bpfAttr = &linux.BPFAttrProgQuery{}
	case linux.BPF_PROG_ATTACH, linux.BPF_PROG_DETACH:
		bpfAttr = &linux.BPFAttrProgAttach{}
	case linux.BPF_MAP_CREATE:
		bpfAttr = &linux.BPFAttrMapCreate{}
	case linux.BPF_MAP_LOOKUP_ELEM, linux.BPF_MAP_UPDATE_ELEM, linux.BPF_MAP_DELETE_ELEM,
		linux.BPF_MAP_GET_NEXT_KEY, linux.BPF_MAP_LOOKUP_AND_DELETE_ELEM, linux.BPF_MAP_FREEZE:
		bpfAttr = &linux.BPFAttrMapElem{}
	case linux.BPF_OBJ_PIN, linux.BPF_OBJ_GET:
		bpfAttr = &linux.BPFAttrObj{}
	case linux.BPF_OBJ_GET_INFO_BY_FD:
		bpfAttr = &linux.BPFAttrObjGetInfo{}
	default:
		return nil, nil
	}

	// Copy in the valid fields of the BPFAttr structure
	copySize := min(int(size), bpfAttr.SizeBytes())
	if _, err := bpfAttr.CopyInN(t, addr, copySize); err != nil {
		return nil, err
	}

	if size > linux.BPF_ATTR_SIZE {
		// Userspace has a newer union version. Check that the fields we don't
		// know about are zeroed out.
		buf := make([]byte, size-linux.BPF_ATTR_SIZE)
		if _, err := t.CopyInBytes(addr+hostarch.Addr(linux.BPF_ATTR_SIZE), buf); err != nil {
			return nil, err
		}

		for _, b := range buf {
			if b != 0 {
				return nil, linuxerr.E2BIG
			}
		}
	}

	if size > bpfAttr.SizeBytes() {
		// Check that the fields after this union *variant* (but still within
		// the overall union) are zeroed out.
		buf := make([]byte, min(size, linux.BPF_ATTR_SIZE)-bpfAttr.SizeBytes())
		if _, err := t.CopyInBytes(addr+hostarch.Addr(bpfAttr.SizeBytes()), buf); err != nil {
			return nil, err
		}

		for _, b := range buf {
			if b != 0 {
				return nil, linuxerr.EINVAL
			}
		}
	}

	return bpfAttr, nil
}
