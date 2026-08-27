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

// Package monitorabi defines the versioned shared-memory protocol between the
// Go VM platform and the freestanding EL1 MMU monitor.
package monitorabi

import (
	"encoding/binary"
	"fmt"
)

const (
	// Magic identifies an MMU monitor command page ("TTCMMU01").
	Magic uint64 = 0x3130554d4d435454
	// Version is the shared-memory ABI version.
	Version uint16 = 1
	// LogicalPageSize is the Linux stage-1 translation granule.
	LogicalPageSize uint64 = 4096
	// MaxOperations bounds validation and monitor execution time per command.
	MaxOperations = 256

	HeaderSize    = 64
	OperationSize = 40
	ResponseSize  = 32
)

// Address-space creation flags describe the privilege domain whose stage-1
// mappings the monitor owns. Application address spaces are accessible from
// EL0. A Sentry address space is EL1-only and is used by ring0 execution
// engines to preserve the Sentry continuation across application syscalls.
const (
	AddressSpaceFlagSentry uint64 = 1 << iota
	addressSpaceFlagsMask         = AddressSpaceFlagSentry
)

// Opcode is a monitor transaction type.
type Opcode uint16

const (
	OpcodeInvalid Opcode = iota
	OpcodeCreateAddressSpace
	OpcodeDestroyAddressSpace
	OpcodeApply
	OpcodeActivate
	OpcodeExpandPageTablePool
)

// OperationType is one stage-1 page-table mutation.
type OperationType uint8

const (
	OperationInvalid OperationType = iota
	OperationMap
	OperationUnmap
	OperationProtect
)

// Access is the guest stage-1 permission mask.
type Access uint8

const (
	AccessRead Access = 1 << iota
	AccessWrite
	AccessExecute
)

// Command is one globally serialized monitor transaction.
type Command struct {
	Sequence       uint64
	Opcode         Opcode
	AddressSpaceID uint64
	Flags          uint64
	PoolGPA        uint64
	PoolLength     uint64
	Operations     []Operation
}

// Operation describes a page-aligned virtual range. GPA is required only for
// OperationMap. Replacements and permission reductions are performed by the
// monitor using architected break-before-make and synchronous TLB invalidation.
type Operation struct {
	Type   OperationType
	Access Access
	VA     uint64
	GPA    uint64
	Length uint64
}

// Status is a monitor response code. StatusOK means that all required
// DSB/TLBI/DSB/ISB sequences have completed.
type Status uint32

const (
	StatusOK Status = iota
	StatusMalformed
	StatusUnsupported
	StatusNoMemory
	StatusNotFound
	StatusConflict
	StatusInternal
)

// Response acknowledges a command sequence.
type Response struct {
	Sequence uint64
	Status   Status
	Detail   uint32
}

// MarshalCommand encodes c using fixed-width little-endian fields.
func MarshalCommand(c Command) ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	b := make([]byte, HeaderSize+len(c.Operations)*OperationSize)
	binary.LittleEndian.PutUint64(b[0:8], Magic)
	binary.LittleEndian.PutUint16(b[8:10], Version)
	binary.LittleEndian.PutUint16(b[10:12], uint16(c.Opcode))
	binary.LittleEndian.PutUint32(b[12:16], uint32(len(c.Operations)))
	binary.LittleEndian.PutUint64(b[16:24], c.Sequence)
	binary.LittleEndian.PutUint64(b[24:32], c.AddressSpaceID)
	binary.LittleEndian.PutUint64(b[32:40], c.Flags)
	binary.LittleEndian.PutUint64(b[40:48], c.PoolGPA)
	binary.LittleEndian.PutUint64(b[48:56], c.PoolLength)
	for i, op := range c.Operations {
		off := HeaderSize + i*OperationSize
		b[off] = byte(op.Type)
		b[off+1] = byte(op.Access)
		binary.LittleEndian.PutUint64(b[off+8:off+16], op.VA)
		binary.LittleEndian.PutUint64(b[off+16:off+24], op.GPA)
		binary.LittleEndian.PutUint64(b[off+24:off+32], op.Length)
	}
	return b, nil
}

