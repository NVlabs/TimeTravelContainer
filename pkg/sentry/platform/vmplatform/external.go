// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package vmplatform

import (
	"context"
	"fmt"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
)

// This file maps host memory that another process shares, such as a GPU
// renderer's, into guests in place. GuestRAM copies ("stages") the contents of
// other files, which would hide the other process's writes from the guest and
// the guest's from it.
//
// Such memory is mapped at stage 2 in external slabs, at IPA of their own, and
// has frames, so that stage-1 mappings of it are tracked, revoked, and rebuilt
// with new epochs as GuestRAM's are. Its frames belong to no backing: they are
// never allocated, decommitted, or moved.

// stage2Access returns the stage-2 access that the slab is mapped with.
// External memory is never executable: stage 2 must never map a page that is
// executable in the host, and guests have no need to run code from it.
func (s *guestSlab) stage2Access() Stage2Access {
	if s.external {
		return Stage2Read | Stage2Write
	}
	return Stage2Read | Stage2Write | Stage2Execute
}

// externalFrames returns the frames of the host memory [hostAddress,
// hostAddress+length), mapping the host slabs that contain it at stage 2 the
// first time.
//
// Preconditions: hostAddress and length are multiples of GuestFrameSize, and
// length > 0. The host slabs that contain the range stay mapped in the host,
// and are not executable there, until releaseExternal.
func (r *GuestRAM) externalFrames(hostAddress uintptr, length uint64) ([]pgalloc.BackingFrame, error) {
	end := uint64(hostAddress) + length
	if uint64(hostAddress)%GuestFrameSize != 0 || length == 0 || length%GuestFrameSize != 0 || end < length {
		return nil, fmt.Errorf("invalid external memory [%#x, %#x)", hostAddress, end)
	}
	slabStart := uint64(hostAddress) &^ (HostSlabSize - 1)
	slabEnd := (end + HostSlabSize - 1) &^ (HostSlabSize - 1)
	if slabEnd < end {
		return nil, fmt.Errorf("invalid external memory [%#x, %#x)", hostAddress, end)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	var missing uint64
	for a := slabStart; a < slabEnd; a += HostSlabSize {
		if r.external[uintptr(a)] == nil {
			missing++
		}
	}
	if missing != 0 && r.wouldReachRolloverLocked(missing) {
		if r.rollover == nil {
			return nil, fmt.Errorf("GuestRAM IPA epoch %d reached 75%% rollover threshold", r.epoch)
		}
		r.startRolloverLocked()
		if r.wouldExhaustIPALocked(missing) {
			return nil, fmt.Errorf("GuestRAM IPA epoch %d is exhausted while its rollover is pending", r.epoch)
		}
	}
	// Map each run of missing slabs at once.
	for a := slabStart; a < slabEnd; {
		if r.external[uintptr(a)] != nil {
			a += HostSlabSize
			continue
		}
		runStart := a
		for a < slabEnd && r.external[uintptr(a)] == nil {
			a += HostSlabSize
		}
		runLength := a - runStart
		gpa := r.nextGPA
		mapStage2 := r.vm.MapStage2
		if shared, ok := r.vm.(SharedStage2Mapper); ok {
			mapStage2 = shared.MapSharedStage2
		}
		if err := mapStage2(gpa, uintptr(runStart), runLength, Stage2Read|Stage2Write); err != nil {
			return nil, fmt.Errorf("mapping external memory [%#x, %#x) at stage 2: %w", runStart, a, err)
		}
		r.nextGPA += runLength
		for offset := uint64(0); offset < runLength; offset += HostSlabSize {
			r.addExternalSlabLocked(uintptr(runStart+offset), gpa+offset)
		}
	}

	frames := make([]pgalloc.BackingFrame, 0, length/GuestFrameSize)
	for a := uint64(hostAddress); a < end; a += GuestFrameSize {
		slab := r.external[uintptr(a&^(HostSlabSize-1))]
		slot := (a - uint64(slab.hostAddress)) / GuestFrameSize
		frames = append(frames, pgalloc.BackingFrame{
			ID:          slab.frames[slot].id,
			GPA:         slab.gpa + slot*GuestFrameSize,
			HostAddress: uintptr(a),
		})
	}
	return frames, nil
}

// addExternalSlabLocked records an external slab that is mapped at gpa.
//
// Preconditions: r.mu is locked.
func (r *GuestRAM) addExternalSlabLocked(hostAddress uintptr, gpa uint64) {
	r.nextSlabID++
	slab := &guestSlab{
		key:         slabKey(r.nextSlabID),
		hostAddress: hostAddress,
		gpa:         gpa,
		external:    true,
	}
	for slot := range slab.frames {
		r.nextFrameID++
		frame := &guestFrame{
			id:       r.nextFrameID,
			slot:     uint8(slot),
			active:   true,
			mappings: make(map[MappingRef]struct{}),
			slab:     slab,
		}
		slab.frames[slot] = frame
		r.frames[frame.id] = frame
	}
	r.slabs[slab.key] = slab
	r.external[hostAddress] = slab
}

// releaseExternal unmaps the external slabs that contain [hostAddress,
// hostAddress+length) from stage 2, first revoking any stage-1 mappings of
// them. Its caller is about to unmap the memory from the host.
func (r *GuestRAM) releaseExternal(hostAddress uintptr, length uint64) error {
	end := uint64(hostAddress) + length
	slabStart := uint64(hostAddress) &^ (HostSlabSize - 1)
	slabEnd := (end + HostSlabSize - 1) &^ (HostSlabSize - 1)

	r.mu.Lock()
	var slabs []*guestSlab
	refs := make(map[pgalloc.FrameID][]MappingRef)
	for a := slabStart; a < slabEnd; a += HostSlabSize {
		slab := r.external[uintptr(a)]
		if slab == nil {
			continue
		}
		slabs = append(slabs, slab)
		for _, frame := range slab.frames {
			// New stage-1 mappings of the frame fail from now on.
			frame.active = false
			for ref := range frame.mappings {
				refs[frame.id] = append(refs[frame.id], ref)
			}
		}
	}
	r.mu.Unlock()
	if len(slabs) == 0 {
		return nil
	}

	var revokeErr error
	if len(refs) != 0 {
		// The Sentry's memory manager unmaps the memory from address spaces
		// before it releases the memory, so this is a backstop.
		if r.revoker == nil {
			revokeErr = fmt.Errorf("releasing external memory with stage-1 mappings and no revoker")
		} else {
			revokeErr = r.revoker.RevokeFrames(context.Background(), refs)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	var unmapErr error
	for _, slab := range slabs {
		// The host is about to unmap the memory, so stage 2 must not keep
		// it whatever the stage-1 mappings.
		if err := r.vm.UnmapStage2(slab.gpa, HostSlabSize); err != nil && unmapErr == nil {
			unmapErr = fmt.Errorf("unmapping external memory at %#x from stage 2: %w", slab.hostAddress, err)
		}
		for _, frame := range slab.frames {
			delete(r.frames, frame.id)
		}
		delete(r.slabs, slab.key)
		delete(r.external, slab.hostAddress)
	}
	r.cond.Broadcast()
	if revokeErr != nil {
		log.Warningf("Revoking stage-1 mappings of external memory [%#x, %#x): %v", hostAddress, end, revokeErr)
	}
	return unmapErr
}

// hostMemoryFrames returns the frames of fr of file, memory that the Sentry
// shares with another host process, which guests map in place.
func (p *Platform) hostMemoryFrames(file memmap.HostMemoryFile, fr memmap.FileRange, at hostarch.AccessType) ([]pgalloc.BackingFrame, error) {
	if at.Execute {
		return nil, fmt.Errorf("shared host memory is not executable")
	}
	address, err := file.HostAddress(fr)
	if err != nil {
		return nil, err
	}
	// Stage 2 maps whole host slabs, which must be the file's memory: a slab
	// that extends past it could be any host memory, even executable.
	slabStart := uint64(address) &^ (HostSlabSize - 1)
	slabEnd := (uint64(address) + fr.Length() + HostSlabSize - 1) &^ (HostSlabSize - 1)
	before := uint64(address) - slabStart
	if fr.Start < before {
		return nil, fmt.Errorf("shared host memory at %#x does not start a host slab", address-uintptr(fr.Start))
	}
	slabs := memmap.FileRange{Start: fr.Start - before, End: fr.Start - before + (slabEnd - slabStart)}
	if slabAddress, err := file.HostAddress(slabs); err != nil || uint64(slabAddress) != slabStart {
		return nil, fmt.Errorf("shared host memory %v is not whole host slabs: %v", fr, err)
	}
	if err := p.trackHostMemory(file, hostRange{start: uintptr(slabStart), end: uintptr(slabEnd)}); err != nil {
		return nil, err
	}
	return p.ram.externalFrames(address, fr.Length())
}

// trackHostMemory records that r of file may be mapped at stage 2, and the
// first time, arranges to unmap it when the file is released.
func (p *Platform) trackHostMemory(file memmap.HostMemoryFile, r hostRange) error {
	p.hostMemoryMu.Lock()
	defer p.hostMemoryMu.Unlock()
	if p.hostMemories == nil {
		return fmt.Errorf("the platform does not map shared host memory")
	}
	if tracked, ok := p.hostMemories[file]; ok {
		p.hostMemories[file] = hostRange{start: min(tracked.start, r.start), end: max(tracked.end, r.end)}
		return nil
	}
	p.hostMemories[file] = r
	file.OnRelease(func() {
		p.releaseHostMemory(file)
	})
	return nil
}

// releaseHostMemory unmaps file's memory from stage 2, before the Sentry
// unmaps it.
func (p *Platform) releaseHostMemory(file memmap.HostMemoryFile) {
	p.hostMemoryMu.Lock()
	r, ok := p.hostMemories[file]
	delete(p.hostMemories, file)
	p.hostMemoryMu.Unlock()
	if !ok {
		return
	}
	if err := p.ram.releaseExternal(r.start, uint64(r.end-r.start)); err != nil {
		log.Warningf("Releasing shared host memory [%#x, %#x): %v", r.start, r.end, err)
	}
}
