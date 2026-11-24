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

//go:build !pagesize_64k

package pgalloc

import (
	"bytes"
	"context"
	"io"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/memutil"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

func newSaveTestMemoryFile(t *testing.T, diskBacked bool) *MemoryFile {
	t.Helper()
	var file *os.File
	if diskBacked {
		var err error
		if file, err = os.CreateTemp(t.TempDir(), "pgalloc-save-test"); err != nil {
			t.Fatalf("CreateTemp: %v", err)
		}
	} else {
		fd, err := memutil.CreateMemFD("pgalloc-save-test", 0)
		if err != nil {
			t.Fatalf("CreateMemFD: %v", err)
		}
		file = os.NewFile(uintptr(fd), "pgalloc-save-test")
	}
	f, err := NewMemoryFile(file, MemoryFileOpts{
		DelayedEviction:         DelayedEvictionDisabled,
		DisableMemoryAccounting: true,
		DiskBackedFile:          diskBacked,
	})
	if err != nil {
		file.Close()
		t.Fatalf("NewMemoryFile: %v", err)
	}
	t.Cleanup(f.Destroy)
	return f
}

func allocateSaveTestPages(t *testing.T, f *MemoryFile) memmap.FileRange {
	t.Helper()
	fr, err := f.Allocate(16*uint64(hostarch.PageSize), AllocOpts{Mode: AllocateUncommitted})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	t.Cleanup(func() { f.DecRef(fr) })
	return fr
}

func pwritePage(t *testing.T, f *MemoryFile, fr memmap.FileRange, page int, b byte) {
	t.Helper()
	buf := make([]byte, hostarch.PageSize)
	buf[17] = b
	if _, err := unix.Pwrite(f.FD(), buf, int64(fr.Start)+int64(page*int(hostarch.PageSize))); err != nil {
		t.Fatalf("Pwrite page %d: %v", page, err)
	}
}

// TestSaveKeepsASharedMemoryFileLocked saves a MemoryFile with a second
// owner, as a branch's Kernel shares its family's with its siblings, which
// go on allocating and freeing while it is saved. No other goroutine may take
// the MemoryFile's lock while SaveTo scans it.
func TestSaveKeepsASharedMemoryFileLocked(t *testing.T) {
	mf, _ := newRemoteTestMemoryFile(t, 1)
	// Allocations of alternating kinds, each a segment of its own to scan.
	var frs []memmap.FileRange
	defer func() {
		for _, fr := range frs {
			mf.DecRef(fr)
		}
	}()
	for i := 0; i < 16384; i++ {
		kind := usage.Anonymous
		if i%2 == 1 {
			kind = usage.Tmpfs
		}
		fr, err := mf.Allocate(hostarch.PageSize, AllocOpts{Kind: kind, Mode: AllocateAndWritePopulate})
		if err != nil {
			t.Fatal(err)
		}
		frs = append(frs, fr)
	}
	mf.IncOwner()
	defer mf.Destroy()

	stop := make(chan struct{})
	var sawSaving atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			mf.mu.Lock()
			if mf.isSaving {
				sawSaving.Store(true)
			}
			mf.mu.Unlock()
		}
	}()
	err := mf.SaveTo(context.Background(), io.Discard, &SaveOpts{ExcludeCommittedZeroPages: true})
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if sawSaving.Load() {
		t.Error("another goroutine took the shared MemoryFile's lock while SaveTo scanned it")
	}
}

