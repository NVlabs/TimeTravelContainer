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

// Package ebpffd provides file descriptors that refer to eBPF objects.
package ebpffd

import (
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/ebpf"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

// ProgramFD represents an eBPF program file descriptor
//
// +stateify savable
type ProgramFD struct {
	vfsfd vfs.FileDescription
	vfs.FileDescriptionDefaultImpl
	vfs.DentryMetadataFileDescriptionImpl
	vfs.NoLockFD

	// program is the eBPF program referred to by this file descriptor.
	//
	// Immutable.
	program *ebpf.Program
}

var _ vfs.LiveForkFileDescription = (*ProgramFD)(nil)

// ValidateLiveFork implements vfs.LiveForkFileDescription. Programs are
// immutable Sentry objects and contain no host resource.
func (*ProgramFD) ValidateLiveFork() ([]*vfs.FileDescription, error) {
	return nil, nil
}

// New returns a new eBPF program file descriptor, which takes a reference
// on program.
func New(ctx context.Context, vfsObj *vfs.VirtualFilesystem, fileFlags uint32, program *ebpf.Program) (*vfs.FileDescription, error) {
	creds := auth.CredentialsFromContext(ctx)
	fd := &ProgramFD{
		program: program,
	}

	vd := vfsObj.NewAnonVirtualDentry("bpf-prog")
	defer vd.DecRef(ctx)

	err := fd.vfsfd.Init(fd, fileFlags, creds, vd.Mount(), vd.Dentry(), &vfs.FileDescriptionOptions{
		UseDentryMetadata: true,
		DenyPRead:         true,
		DenyPWrite:        true,
	})
	if err != nil {
		return nil, err
	}
	program.IncRef()

	return &fd.vfsfd, nil
}

// Release implements vfs.FileDescriptionImpl.Release.
func (fd *ProgramFD) Release(ctx context.Context) {
	fd.program.DecRef()
}

// Program returns the eBPF program associated with the file descriptor.
func (fd *ProgramFD) Program() *ebpf.Program {
	return fd.program
}

// MapFD represents an eBPF map file descriptor. Its file's mode limits what
// bpf(2) may do with the map: reads need a readable file, and writes, a
// writable one.
//
// +stateify savable
type MapFD struct {
	vfsfd vfs.FileDescription
	vfs.FileDescriptionDefaultImpl
	vfs.DentryMetadataFileDescriptionImpl
	vfs.NoLockFD

	// m is the eBPF map referred to by this file descriptor.
	//
	// Immutable.
	m *ebpf.Map
}

var _ vfs.LiveForkFileDescription = (*MapFD)(nil)

// ValidateLiveFork implements vfs.LiveForkFileDescription. Maps are Sentry
// objects and contain no host resource.
func (*MapFD) ValidateLiveFork() ([]*vfs.FileDescription, error) {
	return nil, nil
}

// NewMap returns a new eBPF map file descriptor, which takes a reference on
// m.
func NewMap(ctx context.Context, vfsObj *vfs.VirtualFilesystem, fileFlags uint32, m *ebpf.Map) (*vfs.FileDescription, error) {
	creds := auth.CredentialsFromContext(ctx)
	fd := &MapFD{
		m: m,
	}

	vd := vfsObj.NewAnonVirtualDentry("bpf-map")
	defer vd.DecRef(ctx)

	err := fd.vfsfd.Init(fd, fileFlags, creds, vd.Mount(), vd.Dentry(), &vfs.FileDescriptionOptions{
		UseDentryMetadata: true,
		DenyPRead:         true,
		DenyPWrite:        true,
	})
	if err != nil {
		return nil, err
	}
	m.IncRef()

	return &fd.vfsfd, nil
}

// Release implements vfs.FileDescriptionImpl.Release.
func (fd *MapFD) Release(ctx context.Context) {
	fd.m.DecRef()
}

// Map returns the eBPF map associated with the file descriptor.
func (fd *MapFD) Map() *ebpf.Map {
	return fd.m
}