// UnmarshalCommand decodes and validates a command.
func UnmarshalCommand(b []byte) (Command, error) {
	if len(b) < HeaderSize {
		return Command{}, fmt.Errorf("monitor command is %d bytes, want at least %d", len(b), HeaderSize)
	}
	if magic := binary.LittleEndian.Uint64(b[0:8]); magic != Magic {
		return Command{}, fmt.Errorf("invalid monitor magic %#x", magic)
	}
	if version := binary.LittleEndian.Uint16(b[8:10]); version != Version {
		return Command{}, fmt.Errorf("unsupported monitor ABI version %d", version)
	}
	if !allZero(b[56:HeaderSize]) {
		return Command{}, fmt.Errorf("monitor command has nonzero reserved header bytes")
	}
	count := int(binary.LittleEndian.Uint32(b[12:16]))
	if count > MaxOperations {
		return Command{}, fmt.Errorf("monitor operation count %d exceeds maximum %d", count, MaxOperations)
	}
	want := HeaderSize + count*OperationSize
	if len(b) != want {
		return Command{}, fmt.Errorf("monitor command is %d bytes, header requires %d", len(b), want)
	}
	c := Command{
		Opcode:         Opcode(binary.LittleEndian.Uint16(b[10:12])),
		Sequence:       binary.LittleEndian.Uint64(b[16:24]),
		AddressSpaceID: binary.LittleEndian.Uint64(b[24:32]),
		Flags:          binary.LittleEndian.Uint64(b[32:40]),
		PoolGPA:        binary.LittleEndian.Uint64(b[40:48]),
		PoolLength:     binary.LittleEndian.Uint64(b[48:56]),
		Operations:     make([]Operation, count),
	}
	for i := range c.Operations {
		off := HeaderSize + i*OperationSize
		if !allZero(b[off+2:off+8]) || !allZero(b[off+32:off+OperationSize]) {
			return Command{}, fmt.Errorf("monitor operation %d has nonzero reserved bytes", i)
		}
		c.Operations[i] = Operation{
			Type:   OperationType(b[off]),
			Access: Access(b[off+1]),
			VA:     binary.LittleEndian.Uint64(b[off+8 : off+16]),
			GPA:    binary.LittleEndian.Uint64(b[off+16 : off+24]),
			Length: binary.LittleEndian.Uint64(b[off+24 : off+32]),
		}
	}
	if err := c.Validate(); err != nil {
		return Command{}, err
	}
	return c, nil
}

// Validate rejects malformed input before either side mutates page tables.
func (c Command) Validate() error {
	if c.Sequence == 0 {
		return fmt.Errorf("monitor sequence is zero")
	}
	if len(c.Operations) > MaxOperations {
		return fmt.Errorf("monitor operation count %d exceeds maximum %d", len(c.Operations), MaxOperations)
	}
	switch c.Opcode {
	case OpcodeCreateAddressSpace:
		if c.AddressSpaceID == 0 || c.Flags & ^addressSpaceFlagsMask != 0 || c.PoolGPA != 0 || c.PoolLength != 0 || len(c.Operations) != 0 {
			return fmt.Errorf("invalid create-address-space command")
		}
	case OpcodeDestroyAddressSpace, OpcodeActivate:
		if c.AddressSpaceID == 0 || c.Flags != 0 || c.PoolGPA != 0 || c.PoolLength != 0 || len(c.Operations) != 0 {
			return fmt.Errorf("invalid address-space command")
		}
	case OpcodeApply:
		if c.AddressSpaceID == 0 || c.Flags != 0 || c.PoolGPA != 0 || c.PoolLength != 0 || len(c.Operations) == 0 {
			return fmt.Errorf("invalid apply command")
		}
	case OpcodeExpandPageTablePool:
		if c.AddressSpaceID != 0 || c.Flags != 0 || len(c.Operations) != 0 ||
			c.PoolGPA%LogicalPageSize != 0 || c.PoolLength == 0 || c.PoolLength%LogicalPageSize != 0 ||
			c.PoolGPA > ^uint64(0)-c.PoolLength {
			return fmt.Errorf("invalid page-table-pool expansion command")
		}
	default:
		return fmt.Errorf("unsupported monitor opcode %d", c.Opcode)
	}
	totalPages := uint64(0)
	for i, op := range c.Operations {
		if err := op.Validate(); err != nil {
			return fmt.Errorf("monitor operation %d: %w", i, err)
		}
		totalPages += op.Length / LogicalPageSize
		if totalPages > MaxOperations {
			return fmt.Errorf("monitor command mutates %d pages, maximum is %d", totalPages, MaxOperations)
		}
	}
	return nil
}