func TestSaveToPreservesContents(t *testing.T) {
	for _, tc := range []struct {
		name          string
		diskBacked    bool
		exclude       bool
		wantCommitted uint64
	}{
		{name: "memfd", wantCommitted: 2 * uint64(hostarch.PageSize)},
		{name: "memfd excluding committed zero pages", exclude: true, wantCommitted: uint64(hostarch.PageSize)},
		{name: "disk-backed", diskBacked: true, wantCommitted: 2 * uint64(hostarch.PageSize)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSaveTestMemoryFile(t, tc.diskBacked)
			fr := allocateSaveTestPages(t, f)
			pwritePage(t, f, fr, 2, 1)
			pwritePage(t, f, fr, 13, 2)
			if err := f.SaveTo(context.Background(), io.Discard, &SaveOpts{}); err != nil {
				t.Fatalf("first SaveTo: %v", err)
			}
			pwritePage(t, f, fr, 9, 0)
			pwritePage(t, f, fr, 13, 0)
			want := make([]byte, fr.Length())
			want[2*hostarch.PageSize+17] = 1

			var checkpoint bytes.Buffer
			if err := f.SaveTo(context.Background(), &checkpoint, &SaveOpts{ExcludeCommittedZeroPages: tc.exclude}); err != nil {
				t.Fatalf("SaveTo: %v", err)
			}
			if got := f.knownCommittedBytes; got != tc.wantCommitted {
				t.Errorf("knownCommittedBytes: got %d, want %d", got, tc.wantCommitted)
			}

			restored := newSaveTestMemoryFile(t, tc.diskBacked)
			if err := restored.LoadFrom(context.Background(), &checkpoint, &LoadOpts{}); err != nil {
				t.Fatalf("LoadFrom: %v", err)
			}
			t.Cleanup(func() { restored.DecRef(fr) })
			restored.forEachMappingSlice(fr, func(bs []byte) {
				if !bytes.Equal(bs, want) {
					t.Error("restored contents differ from saved contents")
				}
			})
		})
	}
}

func TestAsyncSaveToPreservesContents(t *testing.T) {
	for _, tc := range []struct {
		name          string
		diskBacked    bool
		exclude       bool
		wantCommitted uint64
	}{
		{name: "memfd", wantCommitted: 2 * uint64(hostarch.PageSize)},
		{name: "memfd excluding committed zero pages", exclude: true, wantCommitted: uint64(hostarch.PageSize)},
		{name: "disk-backed", diskBacked: true, wantCommitted: 2 * uint64(hostarch.PageSize)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSaveTestMemoryFile(t, tc.diskBacked)
			fr := allocateSaveTestPages(t, f)
			pwritePage(t, f, fr, 2, 1)
			pwritePage(t, f, fr, 13, 2)

			save := func(exclude bool) ([]byte, []byte) {
				var metadata bytes.Buffer
				var pages bytes.Buffer
				saveDone := make(chan error, 1)
				apfs, err := StartAsyncPagesFileSave(
					stateio.NewIOWriter(&pages, uint64(hostarch.PageSize), 1 /* maxRanges */, 2 /* maxParallel */),
					func(err error) { saveDone <- err },
				)
				if err != nil {
					t.Fatalf("StartAsyncPagesFileSave: %v", err)
				}
				saveErr := f.SaveTo(context.Background(), &metadata, &SaveOpts{
					ExcludeCommittedZeroPages: exclude,
					PagesFile:                 apfs,
				})
				apfs.MemoryFilesDone()
				asyncErr := <-saveDone
				if saveErr != nil {
					t.Fatalf("SaveTo: %v", saveErr)
				}
				if asyncErr != nil {
					t.Fatalf("asynchronous page save: %v", asyncErr)
				}
				if got, want := uint64(pages.Len()), f.knownCommittedBytes; got != want {
					t.Fatalf("pages file length is %d, want %d", got, want)
				}
				return metadata.Bytes(), pages.Bytes()
			}

			save(false)
			pwritePage(t, f, fr, 9, 0)
			pwritePage(t, f, fr, 13, 0)
			want := make([]byte, fr.Length())
			want[2*hostarch.PageSize+17] = 1
			metadata, pages := save(tc.exclude)
			if got := f.knownCommittedBytes; got != tc.wantCommitted {
				t.Errorf("knownCommittedBytes: got %d, want %d", got, tc.wantCommitted)
			}

			restored := newSaveTestMemoryFile(t, tc.diskBacked)
			loadDone := make(chan error, 1)
			apfl, err := StartAsyncPagesFileLoad(
				stateio.NewIOReader(bytes.NewReader(pages), uint64(hostarch.PageSize), 1 /* maxRanges */, 2 /* maxParallel */),
				func(err error) { loadDone <- err },
				nil,
				AsyncPagesFileLoadOpts{},
			)
			if err != nil {
				t.Fatalf("StartAsyncPagesFileLoad: %v", err)
			}
			loadErr := restored.LoadFrom(context.Background(), bytes.NewReader(metadata), &LoadOpts{PagesFile: apfl})
			apfl.MemoryFilesDone()
			asyncErr := <-loadDone
			if loadErr != nil {
				t.Fatalf("LoadFrom: %v", loadErr)
			}
			if asyncErr != nil {
				t.Fatalf("asynchronous page load: %v", asyncErr)
			}
			t.Cleanup(func() { restored.DecRef(fr) })
			restored.forEachMappingSlice(fr, func(bs []byte) {
				if !bytes.Equal(bs, want) {
					t.Error("restored contents differ from saved contents")
				}
			})
		})
	}
}

