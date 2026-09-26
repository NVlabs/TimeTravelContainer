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
	"fmt"
	"io"
	"reflect"
	"sync"
	"unsafe"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
)

// stagingCache gives host-file-backed PMAs a coherent GuestRAM data plane.
// Entries are shared by file identity and 4 KiB offset for as long as any
// address space maps them. Writable entries are conservatively dirty.
type stagingCache struct {
	mu sync.Mutex
	// cond is signaled when a page finishes decommitting. Decommit must not
	// hold mu because GuestRAM's last-line frame revocation may acquire address
	// space locks, whose mapping paths acquire mu in the opposite order.
	cond *sync.Cond

	backing pgalloc.Backing
	size    uint64
	next    uint64
	free    []uint64
	pages   map[stagingKey]*stagedPage
}

type stagingKey struct {
	typeOf reflect.Type
	ptr    uintptr
	offset uint64
}

type stagedPage struct {
	key           stagingKey
	file          memmap.File
	backingOffset uint64
	frame         pgalloc.FrameID
	// reservations counts resolve results that MapFile has not yet either
	// installed or abandoned. It closes the lifetime gap between resolving a
	// shared page and recording its reverse mapping.
	reservations uint64
	mappings     uint64
	// writable counts the reservations and mappings that permit writes. A
	// writeback cleans the page only when none remain: the guest can keep
	// writing through a writable PTE without faulting.
	writable  uint64
	dirty     bool
	releasing bool
}

func newStagingCache(ram *GuestRAM) (*stagingCache, error) {
	backing, err := ram.NewBacking()
	if err != nil {
		return nil, fmt.Errorf("creating VM staging backing: %w", err)
	}
	cache := &stagingCache{
		backing: backing,
		pages:   make(map[stagingKey]*stagedPage),
	}
	cache.cond = sync.NewCond(&cache.mu)
	return cache, nil
}

func stagingFileKey(file memmap.File, offset uint64) (stagingKey, error) {
	v := reflect.ValueOf(file)
	if !v.IsValid() || v.Kind() != reflect.Pointer || v.IsNil() {
		return stagingKey{}, fmt.Errorf("vmplatform cannot identify staging file %T", file)
	}
	return stagingKey{typeOf: v.Type(), ptr: v.Pointer(), offset: offset}, nil
}

func (s *stagingCache) resolve(file memmap.File, fr memmap.FileRange, at hostarch.AccessType) ([]pgalloc.BackingFrame, []*stagedPage, error) {
	if err := validateFrameRange(fr); err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	frames := make([]pgalloc.BackingFrame, 0, fr.Length()/hostarch.PageSize)
	pages := make([]*stagedPage, 0, cap(frames))
	for sourceOffset := fr.Start; sourceOffset < fr.End; sourceOffset += hostarch.PageSize {
		key, err := stagingFileKey(file, sourceOffset)
		if err != nil {
			return nil, nil, err
		}
		page := s.pages[key]
		for page != nil && page.releasing {
			s.cond.Wait()
			page = s.pages[key]
		}
		if page == nil {
			backingOffset, err := s.allocateOffsetLocked()
			if err != nil {
				return nil, nil, err
			}
			resolved, frameLease, err := s.backing.AcquireMapping(memmap.FileRange{Start: backingOffset, End: backingOffset + hostarch.PageSize}, hostarch.Write)
			if err != nil {
				s.free = append(s.free, backingOffset)
				return nil, nil, fmt.Errorf("allocating VM staging frame: %w", err)
			}
			if len(resolved) != 1 {
				frameLease.Release()
				return nil, nil, fmt.Errorf("VM staging backing returned %d frames for one page", len(resolved))
			}
			if err := copyFileToFrame(file, sourceOffset, resolved[0]); err != nil {
				frameLease.Release()
				_ = s.backing.Decommit(memmap.FileRange{Start: backingOffset, End: backingOffset + hostarch.PageSize})
				s.free = append(s.free, backingOffset)
				return nil, nil, fmt.Errorf("staging %T offset %#x: %w", file, sourceOffset, err)
			}
			frameLease.Release()
			page = &stagedPage{
				key:           key,
				file:          file,
				backingOffset: backingOffset,
				frame:         resolved[0].ID,
			}
			s.pages[key] = page
		}
		resolved, err := s.backing.ResolveFrames(memmap.FileRange{Start: page.backingOffset, End: page.backingOffset + hostarch.PageSize}, at)
		if err != nil {
			return nil, nil, err
		}
		if len(resolved) != 1 || resolved[0].ID != page.frame {
			return nil, nil, fmt.Errorf("VM staging frame identity changed for %T offset %#x", file, sourceOffset)
		}
		if at.Write {
			page.dirty = true
			page.writable++
		}
		page.reservations++
		frames = append(frames, resolved[0])
		pages = append(pages, page)
	}
	return frames, pages, nil
}

