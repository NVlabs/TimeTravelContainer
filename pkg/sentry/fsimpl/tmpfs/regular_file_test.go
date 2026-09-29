// Copyright 2019 The gVisor Authors.
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

package tmpfs

import (
	"bytes"
	"fmt"
	"io"
	stdsync "sync"
	"testing"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/contexttest"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/lock"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/usermem"
)

type recordingMappingSpace struct {
	invalidated bool
	opts        memmap.InvalidateOpts
}

type lockDetectingIO struct {
	*usermem.BytesIO
	mu stdsync.RWMutex
}

func (i *lockDetectingIO) CopyIn(ctx context.Context, addr hostarch.Addr, dst []byte, opts usermem.IOOpts) (int, error) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.BytesIO.CopyIn(ctx, addr, dst, opts)
}

func (i *lockDetectingIO) CopyInTo(ctx context.Context, ars hostarch.AddrRangeSeq, dst safemem.Writer, opts usermem.IOOpts) (int64, error) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.BytesIO.CopyInTo(ctx, ars, dst, opts)
}

type lockDetectingMappingSpace struct {
	io          *lockDetectingIO
	invalidated bool
	reentrant   bool
}

func (ms *lockDetectingMappingSpace) Invalidate(_ hostarch.AddrRange, _ memmap.InvalidateOpts) {
	ms.invalidated = true
	if !ms.io.mu.TryLock() {
		ms.reentrant = true
		return
	}
	ms.io.mu.Unlock() // +checklocksforce: TryLock acquired the write lock.
}

func (ms *recordingMappingSpace) Invalidate(_ hostarch.AddrRange, opts memmap.InvalidateOpts) {
	ms.invalidated = true
	ms.opts = opts
}

