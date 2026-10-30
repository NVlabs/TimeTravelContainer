// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package fsutil

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/fdbudget"
	"gvisor.dev/gvisor/pkg/sentry/hostmm"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
)

// newReopenableFile returns an MmapCachedFile of a new file of size bytes,
// whose descriptor the budget may close, holding a reference on all of it,
// the number of times it has been opened again, and a function that releases
// it, which the test's cleanup calls if the test does not.
func newReopenableFile(t *testing.T, size int) (*MmapCachedFile, *int, func()) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "f")
	content := make([]byte, size)
	for i := range content {
		content[i] = byte(i / hostarch.PageSize)
	}
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	open := func() (int, error) { return unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC, 0) }
	fd, err := open()
	if err != nil {
		t.Fatal(err)
	}
	f := &MmapCachedFile{}
	f.SetFD(fd)
	reopens := new(int)
	f.SetReopen(func() (int, error) {
		*reopens++
		return open()
	})
	// References without memory accounting, as an application mapping of
	// the whole file holds.
	ar := hostarch.AddrRange{0, hostarch.Addr(size)}
	f.AddMapping(ar, 0)
	released := false
	release := func() {
		if !released {
			released = true
			f.MappableRelease()
			f.RemoveMapping(ar, 0)
		}
	}
	t.Cleanup(release)
	return f, reopens, release
}

// evictAll has the budget close every idle descriptor it holds.
func evictAll() {
	for fdbudget.Retry(unix.EMFILE) {
	}
}

func readChunk(t *testing.T, f *MmapCachedFile, page uint64) byte {
	t.Helper()
	fr := memmap.FileRange{page * hostarch.PageSize, (page + 1) * hostarch.PageSize}
	bs, err := f.MapInternal(fr, hostarch.Read)
	if err != nil {
		t.Fatalf("MapInternal(%v): %v", fr, err)
	}
	var b [1]byte
	if _, err := safemem.CopySeq(safemem.BlockSeqOf(safemem.BlockFromSafeSlice(b[:])), bs); err != nil {
		t.Fatal(err)
	}
	return b[0]
}

// The budget closes the descriptor of an idle file, which maps chunks and
// gives out descriptors again by opening it again.
func TestMmapCachedFileReopensClosedDescriptor(t *testing.T) {
	size := 2 * int(chunkSize())
	f, reopens, _ := newReopenableFile(t, size)
	evictAll()
	if fd := f.FD(); fd >= 0 {
		t.Fatalf("the budget left the idle file's descriptor %d open", fd)
	}
	// Mapping a chunk opens the file again.
	if got, want := readChunk(t, f, 1), byte(1); got != want {
		t.Errorf("page 1 reads %d, want %d", got, want)
	}
	if *reopens != 1 {
		t.Errorf("opened again %d times, want 1", *reopens)
	}
	// A chunk that is mapped needs no descriptor.
	evictAll()
	if got, want := readChunk(t, f, 2), byte(2); got != want {
		t.Errorf("page 2 reads %d, want %d", got, want)
	}
	if *reopens != 1 {
		t.Errorf("opened again %d times for a mapped chunk, want 1", *reopens)
	}
	// So does the second chunk, which opens it again.
	page := uint64(chunkSize()/hostarch.PageSize) + 3
	if got, want := readChunk(t, f, page), byte(page); got != want {
		t.Errorf("page %d reads %d, want %d", page, got, want)
	}
	if *reopens != 2 {
		t.Errorf("opened again %d times, want 2", *reopens)
	}
	// A pinned descriptor stays open, and maps what the file holds.
	evictAll()
	fd, err := f.PinDataFD(memmap.FileRange{0, hostarch.PageSize})
	if err != nil {
		t.Fatalf("PinDataFD: %v", err)
	}
	evictAll()
	buf := make([]byte, hostarch.PageSize)
	if _, err := unix.Pread(fd, buf, int64(4*hostarch.PageSize)); err != nil {
		t.Errorf("reading the pinned descriptor: %v", err)
	} else if !bytes.Equal(buf, bytes.Repeat([]byte{4}, hostarch.PageSize)) {
		t.Errorf("the pinned descriptor reads another file")
	}
	f.UnpinDataFD()
	if *reopens != 3 {
		t.Errorf("opened again %d times, want 3", *reopens)
	}
	if evicted, reopened := fdbudget.Stats(); evicted == 0 || reopened < 3 {
		t.Errorf("the budget counts %d closed and %d reopened", evicted, reopened)
	}
}

