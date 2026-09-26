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

package vmplatform

import (
	"errors"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
)

type stagingTestReservation struct {
	raw []byte
}

type stagingTestHost struct {
	reservations map[uintptr]stagingTestReservation
}

func newStagingTestHost() *stagingTestHost {
	return &stagingTestHost{reservations: make(map[uintptr]stagingTestReservation)}
}

func (h *stagingTestHost) Reserve(length uint64) (uintptr, error) {
	raw, err := unix.Mmap(-1, 0, int(length+HostSlabSize), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANON|unix.MAP_PRIVATE)
	if err != nil {
		return 0, err
	}
	base := uintptr(unsafe.Pointer(&raw[0]))
	aligned := (base + uintptr(HostSlabSize-1)) &^ uintptr(HostSlabSize-1)
	h.reservations[aligned] = stagingTestReservation{raw: raw}
	return aligned, nil
}

func (*stagingTestHost) Commit(uintptr, uint64) error { return nil }

func (*stagingTestHost) Decommit(address uintptr, length uint64) error {
	clear(unsafe.Slice((*byte)(unsafe.Pointer(address)), int(length)))
	return nil
}

func (*stagingTestHost) Zero(address uintptr, length uint64) error {
	clear(unsafe.Slice((*byte)(unsafe.Pointer(address)), int(length)))
	return nil
}

func (*stagingTestHost) Copy(dst, src uintptr, length uint64) error {
	copy(unsafe.Slice((*byte)(unsafe.Pointer(dst)), int(length)), unsafe.Slice((*byte)(unsafe.Pointer(src)), int(length)))
	return nil
}

func (h *stagingTestHost) Release(address uintptr, _ uint64) error {
	reservation := h.reservations[address]
	delete(h.reservations, address)
	return unix.Munmap(reservation.raw)
}

type stagingTestFile struct {
	memmap.DefaultMemoryType
	memmap.NoBufferedIOFallback
	data     []byte
	writeErr error
	// readErr fails reads of pages at or after readErrAt.
	readErr   error
	readErrAt uint64
}

func (*stagingTestFile) IncRef(memmap.FileRange, uint32) {}
func (*stagingTestFile) DecRef(memmap.FileRange)         {}
func (*stagingTestFile) DataFD(memmap.FileRange) (int, error) {
	return -1, errors.New("no data fd")
}
func (*stagingTestFile) FD() int { return -1 }

func (f *stagingTestFile) MapInternal(fr memmap.FileRange, at hostarch.AccessType) (safemem.BlockSeq, error) {
	if at.Write && f.writeErr != nil {
		return safemem.BlockSeq{}, f.writeErr
	}
	if !at.Write && f.readErr != nil && fr.Start >= f.readErrAt {
		return safemem.BlockSeq{}, f.readErr
	}
	return safemem.BlockSeqOf(safemem.BlockFromSafeSlice(f.data[fr.Start:fr.End])), nil
}

func newStagingTestCache(t *testing.T) (*GuestRAM, *stagingCache) {
	t.Helper()
	ram, err := NewGuestRAM(GuestRAMConfig{
		VM:       &ramTestVM{},
		Host:     newStagingTestHost(),
		IPAStart: 0x100000,
		IPASize:  1 << 30,
	})
	if err != nil {
		t.Fatalf("NewGuestRAM: %v", err)
	}
	cache, err := newStagingCache(ram)
	if err != nil {
		t.Fatalf("newStagingCache: %v", err)
	}
	return ram, cache
}