func (s *stagingCache) allocateOffsetLocked() (uint64, error) {
	if n := len(s.free); n != 0 {
		offset := s.free[n-1]
		s.free = s.free[:n-1]
		return offset, nil
	}
	offset := s.next
	needed := offset + hostarch.PageSize
	for s.size < needed {
		newSize := s.size + pgalloc.BackingChunkSize
		if _, err := s.backing.Grow(s.size, newSize, false); err != nil {
			return 0, fmt.Errorf("growing VM staging backing: %w", err)
		}
		s.size = newSize
	}
	s.next = needed
	return offset, nil
}

// commitMapping converts a reservation returned by resolve into an installed
// address-space mapping.
func (s *stagingCache) commitMapping(page *stagedPage) error {
	if page == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if page.releasing || s.pages[page.key] != page {
		return fmt.Errorf("VM staging page disappeared before mapping %T offset %#x", page.file, page.key.offset)
	}
	if page.reservations == 0 {
		return fmt.Errorf("VM staging reservation underflow for %T offset %#x", page.file, page.key.offset)
	}
	page.reservations--
	page.mappings++
	return nil
}

// cancelReservations abandons unresolved MapFile results, which resolve
// returned with writable access if writable is set. Pages with neither
// mappings nor other reservations are reclaimed just like a last unmap.
func (s *stagingCache) cancelReservations(pages []*stagedPage, writable bool) error {
	var retErr error
	for _, page := range pages {
		if page != nil {
			retErr = errors.Join(retErr, s.dropReference(page, true, writable))
		}
	}
	return retErr
}

// removeMapping writes a dirty page back before releasing its last mapping.
// The entry is then discarded so a later translation observes invalidation or
// truncation performed by the backing file. writable reports whether the
// mapping permitted writes.
func (s *stagingCache) removeMapping(page *stagedPage, writable bool) error {
	if page == nil {
		return nil
	}
	return s.dropReference(page, false, writable)
}

// setMappingWritable records that an installed mapping of page changed
// whether it permits writes.
func (s *stagingCache) setMappingWritable(page *stagedPage, was, now bool) error {
	if page == nil || was == now {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if now {
		page.writable++
		page.dirty = true
		return nil
	}
	if page.writable == 0 {
		return fmt.Errorf("VM staging writable mapping underflow for %T offset %#x", page.file, page.key.offset)
	}
	page.writable--
	return nil
}

// dropReference drops either a provisional reservation or an installed
// mapping. The page remains discoverable but marked releasing while decommit
// runs, so a concurrent resolve cannot reuse its frame or backing offset.
func (s *stagingCache) dropReference(page *stagedPage, reservation, writable bool) error {
	s.mu.Lock()
	for page.releasing {
		s.cond.Wait()
	}
	if s.pages[page.key] != page {
		s.mu.Unlock()
		return fmt.Errorf("VM staging page missing while dropping %T offset %#x", page.file, page.key.offset)
	}
	if reservation {
		if page.reservations == 0 {
			s.mu.Unlock()
			return fmt.Errorf("VM staging reservation underflow for %T offset %#x", page.file, page.key.offset)
		}
		page.reservations--
	} else {
		if page.mappings == 0 {
			s.mu.Unlock()
			return fmt.Errorf("VM staging mapping underflow for %T offset %#x", page.file, page.key.offset)
		}
		page.mappings--
	}
	if writable {
		if page.writable == 0 {
			s.mu.Unlock()
			return fmt.Errorf("VM staging writable reference underflow for %T offset %#x", page.file, page.key.offset)
		}
		page.writable--
	}
	if page.mappings != 0 || page.reservations != 0 {
		s.mu.Unlock()
		return nil
	}
	if err := s.flushPageLocked(page); err != nil {
		// Keep the page cached and dirty. A later synchronous coherence point
		// reports the error and retries the writeback.
		s.mu.Unlock()
		return err
	}
	page.releasing = true
	s.mu.Unlock()

	err := s.backing.Decommit(memmap.FileRange{Start: page.backingOffset, End: page.backingOffset + hostarch.PageSize})

	s.mu.Lock()
	page.releasing = false
	if err == nil {
		delete(s.pages, page.key)
		s.free = append(s.free, page.backingOffset)
	}
	s.cond.Broadcast()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return nil
}

func (s *stagingCache) flushPages(pages []*stagedPage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := make(map[*stagedPage]struct{}, len(pages))
	var retErr error
	for _, page := range pages {
		if page == nil {
			continue
		}
		for page.releasing {
			s.cond.Wait()
		}
		if s.pages[page.key] != page {
			continue
		}
		if _, ok := seen[page]; ok {
			continue
		}
		seen[page] = struct{}{}
		retErr = errors.Join(retErr, s.flushPageLocked(page))
	}
	return retErr
}

func (s *stagingCache) flushAll() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		waited := false
		for _, page := range s.pages {
			if page.releasing {
				s.cond.Wait()
				waited = true
				break
			}
		}
		if !waited {
			break
		}
	}
	var retErr error
	for _, page := range s.pages {
		retErr = errors.Join(retErr, s.flushPageLocked(page))
	}
	return retErr
}

