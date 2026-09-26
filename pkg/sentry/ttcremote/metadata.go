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

package ttcremote

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
)

const (
	sessionMetadataVersion = 2
	sessionMetadataHeader  = 32
	sessionMetadataDigest  = sha256.Size
	sessionBindingBytes    = 72
)

var sessionMetadataMagic = [4]byte{'T', 'T', 'C', 'M'}

// SessionMetadata is the portable, non-page portion of a distributed fork.
// KernelMetadata is loaded by state.Load; AllocatorMetadata reconstructs the
// target MemoryFile; LoadRanges map the packed source stream into that file.
type SessionMetadata struct {
	FamilyID          string
	SourceBranchID    string
	TargetBranchID    string
	KernelMetadata    []byte
	AllocatorMetadata []byte
	LoadRanges        []pgalloc.RemoteLoadRange
	// MemoryBindings describe allocator-wide page identities in the same packed
	// stream as LoadRanges. They let an existing family relocate and reuse
	// identical resident chunks without importing a second allocator.
	MemoryBindings []pgalloc.CapturedPageBinding
}

// SessionMetadataLimits bounds allocations made while decoding an untrusted
// peer envelope.
type SessionMetadataLimits struct {
	MaxTotalBytes     uint64
	MaxKernelBytes    uint64
	MaxAllocatorBytes uint64
	MaxLoadRanges     uint32
}

// MarshalBinary returns a deterministic, SHA-256-protected wire envelope.
func (m *SessionMetadata) MarshalBinary() ([]byte, error) {
	strings := []string{m.FamilyID, m.SourceBranchID, m.TargetBranchID}
	for _, value := range strings {
		if len(value) > int(^uint16(0)) {
			return nil, errors.New("distributed branch identity exceeds 65535 bytes")
		}
	}
	if uint64(len(m.KernelMetadata)) > uint64(^uint32(0)) || uint64(len(m.AllocatorMetadata)) > uint64(^uint32(0)) || uint64(len(m.LoadRanges)) > uint64(^uint32(0)) || uint64(len(m.MemoryBindings)) > uint64(^uint32(0)) {
		return nil, errors.New("distributed metadata field exceeds protocol limit")
	}
	var body bytes.Buffer
	for _, value := range strings {
		if err := binary.Write(&body, binary.BigEndian, uint16(len(value))); err != nil {
			return nil, err
		}
		body.WriteString(value)
	}
	body.Write(m.KernelMetadata)
	body.Write(m.AllocatorMetadata)
	for _, load := range m.LoadRanges {
		if !load.Target.WellFormed() || load.Target.Length() == 0 {
			return nil, fmt.Errorf("invalid remote load range %+v", load)
		}
		for _, value := range []uint64{load.Target.Start, load.Target.End, load.StreamOffset} {
			if err := binary.Write(&body, binary.BigEndian, value); err != nil {
				return nil, err
			}
		}
	}
	for _, binding := range m.MemoryBindings {
		if binding.ResourceID.Ok() || binding.References == 0 || binding.Page.Allocator == (pgalloc.LogicalAllocatorID{}) || binding.Version.Allocator == (pgalloc.LogicalAllocatorID{}) {
			return nil, fmt.Errorf("invalid distributed memory binding %+v", binding)
		}
		for _, value := range []uint64{binding.OldOffset, binding.StreamOffset, binding.References} {
			if err := binary.Write(&body, binary.BigEndian, value); err != nil {
				return nil, err
			}
		}
		body.Write(binding.Page.Allocator[:])
		if err := binary.Write(&body, binary.BigEndian, binding.Page.Sequence); err != nil {
			return nil, err
		}
		body.Write(binding.Version.Allocator[:])
		if err := binary.Write(&body, binary.BigEndian, binding.Version.Sequence); err != nil {
			return nil, err
		}
	}
	encoded := make([]byte, sessionMetadataHeader+body.Len()+sessionMetadataDigest)
	copy(encoded[:4], sessionMetadataMagic[:])
	binary.BigEndian.PutUint16(encoded[4:6], sessionMetadataVersion)
	binary.BigEndian.PutUint32(encoded[8:12], uint32(len(m.KernelMetadata)))
	binary.BigEndian.PutUint32(encoded[12:16], uint32(len(m.AllocatorMetadata)))
	binary.BigEndian.PutUint32(encoded[16:20], uint32(len(m.LoadRanges)))
	binary.BigEndian.PutUint64(encoded[20:28], uint64(body.Len()))
	binary.BigEndian.PutUint32(encoded[28:32], uint32(len(m.MemoryBindings)))
	copy(encoded[sessionMetadataHeader:], body.Bytes())
	digest := sha256.Sum256(encoded[:len(encoded)-sessionMetadataDigest])
	copy(encoded[len(encoded)-sessionMetadataDigest:], digest[:])
	return encoded, nil
}