// TestSaveToExitAfterSaving checks that SaveTo decommits zero pages that are
// committed on the host even if the sentry exits after saving. Upstream keeps
// them in place then, but only when it skips the host file's holes while
// scanning; TTC's scan reads every page, which commits the holes, so keeping
// zero pages would raise memory usage until the sentry exits.
func TestSaveToExitAfterSaving(t *testing.T) {
	for _, tc := range []struct {
		name            string
		exitAfterSaving bool
		wantDecommitted bool
	}{
		{name: "resume", wantDecommitted: true},
		{name: "exit after saving", exitAfterSaving: true, wantDecommitted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSaveTestMemoryFile(t, false)
			fr := allocateSaveTestPages(t, f)
			pwritePage(t, f, fr, 2, 1)
			pwritePage(t, f, fr, 5, 0)
			if err := f.SaveTo(context.Background(), io.Discard, &SaveOpts{ExitAfterSaving: tc.exitAfterSaving}); err != nil {
				t.Fatalf("SaveTo: %v", err)
			}
			if got, want := f.knownCommittedBytes, uint64(hostarch.PageSize); got != want {
				t.Errorf("knownCommittedBytes: got %d, want %d", got, want)
			}
			if runtime.GOOS != "linux" {
				// Only Linux's MemoryFile punches holes in its host file.
				return
			}
			off := int64(fr.Start) + 5*int64(hostarch.PageSize)
			data, err := unix.Seek(f.FD(), off, unix.SEEK_DATA)
			if err != nil && err != unix.ENXIO {
				t.Fatalf("lseek(SEEK_DATA): %v", err)
			}
			if decommitted := err == unix.ENXIO || data != off; decommitted != tc.wantDecommitted {
				t.Errorf("zero page decommitted: got %t, want %t", decommitted, tc.wantDecommitted)
			}
		})
	}
}

// BenchmarkSaveToCommittedZeroPages measures SaveTo for a 256 MiB MemoryFile
// whose pages are all committed, with one of every 16 pages nonzero.
func BenchmarkSaveToCommittedZeroPages(b *testing.B) {
	const size = 256 << 20
	for _, bc := range []struct {
		name            string
		exitAfterSaving bool
	}{
		{name: "resume"},
		{name: "exit-after-saving", exitAfterSaving: true},
	} {
		b.Run(bc.name, func(b *testing.B) {
			b.SetBytes(size)
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				fd, err := memutil.CreateMemFD("pgalloc-save-bench", 0)
				if err != nil {
					b.Fatalf("CreateMemFD: %v", err)
				}
				f, err := NewMemoryFile(os.NewFile(uintptr(fd), "pgalloc-save-bench"), MemoryFileOpts{
					DelayedEviction:         DelayedEvictionDisabled,
					DisableMemoryAccounting: true,
				})
				if err != nil {
					b.Fatalf("NewMemoryFile: %v", err)
				}
				fr, err := f.Allocate(size, AllocOpts{Mode: AllocateUncommitted})
				if err != nil {
					b.Fatalf("Allocate: %v", err)
				}
				page := 0
				f.forEachMappingSlice(fr, func(bs []byte) {
					for off := 0; off < len(bs); off += hostarch.PageSize {
						if page%16 == 0 {
							bs[off] = 1
						} else {
							bs[off] = 0
						}
						page++
					}
				})
				b.StartTimer()
				if err := f.SaveTo(context.Background(), io.Discard, &SaveOpts{ExitAfterSaving: bc.exitAfterSaving}); err != nil {
					b.Fatalf("SaveTo: %v", err)
				}
				b.StopTimer()
				f.DecRef(fr)
				f.Destroy()
			}
		})
	}
}