func TestStagingSharesFrameAndWritesBackOnLastUnmap(t *testing.T) {
	ram, cache := newStagingTestCache(t)
	file := &stagingTestFile{data: make([]byte, hostarch.PageSize)}
	file.data[0] = 0x11
	fr := memmap.FileRange{Start: 0, End: hostarch.PageSize}

	firstFrames, firstPages, err := cache.resolve(file, fr, hostarch.Read)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.commitMapping(firstPages[0]); err != nil {
		t.Fatal(err)
	}
	secondFrames, secondPages, err := cache.resolve(file, fr, hostarch.ReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.commitMapping(secondPages[0]); err != nil {
		t.Fatal(err)
	}
	if firstFrames[0].ID != secondFrames[0].ID || firstPages[0] != secondPages[0] {
		t.Fatalf("shared file page received different staging identities: first=%+v second=%+v", firstFrames[0], secondFrames[0])
	}
	staged := unsafe.Slice((*byte)(unsafe.Pointer(secondFrames[0].HostAddress)), int(hostarch.PageSize))
	if staged[0] != 0x11 {
		t.Fatalf("staged byte = %#x, want %#x", staged[0], 0x11)
	}
	staged[0] = 0x7a

	if err := cache.removeMapping(firstPages[0], false); err != nil {
		t.Fatal(err)
	}
	if file.data[0] != 0x11 {
		t.Fatalf("writeback occurred before last mapping: got %#x", file.data[0])
	}
	if err := cache.removeMapping(secondPages[0], true); err != nil {
		t.Fatal(err)
	}
	if file.data[0] != 0x7a {
		t.Fatalf("last-unmap writeback = %#x, want %#x", file.data[0], 0x7a)
	}
	metrics := ram.Metrics()
	if metrics.LogicalBytes != 0 || metrics.HostResidentBytes != 0 {
		t.Fatalf("staging page remained resident after last unmap: %+v", metrics)
	}
}

func TestStagingPreservesAndRetriesWritebackError(t *testing.T) {
	_, cache := newStagingTestCache(t)
	wantErr := errors.New("injected writeback failure")
	file := &stagingTestFile{data: make([]byte, hostarch.PageSize), writeErr: wantErr}
	fr := memmap.FileRange{Start: 0, End: hostarch.PageSize}
	frames, pages, err := cache.resolve(file, fr, hostarch.ReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.commitMapping(pages[0]); err != nil {
		t.Fatal(err)
	}
	unsafe.Slice((*byte)(unsafe.Pointer(frames[0].HostAddress)), int(hostarch.PageSize))[0] = 0x55
	if err := cache.removeMapping(pages[0], true); !errors.Is(err, wantErr) {
		t.Fatalf("removeMapping error = %v, want %v", err, wantErr)
	}
	file.writeErr = nil
	if err := cache.flushAll(); err != nil {
		t.Fatalf("flushAll retry: %v", err)
	}
	if file.data[0] != 0x55 {
		t.Fatalf("retried writeback = %#x, want %#x", file.data[0], 0x55)
	}
}

func TestStagingReservationPreventsResolveUnmapRace(t *testing.T) {
	ram, cache := newStagingTestCache(t)
	file := &stagingTestFile{data: make([]byte, hostarch.PageSize)}
	fr := memmap.FileRange{Start: 0, End: hostarch.PageSize}
	_, firstPages, err := cache.resolve(file, fr, hostarch.Read)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.commitMapping(firstPages[0]); err != nil {
		t.Fatal(err)
	}
	_, secondPages, err := cache.resolve(file, fr, hostarch.Read)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.removeMapping(firstPages[0], false); err != nil {
		t.Fatal(err)
	}
	if got := ram.Metrics().LogicalBytes; got != hostarch.PageSize {
		t.Fatalf("reserved staging page was reclaimed: logical bytes = %d, want %d", got, hostarch.PageSize)
	}
	if err := cache.commitMapping(secondPages[0]); err != nil {
		t.Fatal(err)
	}
	if err := cache.removeMapping(secondPages[0], false); err != nil {
		t.Fatal(err)
	}
}

type stagingUnlockedDecommitBacking struct {
	pgalloc.Backing
	cache    *stagingCache
	observed bool
}

func (b *stagingUnlockedDecommitBacking) Decommit(fr memmap.FileRange) error {
	if !b.cache.mu.TryLock() {
		return errors.New("staging cache mutex held during GuestRAM decommit")
	}
	b.observed = true
	b.cache.mu.Unlock() // +checklocksforce: TryLock succeeded.
	return b.Backing.Decommit(fr)
}

func TestStagingDecommitDoesNotHoldCacheMutex(t *testing.T) {
	_, cache := newStagingTestCache(t)
	probe := &stagingUnlockedDecommitBacking{Backing: cache.backing, cache: cache}
	cache.backing = probe
	file := &stagingTestFile{data: make([]byte, hostarch.PageSize)}
	fr := memmap.FileRange{Start: 0, End: hostarch.PageSize}
	_, pages, err := cache.resolve(file, fr, hostarch.Read)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.commitMapping(pages[0]); err != nil {
		t.Fatal(err)
	}
	if err := cache.removeMapping(pages[0], false); err != nil {
		t.Fatal(err)
	}
	if !probe.observed {
		t.Fatal("staging page was not decommitted")
	}
}

// TestStagingWriteAfterFlushReachesFile checks that a flush does not clean a
// page that is still mapped writable. The guest keeps writing through its PTE
// without faulting, so the last unmap must write those bytes back too.
func TestStagingWriteAfterFlushReachesFile(t *testing.T) {
	_, cache := newStagingTestCache(t)
	file := &stagingTestFile{data: make([]byte, hostarch.PageSize)}
	fr := memmap.FileRange{Start: 0, End: hostarch.PageSize}
	frames, pages, err := cache.resolve(file, fr, hostarch.ReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.commitMapping(pages[0]); err != nil {
		t.Fatal(err)
	}
	staged := unsafe.Slice((*byte)(unsafe.Pointer(frames[0].HostAddress)), int(hostarch.PageSize))
	staged[0] = 0x01
	if err := cache.flushPages(pages); err != nil {
		t.Fatal(err)
	}
	if file.data[0] != 0x01 {
		t.Fatalf("flushed byte = %#x, want %#x", file.data[0], 0x01)
	}
	staged[0] = 0x02
	if err := cache.removeMapping(pages[0], true); err != nil {
		t.Fatal(err)
	}
	if file.data[0] != 0x02 {
		t.Fatalf("write after flush was lost: file byte = %#x, want %#x", file.data[0], 0x02)
	}
}

// TestStagingReadOnlyRemapCleansAfterFlush checks that a mapping downgraded
// to read-only no longer keeps its page dirty.
func TestStagingReadOnlyRemapCleansAfterFlush(t *testing.T) {
	_, cache := newStagingTestCache(t)
	file := &stagingTestFile{data: make([]byte, hostarch.PageSize)}
	fr := memmap.FileRange{Start: 0, End: hostarch.PageSize}
	_, pages, err := cache.resolve(file, fr, hostarch.ReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.commitMapping(pages[0]); err != nil {
		t.Fatal(err)
	}
	if err := cache.setMappingWritable(pages[0], true, false); err != nil {
		t.Fatal(err)
	}
	if err := cache.flushPages(pages); err != nil {
		t.Fatal(err)
	}
	if pages[0].dirty {
		t.Fatal("page with only read-only mappings stayed dirty after writeback")
	}
	if err := cache.removeMapping(pages[0], false); err != nil {
		t.Fatal(err)
	}
}

// TestStagingResolveFailureReleasesEarlierPages checks that a range which
// fails partway leaves no reservation behind: MapFile never sees the pages
// reserved before the failure.
func TestStagingResolveFailureReleasesEarlierPages(t *testing.T) {
	ram, cache := newStagingTestCache(t)
	wantErr := errors.New("read failed")
	file := &stagingTestFile{data: make([]byte, 2*hostarch.PageSize), readErr: wantErr, readErrAt: hostarch.PageSize}
	_, _, err := cache.resolve(file, memmap.FileRange{Start: 0, End: 2 * hostarch.PageSize}, hostarch.Read)
	if !errors.Is(err, wantErr) {
		t.Fatalf("resolve error = %v, want %v", err, wantErr)
	}
	cache.mu.Lock()
	cached := len(cache.pages)
	cache.mu.Unlock()
	if cached != 0 {
		t.Fatalf("%d staged pages remain after a failed resolve", cached)
	}
	if metrics := ram.Metrics(); metrics.LogicalBytes != 0 {
		t.Fatalf("staging frames remain resident after a failed resolve: %+v", metrics)
	}
}