// UnmarshalSessionMetadata validates and decodes a portable session envelope.
func UnmarshalSessionMetadata(encoded []byte, limits SessionMetadataLimits) (*SessionMetadata, error) {
	if len(encoded) < sessionMetadataHeader+sessionMetadataDigest || uint64(len(encoded)) > limits.MaxTotalBytes {
		return nil, errors.New("distributed metadata size is outside configured limits")
	}
	if !bytes.Equal(encoded[:4], sessionMetadataMagic[:]) || binary.BigEndian.Uint16(encoded[4:6]) != sessionMetadataVersion {
		return nil, errors.New("invalid distributed metadata magic or version")
	}
	if encoded[6] != 0 || encoded[7] != 0 {
		return nil, errors.New("distributed metadata reserved fields are nonzero")
	}
	kernelLen := uint64(binary.BigEndian.Uint32(encoded[8:12]))
	allocatorLen := uint64(binary.BigEndian.Uint32(encoded[12:16]))
	rangeCount := binary.BigEndian.Uint32(encoded[16:20])
	bindingCount := binary.BigEndian.Uint32(encoded[28:32])
	bodyLen := binary.BigEndian.Uint64(encoded[20:28])
	if kernelLen > limits.MaxKernelBytes || allocatorLen > limits.MaxAllocatorBytes || rangeCount > limits.MaxLoadRanges || bindingCount > limits.MaxLoadRanges ||
		bodyLen != uint64(len(encoded)-sessionMetadataHeader-sessionMetadataDigest) {
		return nil, errors.New("distributed metadata fields exceed configured limits or size is inconsistent")
	}
	want := sha256.Sum256(encoded[:len(encoded)-sessionMetadataDigest])
	if !bytes.Equal(want[:], encoded[len(encoded)-sessionMetadataDigest:]) {
		return nil, errors.New("distributed metadata digest mismatch")
	}
	r := bytes.NewReader(encoded[sessionMetadataHeader : len(encoded)-sessionMetadataDigest])
	readString := func() (string, error) {
		var length uint16
		if err := binary.Read(r, binary.BigEndian, &length); err != nil {
			return "", err
		}
		value := make([]byte, length)
		_, err := io.ReadFull(r, value)
		return string(value), err
	}
	m := &SessionMetadata{}
	var err error
	if m.FamilyID, err = readString(); err != nil {
		return nil, err
	}
	if m.SourceBranchID, err = readString(); err != nil {
		return nil, err
	}
	if m.TargetBranchID, err = readString(); err != nil {
		return nil, err
	}
	if kernelLen+allocatorLen+uint64(rangeCount)*24+uint64(bindingCount)*sessionBindingBytes != uint64(r.Len()) {
		return nil, errors.New("distributed metadata payload layout is inconsistent")
	}
	m.KernelMetadata = make([]byte, kernelLen)
	m.AllocatorMetadata = make([]byte, allocatorLen)
	if _, err := io.ReadFull(r, m.KernelMetadata); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(r, m.AllocatorMetadata); err != nil {
		return nil, err
	}
	m.LoadRanges = make([]pgalloc.RemoteLoadRange, rangeCount)
	for i := range m.LoadRanges {
		var start, end, offset uint64
		for _, value := range []*uint64{&start, &end, &offset} {
			if err := binary.Read(r, binary.BigEndian, value); err != nil {
				return nil, err
			}
		}
		m.LoadRanges[i] = pgalloc.RemoteLoadRange{Target: memmap.FileRange{Start: start, End: end}, StreamOffset: offset}
		if !m.LoadRanges[i].Target.WellFormed() || m.LoadRanges[i].Target.Length() == 0 {
			return nil, errors.New("distributed metadata contains invalid load range")
		}
	}
	m.MemoryBindings = make([]pgalloc.CapturedPageBinding, bindingCount)
	for i := range m.MemoryBindings {
		binding := &m.MemoryBindings[i]
		for _, value := range []*uint64{&binding.OldOffset, &binding.StreamOffset, &binding.References} {
			if err := binary.Read(r, binary.BigEndian, value); err != nil {
				return nil, err
			}
		}
		if _, err := io.ReadFull(r, binding.Page.Allocator[:]); err != nil {
			return nil, err
		}
		if err := binary.Read(r, binary.BigEndian, &binding.Page.Sequence); err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(r, binding.Version.Allocator[:]); err != nil {
			return nil, err
		}
		if err := binary.Read(r, binary.BigEndian, &binding.Version.Sequence); err != nil {
			return nil, err
		}
		if binding.References == 0 || binding.Page.Allocator == (pgalloc.LogicalAllocatorID{}) || binding.Version.Allocator == (pgalloc.LogicalAllocatorID{}) {
			return nil, errors.New("distributed metadata contains invalid memory binding")
		}
		binding.Chunk, binding.Slot = binding.Page.Chunk()
	}
	return m, nil
}