func TestCaptureMemoryFileRangesIncludesUnmappedRegularFile(t *testing.T) {
	ctx := contexttest.Context(t)
	fd, cleanup, err := newFileFD(ctx, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	defer fd.DecRef(ctx)
	data := make([]byte, hostarch.PageSize)
	if n, err := fd.PWrite(ctx, usermem.BytesIOSequence(data), 0, vfs.WriteOptions{}); err != nil || n != int64(len(data)) {
		t.Fatalf("PWrite: n=%d err=%v", n, err)
	}
	capture := pgalloc.NewMemoryRangeCapture()
	if err := CaptureMemoryFileRanges(fd.Mount().Filesystem(), capture); err != nil {
		t.Fatal(err)
	}
	bindings, err := capture.Bindings()
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 {
		t.Fatalf("captured %d pages, want 1", len(bindings))
	}
	if bindings[0].References != 1 {
		t.Fatalf("captured %d references, want 1", bindings[0].References)
	}
}

func TestPrepareLiveForkPreservesPrivatePMAs(t *testing.T) {
	ctx := contexttest.Context(t)
	fd, cleanup, err := newFileFD(ctx, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	defer fd.DecRef(ctx)

	rf := fd.Impl().(*regularFileFD).inode().impl.(*regularFile)
	ms := &recordingMappingSpace{}
	ar := hostarch.AddrRange{Start: 0x10000, End: 0x10000 + hostarch.PageSize}
	if err := rf.AddMapping(ctx, ms, ar, 0, true); err != nil {
		t.Fatalf("AddMapping: %v", err)
	}
	defer rf.RemoveMapping(ctx, ms, ar, 0, true)

	rf.prepareLiveFork()
	if !ms.invalidated {
		t.Fatal("prepareLiveFork did not invalidate file-backed translations")
	}
	if ms.opts.InvalidatePrivate {
		t.Fatal("prepareLiveFork invalidated private PMAs containing application modifications")
	}
}

func TestDiskVolumeAfterLoadRebuildsMmapReferences(t *testing.T) {
	fs := &filesystem{diskVolume: &diskVolumeState{}}
	rf := &regularFile{inode: inode{fs: fs}}
	firstMS := &recordingMappingSpace{}
	secondMS := &recordingMappingSpace{}
	firstAR := hostarch.AddrRange{
		Start: 0x10000,
		End:   0x10000 + 4*hostarch.PageSize,
	}
	secondAR := hostarch.AddrRange{
		Start: 0x20000,
		End:   0x20000 + 2*hostarch.PageSize,
	}

	// State restore reconstructs MappingSet directly instead of invoking
	// regularFile.AddMapping. Model that here, including overlapping mappings
	// of the same file range.
	rf.mappings.AddMapping(firstMS, firstAR, 0, false)
	rf.mappings.AddMapping(secondMS, secondAR, hostarch.PageSize, false)
	rf.afterLoad(contexttest.Context(t))

	// Each removal must have a matching diskMmap reference. Before the restore
	// reconstruction in afterLoad, the first removal panicked with a reference
	// count underflow.
	rf.RemoveMapping(contexttest.Context(t), firstMS, firstAR, 0, false)
	rf.RemoveMapping(contexttest.Context(t), secondMS, secondAR, hostarch.PageSize, false)
}

func TestLiveForkPWriteFromSameMemoryManagerDoesNotReenterActiveLock(t *testing.T) {
	ctx := contexttest.Context(t)
	fd, cleanup, err := newFileFD(ctx, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	defer fd.DecRef(ctx)

	data := bytes.Repeat([]byte{'a'}, hostarch.PageSize)
	if n, err := fd.PWrite(ctx, usermem.BytesIOSequence(data), 0, vfs.WriteOptions{}); err != nil || n != int64(len(data)) {
		t.Fatalf("initial PWrite: n=%d err=%v", n, err)
	}
	rf := fd.Impl().(*regularFileFD).inode().impl.(*regularFile)
	guarded := &lockDetectingIO{BytesIO: &usermem.BytesIO{Bytes: bytes.Repeat([]byte{'b'}, hostarch.PageSize)}}
	ms := &lockDetectingMappingSpace{io: guarded}
	ar := hostarch.AddrRange{Start: 0x10000, End: 0x10000 + hostarch.PageSize}
	if err := rf.AddMapping(ctx, ms, ar, 0, true); err != nil {
		t.Fatalf("AddMapping: %v", err)
	}
	defer rf.RemoveMapping(ctx, ms, ar, 0, true)
	rf.prepareLiveFork()
	ms.invalidated = false

	src := usermem.IOSequence{
		IO:    guarded,
		Addrs: hostarch.AddrRangeSeqOf(hostarch.AddrRange{Start: 0, End: hostarch.PageSize}),
	}
	if n, err := fd.PWrite(ctx, src, 0, vfs.WriteOptions{}); err != nil || n != int64(hostarch.PageSize) {
		t.Fatalf("CoW PWrite: n=%d err=%v", n, err)
	}
	if !ms.invalidated {
		t.Fatal("CoW PWrite did not invalidate the file mapping")
	}
	if ms.reentrant {
		t.Fatal("CoW PWrite invalidated the source MemoryManager while its read lock was held")
	}
	got := make([]byte, hostarch.PageSize)
	if n, err := fd.PRead(ctx, usermem.BytesIOSequence(got), 0, vfs.ReadOptions{}); err != nil || n != int64(len(got)) {
		t.Fatalf("PRead after CoW write: n=%d err=%v", n, err)
	}
	if !bytes.Equal(got, guarded.Bytes) {
		t.Fatal("CoW PWrite data mismatch")
	}
}

func TestLiveForkCopyOnWriteSkipsAdjacentRange(t *testing.T) {
	ctx := contexttest.Context(t)
	fd, cleanup, err := newFileFD(ctx, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	defer fd.DecRef(ctx)

	data := bytes.Repeat([]byte{'a'}, 2*hostarch.PageSize)
	if n, err := fd.PWrite(ctx, usermem.BytesIOSequence(data), 0, vfs.WriteOptions{}); err != nil || n != int64(len(data)) {
		t.Fatalf("initial PWrite: n=%d err=%v", n, err)
	}
	rf := fd.Impl().(*regularFileFD).inode().impl.(*regularFile)
	rf.prepareLiveFork()

	// Leave a CoW range that ends exactly where the requested translation
	// starts. FileRangeSet.LowerBoundSegment may return that preceding segment,
	// but adjacency is not a zero-length CoW operation.
	rf.dataMu.Lock()
	rf.cow.RemoveRange(memmap.MappableRange{Start: hostarch.PageSize, End: 2 * hostarch.PageSize})
	err = rf.copyOnWriteLocked(ctx, memmap.MappableRange{Start: hostarch.PageSize, End: 2 * hostarch.PageSize})
	rf.dataMu.Unlock()
	if err != nil {
		t.Fatalf("copyOnWriteLocked for range adjacent to CoW state: %v", err)
	}
}

func TestLiveForkCopyOnWriteRemovesFinalRange(t *testing.T) {
	ctx := contexttest.Context(t)
	fd, cleanup, err := newFileFD(ctx, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	defer fd.DecRef(ctx)

	data := bytes.Repeat([]byte{'a'}, hostarch.PageSize)
	if n, err := fd.PWrite(ctx, usermem.BytesIOSequence(data), 0, vfs.WriteOptions{}); err != nil || n != int64(len(data)) {
		t.Fatalf("initial PWrite: n=%d err=%v", n, err)
	}
	rf := fd.Impl().(*regularFileFD).inode().impl.(*regularFile)
	rf.prepareLiveFork()

	rf.dataMu.Lock()
	err = rf.copyOnWriteLocked(ctx, memmap.MappableRange{Start: 0, End: hostarch.PageSize})
	empty := rf.cow.IsEmpty()
	rf.dataMu.Unlock()
	if err != nil {
		t.Fatalf("copyOnWriteLocked for final CoW range: %v", err)
	}
	if !empty {
		t.Fatal("final CoW range was not removed")
	}
}

// Test that we can write some data to a file and read it back.`
func TestSimpleWriteRead(t *testing.T) {
	ctx := contexttest.Context(t)
	fd, cleanup, err := newFileFD(ctx, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	// Write.
	data := []byte("foobarbaz")
	n, err := fd.Write(ctx, usermem.BytesIOSequence(data), vfs.WriteOptions{})
	if err != nil {
		t.Fatalf("fd.Write failed: %v", err)
	}
	if n != int64(len(data)) {
		t.Errorf("fd.Write got short write length %d, want %d", n, len(data))
	}
	if got, want := fd.Impl().(*regularFileFD).inode().fs.AllocatedBytes(), uint64(hostarch.PageSize); got != want {
		t.Errorf("AllocatedBytes after write: got %d, want %d", got, want)
	}
	if got, want := fd.Impl().(*regularFileFD).off, int64(len(data)); got != want {
		t.Errorf("fd.Write left offset at %d, want %d", got, want)
	}

	// Seek back to beginning.
	if _, err := fd.Seek(ctx, 0, linux.SEEK_SET); err != nil {
		t.Fatalf("fd.Seek failed: %v", err)
	}
	if got, want := fd.Impl().(*regularFileFD).off, int64(0); got != want {
		t.Errorf("fd.Seek(0) left offset at %d, want %d", got, want)
	}

	// Read.
	buf := make([]byte, len(data))
	n, err = fd.Read(ctx, usermem.BytesIOSequence(buf), vfs.ReadOptions{})
	if err != nil && err != io.EOF {
		t.Fatalf("fd.Read failed: %v", err)
	}
	if n != int64(len(data)) {
		t.Errorf("fd.Read got short read length %d, want %d", n, len(data))
	}
	if got, want := string(buf), string(data); got != want {
		t.Errorf("Read got %q want %s", got, want)
	}
	if got, want := fd.Impl().(*regularFileFD).off, int64(len(data)); got != want {
		t.Errorf("fd.Write left offset at %d, want %d", got, want)
	}
}

func TestPWrite(t *testing.T) {
	ctx := contexttest.Context(t)
	fd, cleanup, err := newFileFD(ctx, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	// Fill file with 1k 'a's.
	data := bytes.Repeat([]byte{'a'}, 1000)
	n, err := fd.Write(ctx, usermem.BytesIOSequence(data), vfs.WriteOptions{})
	if err != nil {
		t.Fatalf("fd.Write failed: %v", err)
	}
	if n != int64(len(data)) {
		t.Errorf("fd.Write got short write length %d, want %d", n, len(data))
	}

	// Write "gVisor is awesome" at various offsets.
	buf := []byte("gVisor is awesome")
	offsets := []int{0, 1, 2, 10, 20, 50, 100, len(data) - 100, len(data) - 1, len(data), len(data) + 1}
	for _, offset := range offsets {
		name := fmt.Sprintf("PWrite offset=%d", offset)
		t.Run(name, func(t *testing.T) {
			n, err := fd.PWrite(ctx, usermem.BytesIOSequence(buf), int64(offset), vfs.WriteOptions{})
			if err != nil {
				t.Errorf("fd.PWrite got err %v want nil", err)
			}
			if n != int64(len(buf)) {
				t.Errorf("fd.PWrite got %d bytes want %d", n, len(buf))
			}

			// Update data to reflect expected file contents.
			if len(data) < offset+len(buf) {
				data = append(data, make([]byte, (offset+len(buf))-len(data))...)
			}
			copy(data[offset:], buf)

			// Read the whole file and compare with data.
			readBuf := make([]byte, len(data))
			n, err = fd.PRead(ctx, usermem.BytesIOSequence(readBuf), 0, vfs.ReadOptions{})
			if err != nil {
				t.Fatalf("fd.PRead failed: %v", err)
			}
			if n != int64(len(data)) {
				t.Errorf("fd.PRead got short read length %d, want %d", n, len(data))
			}
			if got, want := string(readBuf), string(data); got != want {
				t.Errorf("PRead got %q want %s", got, want)
			}

		})
	}
}

func TestLocks(t *testing.T) {
	ctx := contexttest.Context(t)
	fd, cleanup, err := newFileFD(ctx, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	uid1 := 123
	uid2 := 456
	if err := fd.Impl().LockBSD(ctx, uid1, 0 /* ownerPID */, lock.ReadLock, false /* block */); err != nil {
		t.Fatalf("fd.Impl().LockBSD failed: err = %v", err)
	}
	if err := fd.Impl().LockBSD(ctx, uid2, 0 /* ownerPID */, lock.ReadLock, false /* block */); err != nil {
		t.Fatalf("fd.Impl().LockBSD failed: err = %v", err)
	}
	if got, want := fd.Impl().LockBSD(ctx, uid2, 0 /* ownerPID */, lock.WriteLock, false /* block */), linuxerr.ErrWouldBlock; got != want {
		t.Fatalf("fd.Impl().LockBSD failed: got = %v, want = %v", got, want)
	}
	if err := fd.Impl().UnlockBSD(ctx, uid1); err != nil {
		t.Fatalf("fd.Impl().UnlockBSD failed: err = %v", err)
	}
	if err := fd.Impl().LockBSD(ctx, uid2, 0 /* ownerPID */, lock.WriteLock, false /* block */); err != nil {
		t.Fatalf("fd.Impl().LockBSD failed: err = %v", err)
	}

	if err := fd.Impl().LockPOSIX(ctx, uid1, 0 /* ownerPID */, lock.ReadLock, lock.LockRange{Start: 0, End: 1}, false /* block */); err != nil {
		t.Fatalf("fd.Impl().LockPOSIX failed: err = %v", err)
	}
	if err := fd.Impl().LockPOSIX(ctx, uid2, 0 /* ownerPID */, lock.ReadLock, lock.LockRange{Start: 1, End: 2}, false /* block */); err != nil {
		t.Fatalf("fd.Impl().LockPOSIX failed: err = %v", err)
	}
	if err := fd.Impl().LockPOSIX(ctx, uid1, 0 /* ownerPID */, lock.WriteLock, lock.LockRange{Start: 0, End: 1}, false /* block */); err != nil {
		t.Fatalf("fd.Impl().LockPOSIX failed: err = %v", err)
	}
	if got, want := fd.Impl().LockPOSIX(ctx, uid2, 0 /* ownerPID */, lock.ReadLock, lock.LockRange{Start: 0, End: 1}, false /* block */), linuxerr.ErrWouldBlock; got != want {
		t.Fatalf("fd.Impl().LockPOSIX failed: got = %v, want = %v", got, want)
	}
	if err := fd.Impl().UnlockPOSIX(ctx, uid1, lock.LockRange{Start: 0, End: 1}); err != nil {
		t.Fatalf("fd.Impl().UnlockPOSIX failed: err = %v", err)
	}
}

func TestPRead(t *testing.T) {
	ctx := contexttest.Context(t)
	fd, cleanup, err := newFileFD(ctx, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	// Write 100 sequences of 'gVisor is awesome'.
	data := bytes.Repeat([]byte("gVisor is awesome"), 100)
	n, err := fd.Write(ctx, usermem.BytesIOSequence(data), vfs.WriteOptions{})
	if err != nil {
		t.Fatalf("fd.Write failed: %v", err)
	}
	if n != int64(len(data)) {
		t.Errorf("fd.Write got short write length %d, want %d", n, len(data))
	}

	// Read various sizes from various offsets.
	sizes := []int{0, 1, 2, 10, 20, 50, 100, 1000}
	offsets := []int{0, 1, 2, 10, 20, 50, 100, 1000, len(data) - 100, len(data) - 1, len(data), len(data) + 1}

	for _, size := range sizes {
		for _, offset := range offsets {
			name := fmt.Sprintf("PRead offset=%d size=%d", offset, size)
			t.Run(name, func(t *testing.T) {
				var (
					wantRead []byte
					wantErr  error
				)
				if offset < len(data) {
					wantRead = data[offset:]
				} else if size > 0 {
					wantErr = io.EOF
				}
				if offset+size < len(data) {
					wantRead = wantRead[:size]
				}
				buf := make([]byte, size)
				n, err := fd.PRead(ctx, usermem.BytesIOSequence(buf), int64(offset), vfs.ReadOptions{})
				if err != wantErr {
					t.Errorf("fd.PRead got err %v want %v", err, wantErr)
				}
				if n != int64(len(wantRead)) {
					t.Errorf("fd.PRead got %d bytes want %d", n, len(wantRead))
				}
				if got := string(buf[:n]); got != string(wantRead) {
					t.Errorf("fd.PRead got %q want %q", got, string(wantRead))
				}
			})
		}
	}
}

func TestTruncate(t *testing.T) {
	ctx := contexttest.Context(t)
	fd, cleanup, err := newFileFD(ctx, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	// Fill the file with some data.
	data := bytes.Repeat([]byte("gVisor is awesome"), 100)
	written, err := fd.Write(ctx, usermem.BytesIOSequence(data), vfs.WriteOptions{})
	if err != nil {
		t.Fatalf("fd.Write failed: %v", err)
	}

	// Size should be same as written.
	sizeStatOpts := vfs.StatOptions{Mask: linux.STATX_SIZE}
	stat, err := fd.Stat(ctx, sizeStatOpts)
	if err != nil {
		t.Fatalf("fd.Stat failed: %v", err)
	}
	if got, want := int64(stat.Size), written; got != want {
		t.Errorf("fd.Stat got size %d, want %d", got, want)
	}

	// Truncate down.
	newSize := uint64(10)
	if err := fd.SetStat(ctx, vfs.SetStatOptions{
		Stat: linux.Statx{
			Mask: linux.STATX_SIZE,
			Size: newSize,
		},
	}); err != nil {
		t.Errorf("fd.Truncate failed: %v", err)
	}
	// Size should be updated.
	statAfterTruncateDown, err := fd.Stat(ctx, sizeStatOpts)
	if err != nil {
		t.Fatalf("fd.Stat failed: %v", err)
	}
	if got, want := statAfterTruncateDown.Size, newSize; got != want {
		t.Errorf("fd.Stat got size %d, want %d", got, want)
	}
	// We should only read newSize worth of data.
	buf := make([]byte, 1000)
	if n, err := fd.PRead(ctx, usermem.BytesIOSequence(buf), 0, vfs.ReadOptions{}); err != nil && err != io.EOF {
		t.Fatalf("fd.PRead failed: %v", err)
	} else if uint64(n) != newSize {
		t.Errorf("fd.PRead got size %d, want %d", n, newSize)
	}
	// Mtime and Ctime should be bumped.
	if got := statAfterTruncateDown.Mtime.ToNsec(); got <= stat.Mtime.ToNsec() {
		t.Errorf("fd.Stat got Mtime %v, want > %v", got, stat.Mtime)
	}
	if got := statAfterTruncateDown.Ctime.ToNsec(); got <= stat.Ctime.ToNsec() {
		t.Errorf("fd.Stat got Ctime %v, want > %v", got, stat.Ctime)
	}

	// Truncate up.
	newSize = 100
	if err := fd.SetStat(ctx, vfs.SetStatOptions{
		Stat: linux.Statx{
			Mask: linux.STATX_SIZE,
			Size: newSize,
		},
	}); err != nil {
		t.Errorf("fd.Truncate failed: %v", err)
	}
	// Size should be updated.
	statAfterTruncateUp, err := fd.Stat(ctx, sizeStatOpts)
	if err != nil {
		t.Fatalf("fd.Stat failed: %v", err)
	}
	if got, want := statAfterTruncateUp.Size, newSize; got != want {
		t.Errorf("fd.Stat got size %d, want %d", got, want)
	}
	// We should read newSize worth of data.
	buf = make([]byte, 1000)
	if n, err := fd.PRead(ctx, usermem.BytesIOSequence(buf), 0, vfs.ReadOptions{}); err != nil && err != io.EOF {
		t.Fatalf("fd.PRead failed: %v", err)
	} else if uint64(n) != newSize {
		t.Errorf("fd.PRead got size %d, want %d", n, newSize)
	}
	// Bytes should be null after 10, since we previously truncated to 10.
	for i := uint64(10); i < newSize; i++ {
		if buf[i] != 0 {
			t.Errorf("fd.PRead got byte %d=%x, want 0", i, buf[i])
			break
		}
	}
	// Mtime and Ctime should be bumped.
	if got := statAfterTruncateUp.Mtime.ToNsec(); got <= statAfterTruncateDown.Mtime.ToNsec() {
		t.Errorf("fd.Stat got Mtime %v, want > %v", got, statAfterTruncateDown.Mtime)
	}
	if got := statAfterTruncateUp.Ctime.ToNsec(); got <= statAfterTruncateDown.Ctime.ToNsec() {
		t.Errorf("fd.Stat got Ctime %v, want > %v", got, stat.Ctime)
	}
}