func (s *stagingCache) flushPageLocked(page *stagedPage) error {
	if !page.dirty {
		return nil
	}
	resolved, frameLease, err := s.backing.AcquireMapping(memmap.FileRange{Start: page.backingOffset, End: page.backingOffset + hostarch.PageSize}, hostarch.Read)
	if err != nil {
		return err
	}
	defer frameLease.Release()
	if len(resolved) != 1 {
		return fmt.Errorf("VM staging backing returned %d frames during writeback", len(resolved))
	}
	if err := copyFrameToFile(resolved[0], page.file, page.key.offset); err != nil {
		return fmt.Errorf("writing back %T offset %#x: %w", page.file, page.key.offset, err)
	}
	// Writes through a remaining writable mapping do not fault, so the page
	// stays dirty until the last one goes away.
	page.dirty = page.writable != 0
	return nil
}

func copyFileToFrame(file memmap.File, offset uint64, frame pgalloc.BackingFrame) error {
	fr := memmap.FileRange{Start: offset, End: offset + hostarch.PageSize}
	src, mappingLease, err := memmap.MapInternalWithLease(file, fr, hostarch.Read)
	if err != nil {
		if _, ok := err.(memmap.BufferedIOFallbackErr); !ok {
			return err
		}
		buf := make([]byte, hostarch.PageSize)
		n, readErr := file.BufferReadAt(offset, buf)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		if n != hostarch.PageSize {
			return io.ErrUnexpectedEOF
		}
		src = safemem.BlockSeqOf(safemem.BlockFromSafeSlice(buf))
	}
	if mappingLease != nil {
		defer mappingLease.Release()
	}
	dst := safemem.BlockSeqOf(safemem.BlockFromSafePointer(unsafe.Pointer(frame.HostAddress), int(hostarch.PageSize)))
	n, err := safemem.CopySeq(dst, src)
	if err != nil {
		return err
	}
	if n != hostarch.PageSize {
		return io.ErrUnexpectedEOF
	}
	return nil
}

func copyFrameToFile(frame pgalloc.BackingFrame, file memmap.File, offset uint64) error {
	src := safemem.BlockSeqOf(safemem.BlockFromSafePointer(unsafe.Pointer(frame.HostAddress), int(hostarch.PageSize)))
	fr := memmap.FileRange{Start: offset, End: offset + hostarch.PageSize}
	dst, mappingLease, err := memmap.MapInternalWithLease(file, fr, hostarch.Write)
	if err != nil {
		if _, ok := err.(memmap.BufferedIOFallbackErr); !ok {
			return err
		}
		buf := make([]byte, hostarch.PageSize)
		n, copyErr := safemem.CopySeq(safemem.BlockSeqOf(safemem.BlockFromSafeSlice(buf)), src)
		if copyErr != nil {
			return copyErr
		}
		if n != hostarch.PageSize {
			return io.ErrUnexpectedEOF
		}
		written, writeErr := file.BufferWriteAt(offset, buf)
		if writeErr != nil {
			return writeErr
		}
		if written != hostarch.PageSize {
			return io.ErrShortWrite
		}
		return nil
	}
	if mappingLease != nil {
		defer mappingLease.Release()
	}
	n, err := safemem.CopySeq(dst, src)
	if err != nil {
		return err
	}
	if n != hostarch.PageSize {
		return io.ErrShortWrite
	}
	return nil
}