// A file released while the budget has closed its descriptor is not opened
// again.
func TestMmapCachedFileReleasedWhileClosed(t *testing.T) {
	f, reopens, release := newReopenableFile(t, int(chunkSize()))
	evictAll()
	release()
	if _, err := f.DataFD(memmap.FileRange{0, hostarch.PageSize}); err == nil {
		t.Errorf("a released file gave out a descriptor")
	}
	if *reopens != 0 {
		t.Errorf("a released file was opened again")
	}
	// Nothing of it remains for the budget to close.
	evictAll()
}

// Past the budget of chunk mappings, MapInternal declines to map more chunks
// of files, and buffered I/O reads and writes what it would have mapped.
func TestMmapCachedFileChunkBudget(t *testing.T) {
	BoundChunkMappings()
	chunkMappingBudget()
	before := cachedChunks.Load()
	chunkBudget.n = before + 1
	t.Cleanup(func() {
		chunkBudget.bounded.Store(false)
		chunkBudget.n = int64(hostmm.MaxMapCount() / 4)
	})
	f, _, release := newReopenableFile(t, 2*int(chunkSize()))
	if got := readChunk(t, f, 0); got != 0 {
		t.Errorf("page 0 reads %d, want 0", got)
	}
	page := uint64(chunkSize()/hostarch.PageSize) + 5
	fr := memmap.FileRange{page * hostarch.PageSize, (page + 1) * hostarch.PageSize}
	if _, err := f.MapInternal(fr, hostarch.Read); err != (memmap.BufferedIOFallbackErr{}) {
		t.Fatalf("MapInternal of a chunk past the budget: %v, want BufferedIOFallbackErr", err)
	}
	buf := make([]byte, hostarch.PageSize)
	if n, err := f.BufferReadAt(fr.Start, buf); err != nil || n != hostarch.PageSize || !bytes.Equal(buf, bytes.Repeat([]byte{byte(page)}, hostarch.PageSize)) {
		t.Errorf("BufferReadAt: %d bytes, %v; want the page's", n, err)
	}
	if _, err := f.BufferWriteAt(fr.Start+7, []byte{0xee}); err != nil {
		t.Errorf("BufferWriteAt: %v", err)
	}
	if _, err := f.BufferReadAt(fr.Start+7, buf[:1]); err != nil || buf[0] != 0xee {
		t.Errorf("BufferReadAt after BufferWriteAt reads %#x (%v), want 0xee", buf[0], err)
	}
	// Past the file's end, buffered I/O faults, as a mapping would.
	if _, err := f.BufferReadAt(2*chunkSize()-1, buf[:2]); err == nil {
		t.Errorf("BufferReadAt past the end of the file succeeded")
	}
	release()
	if after := cachedChunks.Load(); after != before {
		t.Errorf("%d chunks mapped after the file's release, want %d", after, before)
	}
}

// Buffered I/O of a file whose end is not at a page boundary reads and writes
// what a mapping of it would: zeroes past the end in the page that holds it,
// writes there that do not extend the file, and faults past that page.
func TestMmapCachedFileBufferedEOF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	const size = hostarch.PageSize + 100
	if err := os.WriteFile(path, bytes.Repeat([]byte{0xab}, size), 0600); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	f := &MmapCachedFile{}
	f.SetFD(fd)
	defer f.MappableRelease()
	buf := make([]byte, hostarch.PageSize)
	if n, err := f.BufferReadAt(hostarch.PageSize, buf); err != nil || n != hostarch.PageSize {
		t.Fatalf("BufferReadAt of the last page: %d bytes, %v", n, err)
	}
	if !bytes.Equal(buf[:100], bytes.Repeat([]byte{0xab}, 100)) || !bytes.Equal(buf[100:], make([]byte, hostarch.PageSize-100)) {
		t.Errorf("the last page reads other than the file's 100 bytes and zeroes")
	}
	if _, err := f.BufferReadAt(2*hostarch.PageSize, buf[:1]); err == nil {
		t.Errorf("BufferReadAt past the last page succeeded")
	}
	if n, err := f.BufferWriteAt(hostarch.PageSize+90, bytes.Repeat([]byte{0xcd}, 20)); err != nil || n != 20 {
		t.Errorf("BufferWriteAt across the end of the file: %d bytes, %v", n, err)
	}
	if data, err := os.ReadFile(path); err != nil || len(data) != size || data[size-1] != 0xcd || data[size-11] != 0xab {
		t.Errorf("after a write across its end, the file is %d bytes (%v), want %d with the write's first 10", len(data), err, size)
	}
	if _, err := f.BufferWriteAt(2*hostarch.PageSize-1, []byte{1, 2}); err == nil {
		t.Errorf("BufferWriteAt past the last page succeeded")
	}
}
