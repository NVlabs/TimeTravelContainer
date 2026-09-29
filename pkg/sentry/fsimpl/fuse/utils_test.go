// Copyright 2020 The gVisor Authors.
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

package fuse

import (
	"bytes"
	"fmt"
	"testing"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/testutil"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

func setup(t *testing.T) *testutil.System {
	k, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Error creating kernel: %v", err)
	}

	ctx := k.SupervisorContext()
	creds := auth.CredentialsFromContext(ctx)

	k.VFS().MustRegisterFilesystemType(Name, &FilesystemType{}, &vfs.RegisterFilesystemTypeOptions{
		AllowUserList:  true,
		AllowUserMount: true,
	})

	mntns, err := k.VFS().NewMountNamespace(ctx, creds, "", "tmpfs", &vfs.MountOptions{}, nil)
	if err != nil {
		t.Fatalf("NewMountNamespace(): %v", err)
	}

	return testutil.NewSystem(ctx, t, k.VFS(), mntns)
}

// newTestConnection creates a fuse connection that the sentry can communicate with
// and the FD for the server to communicate with.
func newTestConnection(system *testutil.System, maxActiveRequests uint64) (*connection, *vfs.FileDescription, error) {
	fuseDev := &DeviceFD{}

	vd := system.VFS.NewAnonVirtualDentry("fuse")
	defer vd.DecRef(system.Ctx)
	if err := fuseDev.vfsfd.Init(fuseDev, linux.O_RDWR, auth.CredentialsFromContext(system.Ctx), vd.Mount(), vd.Dentry(), &vfs.FileDescriptionOptions{}); err != nil {
		return nil, nil, err
	}

	fsopts := filesystemOptions{
		maxActiveRequests: maxActiveRequests,
	}
	fuseDev.mu.Lock()
	conn, err := newFUSEConnection(system.Ctx, fuseDev, &fsopts)
	if err != nil {
		return nil, nil, err
	}
	fuseDev.conn = conn
	fuseDev.mu.Unlock()

	// Fake the connection being properly initialized for testing purposes.
	conn.mu.Lock()
	conn.connInitSuccess = true
	conn.mu.Unlock()
	return conn, &fuseDev.vfsfd, nil
}

// newTestFilesystem creates a filesystem that the sentry can communicate with
// and the FD for the server to communicate with.
func newTestFilesystem(system *testutil.System, fd *vfs.FileDescription, maxActiveRequests uint64) (*filesystem, error) {
	fuseFD, ok := fd.Impl().(*DeviceFD)
	if !ok {
		return nil, fmt.Errorf("newTestFilesystem: FD is %T, not a FUSE device", fd)
	}
	fsopts := filesystemOptions{
		maxActiveRequests: maxActiveRequests,
	}

	fuseFD.mu.Lock()
	defer fuseFD.mu.Unlock()
	fs, err := newFUSEFilesystem(system.Ctx, system.VFS, &FilesystemType{}, fuseFD, 0, &fsopts)
	if err != nil {
		return nil, err
	}
	return fs, nil
}

func TestStatFromFUSEAttrReportsRequestedType(t *testing.T) {
	stat := statFromFUSEAttr(linux.FUSEAttr{Mode: uint32(linux.ModeRegular | 0755)}, linux.STATX_TYPE, 1)
	if stat.Mask&linux.STATX_TYPE == 0 {
		t.Fatalf("stat mask %#x does not report STATX_TYPE", stat.Mask)
	}
	if got := linux.FileMode(stat.Mode).FileType(); got != linux.ModeRegular {
		t.Fatalf("file type %v, want regular", got)
	}
}

func TestFileCacheUsesFUSEInodeIdentity(t *testing.T) {
	fs := &filesystem{}
	first := fs.fileCache(2, 7)
	if got := fs.fileCache(2, 7); got != first {
		t.Fatal("same FUSE inode identity received distinct mmap caches")
	}
	if got := fs.fileCache(2, 8); got == first {
		t.Fatal("reused FUSE node ID with a new generation shared an mmap cache")
	}
}

func TestFileCacheLiveForkCopyOnWrite(t *testing.T) {
	system := setup(t)
	mf := pgalloc.MemoryFileFromContext(system.Ctx)
	cache := &fileCache{}
	defer cache.dropAll(mf)

	fr, err := mf.Allocate(hostarch.PageSize, pgalloc.AllocOpts{
		Kind: usage.PageCache,
		Mode: pgalloc.AllocateAndWritePopulate,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Repeat([]byte{'x'}, hostarch.PageSize)
	ims, err := mf.MapInternal(fr, hostarch.Write)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := safemem.CopySeq(ims, safemem.BlockSeqOf(safemem.BlockFromSafeSlice(want))); err != nil || n != hostarch.PageSize {
		t.Fatalf("initializing cache: n=%d err=%v", n, err)
	}
	cache.data.Insert(cache.data.FindGap(0), memmap.MappableRange{Start: 0, End: hostarch.PageSize}, fr.Start)
	original := fr.Start
	cache.prepareLiveFork()

	cache.dataMu.Lock()
	err = cache.copyOnWriteLocked(system.Ctx, &filesystem{mf: mf}, memmap.MappableRange{Start: 0, End: hostarch.PageSize})
	seg := cache.data.FindSegment(0)
	empty := cache.cow.IsEmpty()
	cache.dataMu.Unlock()
	if err != nil {
		t.Fatalf("copyOnWriteLocked: %v", err)
	}
	if !seg.Ok() || seg.Value() == original {
		t.Fatal("FUSE mmap cache retained shared MemoryFile backing after CoW")
	}
	if !empty {
		t.Fatal("FUSE mmap cache retained completed CoW metadata")
	}
	gotIMS, err := mf.MapInternal(seg.FileRange(), hostarch.Read)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, hostarch.PageSize)
	if n, err := safemem.CopySeq(safemem.BlockSeqOf(safemem.BlockFromSafeSlice(got)), gotIMS); err != nil || n != hostarch.PageSize {
		t.Fatalf("reading CoW cache: n=%d err=%v", n, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("FUSE mmap CoW did not preserve page contents")
	}
}

func TestLiveForkFilesystemEligibilityDistinguishesTransport(t *testing.T) {
	conn := &connection{}
	conn.fuseConn = &deviceConn{conn: conn}
	fs := filesystem{conn: conn}
	if got := fs.LiveForkFilesystemEligibility().Decision; got != vfs.LiveForkFilesystemClone {
		t.Fatalf("in-sandbox FUSE decision %q, want clone", got)
	}
	conn.fuseConn = &hostConnection{}
	if got := fs.LiveForkFilesystemEligibility().Decision; got != vfs.LiveForkFilesystemReject {
		t.Fatalf("external FUSE decision %q, want reject", got)
	}
}