// Validate validates an individual page-table mutation.
func (op Operation) Validate() error {
	if op.VA%LogicalPageSize != 0 || op.Length == 0 || op.Length%LogicalPageSize != 0 {
		return fmt.Errorf("unaligned virtual range [%#x, %#x)", op.VA, op.VA+op.Length)
	}
	if op.VA > ^uint64(0)-op.Length {
		return fmt.Errorf("virtual range overflows")
	}
	if op.Access & ^(AccessRead|AccessWrite|AccessExecute) != 0 {
		return fmt.Errorf("invalid access mask %#x", op.Access)
	}
	switch op.Type {
	case OperationMap:
		if op.GPA%LogicalPageSize != 0 || op.Access == 0 {
			return fmt.Errorf("invalid map GPA or permissions")
		}
		if op.GPA > ^uint64(0)-op.Length {
			return fmt.Errorf("physical range overflows")
		}
	case OperationUnmap:
		if op.GPA != 0 || op.Access != 0 {
			return fmt.Errorf("unmap includes GPA or permissions")
		}
	case OperationProtect:
		if op.GPA != 0 || op.Access == 0 {
			return fmt.Errorf("invalid protect operation")
		}
	default:
		return fmt.Errorf("unsupported operation type %d", op.Type)
	}
	return nil
}

// MarshalResponse encodes r.
func MarshalResponse(r Response) []byte {
	b := make([]byte, ResponseSize)
	binary.LittleEndian.PutUint64(b[0:8], Magic)
	binary.LittleEndian.PutUint16(b[8:10], Version)
	binary.LittleEndian.PutUint64(b[16:24], r.Sequence)
	binary.LittleEndian.PutUint32(b[24:28], uint32(r.Status))
	binary.LittleEndian.PutUint32(b[28:32], r.Detail)
	return b
}

// UnmarshalResponse decodes r.
func UnmarshalResponse(b []byte) (Response, error) {
	if len(b) != ResponseSize {
		return Response{}, fmt.Errorf("monitor response is %d bytes, want %d", len(b), ResponseSize)
	}
	if binary.LittleEndian.Uint64(b[0:8]) != Magic || binary.LittleEndian.Uint16(b[8:10]) != Version {
		return Response{}, fmt.Errorf("invalid monitor response header")
	}
	if !allZero(b[10:16]) {
		return Response{}, fmt.Errorf("monitor response has nonzero reserved bytes")
	}
	r := Response{
		Sequence: binary.LittleEndian.Uint64(b[16:24]),
		Status:   Status(binary.LittleEndian.Uint32(b[24:28])),
		Detail:   binary.LittleEndian.Uint32(b[28:32]),
	}
	if r.Sequence == 0 {
		return Response{}, fmt.Errorf("monitor response sequence is zero")
	}
	if r.Status > StatusInternal {
		return Response{}, fmt.Errorf("monitor response status %d is invalid", r.Status)
	}
	return r, nil
}

func allZero(b []byte) bool {
	for _, value := range b {
		if value != 0 {
			return false
		}
	}
	return true
}
