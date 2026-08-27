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
	mappings      uint64
	dirty         bool
}

func newStagingCache(ram *GuestRAM) (*stagingCache, error) {
	backing, err := ram.NewBacking()
	if err != nil {
		return nil, fmt.Errorf("creating VM staging backing: %w", err)
	}
	return &stagingCache{
		backing: backing,
		pages:   make(map[stagingKey]*stagedPage),
	}, nil
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
		}
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

func (s *stagingCache) addMapping(page *stagedPage) {
	if page == nil {
		return
	}
	s.mu.Lock()
	page.mappings++
	s.mu.Unlock()
}

// removeMapping writes a dirty page back before releasing its last mapping.
// The entry is then discarded so a later translation observes invalidation or
// truncation performed by the backing file.
func (s *stagingCache) removeMapping(page *stagedPage) error {
	if page == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if page.mappings == 0 {
		return fmt.Errorf("VM staging mapping underflow for %T offset %#x", page.file, page.key.offset)
	}
	page.mappings--
	if page.mappings != 0 {
		return nil
	}
	if err := s.flushPageLocked(page); err != nil {
		// Keep the page cached and dirty. A later synchronous coherence point
		// reports the error and retries the writeback.
		return err
	}
	delete(s.pages, page.key)
	if err := s.backing.Decommit(memmap.FileRange{Start: page.backingOffset, End: page.backingOffset + hostarch.PageSize}); err != nil {
		return err
	}
	s.free = append(s.free, page.backingOffset)
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
	page.dirty = false
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
