// Copyright 2018 The gVisor Authors.
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

// Package shm implements sysv shared memory segments.
//
// Known missing features:
//
//   - SHM_LOCK/SHM_UNLOCK are no-ops. The sentry currently doesn't implement
//     memory locking in general.
//
//   - SHM_HUGETLB and related flags for shmget(2) are ignored. There's no easy
//     way to implement hugetlb support on a per-map basis, and it has no impact
//     on correctness.
//
//   - SHM_NORESERVE for shmget(2) is ignored, the sentry doesn't implement swap
//     so it's meaningless to reserve space for swap.
//
//   - No per-process segment size enforcement. This feature probably isn't used
//     much anyways, since Linux sets the per-process limits to the system-wide
//     limits by default.
//
// Lock ordering: mm.mappingMu -> shm registry lock -> shm lock
package shm

import (
	goContext "context"
	"fmt"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/kernel/ipc"
	"gvisor.dev/gvisor/pkg/sentry/ktime"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sync"
)

// Registry tracks all shared memory segments in an IPC namespace. The registry
// provides the mechanisms for creating and finding segments, and reporting
// global shm parameters.
//
// +stateify savable
type Registry struct {
	// userNS owns the IPC namespace this registry belong to. Immutable.
	userNS *auth.UserNamespace

	// mu protects all fields below.
	mu sync.Mutex `state:"nosave"`

	// reg defines basic fields and operations needed for all SysV registries.
	//
	// Within reg, there are two maps, objects and keysToIDs.
	//
	// reg.objects holds all referenced segments, which are removed on the last
	// DecRef. Thus, it cannot itself hold a reference on the Shm.
	//
	// Since removal only occurs after the last (unlocked) DecRef, there
	// exists a short window during which a Shm is still in reg.objects but is
	// unreferenced. Users must use TryIncRef to determine if the Shm is valid.
	//
	// keysToIDs maps segment keys to IDs.
	//
	// Shms in keysToIDs are guaranteed to be referenced, as they are
	// removed by dissociateKey before the last DecRef.
	//
	// +checklocks:mu
	reg *ipc.Registry

	// Sum of the sizes of all existing segments rounded up to page size, in
	// units of page size.
	//
	// +checklocks:mu
	totalPages uint64
}

// NewRegistry creates a new shm registry.
func NewRegistry(userNS *auth.UserNamespace) *Registry {
	return &Registry{
		userNS: userNS,
		reg:    ipc.NewRegistry(userNS),
	}
}

// FindByID looks up a segment given an ID.
//
// FindByID returns a reference on Shm.
//
// +checklocksexclude:r.mu
func (r *Registry) FindByID(id ipc.ID) *Shm {
	r.mu.Lock()
	defer r.mu.Unlock()
	mech := r.reg.FindByID(id)
	if mech == nil {
		return nil
	}
	s := mech.(*Shm)

	// Take a reference on s. If TryIncRef fails, s has reached the last
	// DecRef, but hasn't quite been removed from r.reg.objects yet.
	if s != nil && s.TryIncRef() {
		return s
	}
	return nil
}

// dissociateKey removes the association between a segment and its key,
// preventing it from being found by key. This doesn't necessarily mean the
// segment is about to be destroyed: it remains accessible by ID while
// references remain.
//
// +checklocksexclude:r.mu
// +checklocksexclude:s.mu
func (r *Registry) dissociateKey(s *Shm) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.obj.Key != linux.IPC_PRIVATE {
		r.reg.DissociateKey(s.obj.Key)
		s.obj.Key = linux.IPC_PRIVATE
	}
}

// FindOrCreate looks up or creates a segment in the registry. It's functionally
// analogous to open(2).
//
// FindOrCreate returns a reference on Shm.
//
// +checklocksexclude:r.mu
func (r *Registry) FindOrCreate(ctx context.Context, pid int32, key ipc.Key, size uint64, mode linux.FileMode, private, create, exclusive bool) (*Shm, error) {
	if (create || private) && (size < linux.SHMMIN || size > linux.SHMMAX) {
		// "A new segment was to be created and size is less than SHMMIN or
		// greater than SHMMAX." - man shmget(2)
		//
		// Note that 'private' always implies the creation of a new segment
		// whether IPC_CREAT is specified or not.
		return nil, linuxerr.EINVAL
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.reg.ObjectCount() >= linux.SHMMNI {
		// "All possible shared memory IDs have been taken (SHMMNI) ..."
		//   - man shmget(2)
		return nil, linuxerr.ENOSPC
	}

	if !private {
		shm, err := r.reg.Find(ctx, key, mode, create, exclusive)
		if err != nil {
			return nil, err
		}

		// Validate shm-specific parameters.
		if shm != nil {
			shm := shm.(*Shm)
			if size > shm.size {
				// "A segment for the given key exists, but size is greater than
				// the size of that segment." - man shmget(2)
				return nil, linuxerr.EINVAL
			}
			shm.IncRef()
			return shm, nil
		}
	}

	var sizeAligned uint64
	if val, ok := hostarch.Addr(size).RoundUp(); ok {
		sizeAligned = uint64(val)
	} else {
		return nil, linuxerr.EINVAL
	}

	if numPages := sizeAligned / hostarch.PageSize; r.totalPages+numPages > linux.SHMALL {
		// "... allocating a segment of the requested size would cause the
		// system to exceed the system-wide limit on shared memory (SHMALL)."
		//   - man shmget(2)
		return nil, linuxerr.ENOSPC
	}

	// Need to create a new segment.
	s, err := r.newShmLocked(ctx, pid, key, auth.CredentialsFromContext(ctx), mode, size)
	if err != nil {
		return nil, err
	}
	// The initial reference is held by s itself. Take another to return to
	// the caller.
	s.IncRef()
	return s, nil
}

// newShmLocked creates a new segment in the registry.
//
// +checklocks:r.mu
func (r *Registry) newShmLocked(ctx context.Context, pid int32, key ipc.Key, creator *auth.Credentials, mode linux.FileMode, size uint64) (*Shm, error) {
	mf := pgalloc.MemoryFileFromContext(ctx)
	if mf == nil {
		panic(fmt.Sprintf("context.Context %T lacks non-nil value for key %T", ctx, pgalloc.CtxMemoryFile))
	}
	devID, ok := deviceIDFromContext(ctx)
	if !ok {
		panic(fmt.Sprintf("context.Context %T lacks value for key %T", ctx, CtxDeviceID))
	}

	effectiveSize := uint64(hostarch.Addr(size).MustRoundUp())
	fr, err := mf.Allocate(effectiveSize, pgalloc.AllocOpts{Kind: usage.Anonymous, MemCgID: pgalloc.MemoryCgroupIDFromContext(ctx)})
	if err != nil {
		return nil, err
	}

	shm := &Shm{
		mf:            mf,
		registry:      r,
		devID:         devID,
		size:          size,
		effectiveSize: effectiveSize,
		obj:           ipc.NewObject(r.reg.UserNS, ipc.Key(key), creator, creator, mode),
		creatorPID:    pid,
		changeTime:    ktime.NowFromContext(ctx),
	}
	shm.data.Insert(shm.data.FindGap(0), memmap.MappableRange{Start: 0, End: effectiveSize}, fr.Start)
	shm.InitRefs()

	if err := r.reg.Register(shm); err != nil {
		return nil, err
	}
	r.totalPages += effectiveSize / hostarch.PageSize

	return shm, nil
}

// IPCInfo reports global parameters for sysv shared memory segments on this
// system. See shmctl(IPC_INFO).
func (r *Registry) IPCInfo() *linux.ShmParams {
	return &linux.ShmParams{
		ShmMax: linux.SHMMAX,
		ShmMin: linux.SHMMIN,
		ShmMni: linux.SHMMNI,
		ShmSeg: linux.SHMSEG,
		ShmAll: linux.SHMALL,
	}
}

// ShmInfo reports linux-specific global parameters for sysv shared memory
// segments on this system. See shmctl(SHM_INFO).
//
// +checklocksexclude:r.mu
func (r *Registry) ShmInfo() *linux.ShmInfo {
	r.mu.Lock()
	defer r.mu.Unlock()

	return &linux.ShmInfo{
		UsedIDs: int32(r.reg.LastIDUsed()),
		ShmTot:  r.totalPages,
		ShmRss:  r.totalPages, // We could probably get a better estimate from memory accounting.
		ShmSwp:  0,            // No reclaim at the moment.
	}
}

// remove deletes a segment from this registry, deaccounting the memory used by
// the segment.
//
// Precondition: Must follow a call to r.dissociateKey(s).
//
// +checklocksexclude:r.mu
// +checklocksexclude:s.mu
func (r *Registry) remove(s *Shm) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.obj.Key != linux.IPC_PRIVATE {
		panic(fmt.Sprintf("Attempted to remove %s from the registry whose key is still associated", s.debugLocked()))
	}

	r.reg.DissociateID(s.obj.ID)
	r.totalPages -= s.effectiveSize / hostarch.PageSize
}

// Release drops the self-reference of each active shm segment in the registry.
// It is called when the kernel.IPCNamespace containing r is being destroyed.
//
// +checklocksexclude:r.mu
func (r *Registry) Release(ctx context.Context) {
	// Because Shm.DecRef() may acquire the same locks, collect the segments to
	// release first. Note that this should not race with any updates to r, since
	// the IPC namespace containing it has no more references.
	toRelease := make([]*Shm, 0)
	r.mu.Lock()
	r.reg.ForAllObjects(
		func(o ipc.Mechanism) {
			s := o.(*Shm)
			s.mu.Lock()
			if !s.pendingDestruction {
				toRelease = append(toRelease, s)
			}
			s.mu.Unlock()
		},
	)
	r.mu.Unlock()

	for _, s := range toRelease {
		r.dissociateKey(s)
		s.DecRef(ctx)
	}
}

// PrepareLiveFork invalidates all cached segment translations and marks the
// backing pages of every live segment as shared with an impending child.
// Callers must prevent concurrent registry and mapping mutations.
func (r *Registry) PrepareLiveFork(ctx context.Context) {
	var segments []*Shm
	r.mu.Lock()
	r.reg.ForAllObjects(func(o ipc.Mechanism) {
		s := o.(*Shm)
		if s.TryIncRef() {
			segments = append(segments, s)
		}
	})
	r.mu.Unlock()

	for _, s := range segments {
		s.prepareLiveFork()
		s.DecRef(ctx)
	}
}

// CaptureMemoryFileRanges records every SysV SHM segment serialized through
// this registry, including segments that currently have no VMA/PMA.
func (r *Registry) CaptureMemoryFileRanges(ctx context.Context, capture *pgalloc.MemoryRangeCapture) error {
	var segments []*Shm
	r.mu.Lock()
	r.reg.ForAllObjects(func(o ipc.Mechanism) {
		s := o.(*Shm)
		if s.TryIncRef() {
			segments = append(segments, s)
		}
	})
	r.mu.Unlock()
	var captureErr error
	for _, s := range segments {
		for _, owned := range s.MemoryFileRanges() {
			if captureErr != nil {
				break
			}
			mf, ok := owned.File.(*pgalloc.MemoryFile)
			if !ok {
				captureErr = fmt.Errorf("SysV SHM owns unsupported memory file %T", owned.File)
				break
			}
			if err := capture.RecordOwned(s, mf, owned.Range); err != nil {
				captureErr = err
				break
			}
		}
		s.DecRef(ctx)
	}
	return captureErr
}

// Shm represents a single shared memory segment.
//
// Shm segments are backed directly by an allocation from platform memory.
// Segments are always mapped as a whole, greatly simplifying how mappings are
// tracked. However note that mremap and munmap calls may cause the vma for a
// segment to become fragmented; which requires special care when unmapping a
// segment. See mm/shm.go.
//
// Segments persist until they are explicitly marked for destruction via
// MarkDestroyed().
//
// Shm implements memmap.Mappable and memmap.MappingIdentity.
//
// +stateify savable
type Shm struct {
	// ShmRefs tracks the number of references to this segment.
	//
	// A segment holds a reference to itself until it is marked for
	// destruction.
	//
	// In addition to direct users, the MemoryManager will hold references
	// via MappingIdentity.
	ShmRefs

	// mf is initialized during construction or afterLoad, then immutable while
	// the segment is in use.
	mf *pgalloc.MemoryFile `state:"nosave"`

	// registry points to the shm registry containing this segment. Immutable.
	registry *Registry

	// devID is the segment's device ID. Immutable.
	devID uint32

	// size is the requested size of the segment at creation, in
	// bytes. Immutable.
	size uint64

	// effectiveSize of the segment, rounding up to the next page
	// boundary. Immutable.
	//
	// Invariant: effectiveSize must be a multiple of hostarch.PageSize.
	effectiveSize uint64

	// mapsMu protects mappings.
	mapsMu sync.Mutex `state:"nosave"`

	// mappings tracks every mapping of this segment into a MappingSpace.
	// Protected by mapsMu.
	mappings memmap.MappingSet

	// dataMu protects data and cow.
	dataMu sync.Mutex `state:"nosave"`

	// data maps offsets in this segment to offsets in mf. Unlike the original
	// single FileRange backing, this may become page-granular after live fork.
	// Protected by dataMu.
	data ShmFileRangeSet

	// cow contains logical ranges whose backing is also owned by a sibling
	// live-fork branch. It does not own MemoryFile references.
	// Protected by dataMu.
	cow ShmFileRangeSet

	// mu protects mutable state below, including the mutable fields of obj.
	mu sync.Mutex `state:"nosave"`

	// obj points to common IPC metadata. The pointer is immutable; the ID is
	// assigned during registration, then immutable. The key, owner IDs, and mode
	// require mu after initialization.
	obj *ipc.Object

	// attachTime is updated on every successful shmat.
	//
	// +checklocks:mu
	attachTime ktime.Time

	// detachTime is updated on every successful shmdt.
	//
	// +checklocks:mu
	detachTime ktime.Time

	// changeTime is updated on every successful changes to the segment via
	// shmctl(IPC_SET).
	//
	// +checklocks:mu
	changeTime ktime.Time

	// creatorPID is the PID of the process that created the segment. Immutable.
	creatorPID int32

	// lastAttachDetachPID is the pid of the process that issued the last shmat
	// or shmdt syscall.
	//
	// +checklocks:mu
	lastAttachDetachPID int32

	// pendingDestruction indicates the segment was marked as destroyed through
	// shmctl(IPC_RMID). Its key is dissociated from the registry, but the segment
	// remains accessible by ID while references remain. It is destroyed when
	// the last reference is dropped.
	//
	// +checklocks:mu
	pendingDestruction bool
}

// MemoryFileRanges implements memmap.MemoryFileRangeProvider. The returned
// ranges represent Shm's direct ownership, independently of mapped PMAs.
func (s *Shm) MemoryFileRanges() []memmap.MemoryFileRange {
	s.dataMu.Lock()
	defer s.dataMu.Unlock()
	ranges := make([]memmap.MemoryFileRange, 0, s.data.Span()/hostarch.PageSize)
	for seg := s.data.FirstSegment(); seg.Ok(); seg = seg.NextSegment() {
		ranges = append(ranges, memmap.MemoryFileRange{File: s.mf, Range: seg.FileRange()})
	}
	return ranges
}

// afterLoad is invoked by stateify.
func (s *Shm) afterLoad(ctx goContext.Context) {
	s.mf = pgalloc.MemoryFileFromContext(ctx)
	if relocation := pgalloc.SparseRelocationFromContext(ctx); relocation != nil {
		relocateSet := func(set *ShmFileRangeSet) error {
			var relocated ShmFileRangeSet
			for seg := set.FirstSegment(); seg.Ok(); seg = seg.NextSegment() {
				runs, err := relocation.Runs(s.mf.ResourceID(), seg.FileRange())
				if err != nil {
					return err
				}
				logical := seg.Start()
				for _, run := range runs {
					mr := memmap.MappableRange{Start: logical, End: logical + run.Length()}
					relocated.Insert(relocated.FindGap(mr.Start), mr, run.Start)
					logical = mr.End
				}
			}
			// Preserve the segment tree's internal parent links when replacing
			// the restored set; a struct copy is invalid once the set has more
			// than one tree level.
			set.MoveFrom(&relocated)
			return nil
		}
		if err := relocateSet(&s.data); err != nil {
			panic(fmt.Sprintf("relocating SysV SHM data: %v", err))
		}
		if err := relocateSet(&s.cow); err != nil {
			panic(fmt.Sprintf("relocating SysV SHM CoW metadata: %v", err))
		}
	}
	if !pgalloc.LiveForkCloneNeedsPageRefs(ctx) {
		return
	}
	memCgID := pgalloc.MemoryCgroupIDFromContext(ctx)
	for seg := s.data.FirstSegment(); seg.Ok(); seg = seg.NextSegment() {
		pgalloc.AcquireRestoreRef(ctx, s.mf, seg.FileRange(), memCgID)
	}
}

// prepareLiveFork invalidates all cached translations and marks all segment
// pages as shared with the impending child branch.
func (s *Shm) prepareLiveFork() {
	s.mapsMu.Lock()
	s.mappings.InvalidateAll(memmap.InvalidateOpts{InvalidatePrivate: true})
	s.dataMu.Lock()
	s.cow.RemoveAll()
	for seg := s.data.FirstSegment(); seg.Ok(); seg = seg.NextSegment() {
		s.cow.Insert(s.cow.FindGap(seg.Start()), seg.Range(), seg.Value())
	}
	s.dataMu.Unlock()
	s.mapsMu.Unlock()
}

// copyOnWriteLocked gives s branch-private backing for shared pages in mr.
//
// Preconditions:
//   - s.dataMu is locked.
//   - mr is page-aligned.
func (s *Shm) copyOnWriteLocked(ctx context.Context, mr memmap.MappableRange) error {
	memCgID := pgalloc.MemoryCgroupIDFromContext(ctx)
	for cowSeg := s.cow.LowerBoundSegment(mr.Start); cowSeg.Ok() && cowSeg.Start() < mr.End; {
		cowRange := cowSeg.Range().Intersect(mr)
		cowSeg = s.cow.Isolate(cowSeg, cowRange)
		dataSeg := s.data.FindSegment(cowRange.Start)
		if !dataSeg.Ok() || !dataSeg.Range().IsSupersetOf(cowRange) {
			return fmt.Errorf("SysV SHM live-fork CoW range %v has no data segment", cowRange)
		}
		dataSeg = s.data.Isolate(dataSeg, cowRange)
		oldRange := dataSeg.FileRange()
		newRange, err := s.mf.Allocate(cowRange.Length(), pgalloc.AllocOpts{
			Kind:    usage.Anonymous,
			MemCgID: memCgID,
			Mode:    pgalloc.AllocateAndWritePopulate,
		})
		if err != nil {
			return err
		}
		mappings, mappingLease, err := memmap.MapInternalBatchWithLease(s.mf, []memmap.InternalMappingRequest{
			{Range: oldRange, Access: hostarch.Read},
			{Range: newRange, Access: hostarch.Write},
		})
		if err != nil {
			s.mf.DecRef(newRange)
			return err
		}
		oldIMS, newIMS := mappings[0], mappings[1]
		n, copyErr := safemem.CopySeq(newIMS, oldIMS)
		if mappingLease != nil {
			mappingLease.Release()
		}
		if copyErr != nil || n != cowRange.Length() {
			s.mf.DecRef(newRange)
			if copyErr != nil {
				return copyErr
			}
			return fmt.Errorf("short SysV SHM live-fork CoW copy: got %d bytes, want %d", n, cowRange.Length())
		}
		dataSeg.SetValue(newRange.Start)
		s.mf.DecRef(oldRange)
		cowSeg = s.cow.Remove(cowSeg).NextSegment()
	}
	return nil
}

// ID returns object's ID.
func (s *Shm) ID() ipc.ID {
	return s.obj.ID
}

// Object implements ipc.Mechanism.Object.
func (s *Shm) Object() *ipc.Object {
	return s.obj
}

// Destroy implements ipc.Mechanism.Destroy. No work is performed on shm.Destroy
// because a different removal mechanism is used in shm. See Shm.MarkDestroyed.
//
// +checklocks:s.mu
func (s *Shm) Destroy() {
}

// Lock implements ipc.Mechanism.Lock.
//
// +checklocksacquire:s.mu
func (s *Shm) Lock() {
	s.mu.Lock()
}

// Unlock implements ipc.Mechanism.Unlock.
//
// +checklocksrelease:s.mu
func (s *Shm) Unlock() {
	s.mu.Unlock()
}

// +checklocks:s.mu
func (s *Shm) debugLocked() string {
	return fmt.Sprintf("Shm{id: %d, key: %d, size: %d bytes, refs: %d, destroyed: %v}",
		s.obj.ID, s.obj.Key, s.size, s.ReadRefs(), s.pendingDestruction)
}

// MappedName implements memmap.MappingIdentity.MappedName.
//
// +checklocksexclude:s.mu
func (s *Shm) MappedName(ctx context.Context) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fmt.Sprintf("SYSV%08d", s.obj.Key)
}

// DeviceID implements memmap.MappingIdentity.DeviceID.
func (s *Shm) DeviceID() uint64 {
	return uint64(s.devID)
}

// InodeID implements memmap.MappingIdentity.InodeID.
func (s *Shm) InodeID() uint64 {
	// "shmid gets reported as "inode#" in /proc/pid/maps. proc-ps tools use
	// this. Changing this will break them." -- Linux, ipc/shm.c:newseg()
	return uint64(s.obj.ID)
}

// DecRef drops a reference on s. The final reference drop acquires the registry
// and segment mutexes to remove the segment.
//
// +checklocksexclude:s.mu
// +checklocksexclude:s.registry.mu
func (s *Shm) DecRef(ctx context.Context) {
	s.ShmRefs.DecRef(func() {
		s.dataMu.Lock()
		s.data.DropAll(s.mf)
		s.cow.RemoveAll()
		s.dataMu.Unlock()
		s.registry.remove(s)
	})
}

// Msync implements memmap.MappingIdentity.Msync. Msync is a no-op for shm
// segments.
func (s *Shm) Msync(context.Context, memmap.MappableRange) error {
	return nil
}

// AddMapping implements memmap.Mappable.AddMapping.
//
// +checklocksexclude:s.mu
func (s *Shm) AddMapping(ctx context.Context, ms memmap.MappingSpace, ar hostarch.AddrRange, offset uint64, writable bool) error {
	s.mapsMu.Lock()
	s.mappings.AddMapping(ms, ar, offset, writable)
	s.mapsMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attachTime = ktime.NowFromContext(ctx)
	if pid, ok := auth.ThreadGroupIDFromContext(ctx); ok {
		s.lastAttachDetachPID = pid
	} else {
		// AddMapping is called during a syscall, so ctx should always be a task
		// context.
		log.Warningf("Adding mapping to %s but couldn't get the current pid; not updating the last attach pid", s.debugLocked())
	}
	return nil
}

// RemoveMapping implements memmap.Mappable.RemoveMapping.
//
// +checklocksexclude:s.mu
func (s *Shm) RemoveMapping(ctx context.Context, ms memmap.MappingSpace, ar hostarch.AddrRange, offset uint64, writable bool) {
	s.mapsMu.Lock()
	s.mappings.RemoveMapping(ms, ar, offset, writable)
	s.mapsMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	// RemoveMapping may be called during task exit, when ctx
	// is context.Background. Gracefully handle missing clocks. Failing to
	// update the detach time in these cases is ok, since no one can observe the
	// omission.
	if clock := ktime.RealtimeClockFromContext(ctx); clock != nil {
		s.detachTime = clock.Now()
	}

	// If called from a non-task context we also won't have a threadgroup
	// id. Silently skip updating the lastAttachDetachPid in that case.
	if pid, ok := auth.ThreadGroupIDFromContext(ctx); ok {
		s.lastAttachDetachPID = pid
	} else {
		log.Debugf("Couldn't obtain pid when removing mapping to %s, not updating the last detach pid.", s.debugLocked())
	}
}

// CopyMapping implements memmap.Mappable.CopyMapping.

func (s *Shm) CopyMapping(_ context.Context, ms memmap.MappingSpace, _, dstAR hostarch.AddrRange, offset uint64, writable bool) error {
	s.mapsMu.Lock()
	s.mappings.AddMapping(ms, dstAR, offset, writable)
	s.mapsMu.Unlock()
	return nil
}

// Translate implements memmap.Mappable.Translate.
func (s *Shm) Translate(ctx context.Context, required, optional memmap.MappableRange, at hostarch.AccessType) ([]memmap.Translation, error) {
	s.dataMu.Lock()
	defer s.dataMu.Unlock()

	var err error
	valid := memmap.MappableRange{Start: 0, End: s.effectiveSize}
	if required.End > valid.End {
		err = &memmap.BusError{linuxerr.EFAULT}
	}
	required = required.Intersect(valid)
	if required.Length() == 0 {
		return nil, err
	}

	// As with tmpfs, conservatively detach required pages on their first
	// post-fork fault. This guarantees that all mappings in this branch will
	// subsequently resolve to the same branch-private page.
	if cowErr := s.copyOnWriteLocked(ctx, required); cowErr != nil {
		return nil, &memmap.BusError{cowErr}
	}

	translateRange := optional.Intersect(valid)
	if !s.cow.IsEmpty() {
		// Do not install translations for shared pages that have not detached.
		translateRange = required
	}
	var ts []memmap.Translation
	for seg := s.data.FindSegment(required.Start); seg.Ok() && seg.Start() < required.End; seg = seg.NextSegment() {
		segMR := seg.Range().Intersect(translateRange)
		ts = append(ts, memmap.Translation{
			Source: segMR,
			File:   s.mf,
			Offset: seg.FileRangeOf(segMR).Start,
			Perms:  hostarch.AnyAccess,
		})
	}
	return ts, err
}

// InvalidateUnsavable implements memmap.Mappable.InvalidateUnsavable.
func (s *Shm) InvalidateUnsavable(ctx context.Context) error {
	return nil
}

// AttachOpts describes various flags passed to shmat(2).
type AttachOpts struct {
	Execute  bool
	Readonly bool
	Remap    bool
}

// ConfigureAttach creates an mmap configuration for the segment with the
// requested attach options.
//
// Postconditions: The returned MMapOpts are valid only as long as a reference
// continues to be held on s.
//
// +checklocksexclude:s.mu
func (s *Shm) ConfigureAttach(ctx context.Context, addr hostarch.Addr, opts AttachOpts) (memmap.MMapOpts, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingDestruction && s.ReadRefs() == 0 {
		return memmap.MMapOpts{}, linuxerr.EIDRM
	}

	creds := auth.CredentialsFromContext(ctx)
	ats := vfs.MayRead
	if !opts.Readonly {
		ats |= vfs.MayWrite
	}
	if opts.Execute {
		ats |= vfs.MayExec
	}
	if !s.obj.CheckPermissions(creds, ats) {
		// "The calling process does not have the required permissions for the
		// requested attach type, and does not have the CAP_IPC_OWNER capability
		// in the user namespace that governs its IPC namespace." - man shmat(2)
		return memmap.MMapOpts{}, linuxerr.EACCES
	}
	maxPerms := hostarch.AnyAccess
	if opts.Readonly {
		maxPerms.Write = false
	}
	return memmap.MMapOpts{
		Length: s.size,
		Offset: 0,
		Addr:   addr,
		Fixed:  opts.Remap,
		Perms: hostarch.AccessType{
			Read:    true,
			Write:   !opts.Readonly,
			Execute: opts.Execute,
		},
		MaxPerms:        maxPerms,
		Mappable:        s,
		MappingIdentity: s,
	}, nil
}

// EffectiveSize returns the size of the underlying shared memory segment. This
// may be larger than the requested size at creation, due to rounding to page
// boundaries.
func (s *Shm) EffectiveSize() uint64 {
	return s.effectiveSize
}

// IPCStat returns information about a shm. See shmctl(IPC_STAT).
//
// +checklocksexclude:s.mu
func (s *Shm) IPCStat(ctx context.Context) (*linux.ShmidDS, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// "The caller must have read permission on the shared memory segment."
	//   - man shmctl(2)
	creds := auth.CredentialsFromContext(ctx)
	if !s.obj.CheckPermissions(creds, vfs.MayRead) {
		// "IPC_STAT or SHM_STAT is requested and shm_perm.mode does not allow
		// read access for shmid, and the calling process does not have the
		// CAP_IPC_OWNER capability in the user namespace that governs its IPC
		// namespace." - man shmctl(2)
		return nil, linuxerr.EACCES
	}

	var mode uint16
	if s.pendingDestruction {
		mode |= linux.SHM_DEST
	}

	// Use the reference count as a rudimentary count of the number of
	// attaches. We exclude:
	//
	// 1. The reference the caller holds.
	// 2. The self-reference held by s prior to destruction.
	//
	// Note that this may still overcount by including transient references
	// used in concurrent calls.
	nattach := uint64(s.ReadRefs()) - 1
	if !s.pendingDestruction {
		nattach--
	}

	ds := &linux.ShmidDS{
		ShmPerm: linux.IPCPerm{
			Key:  uint32(s.obj.Key),
			UID:  uint32(creds.UserNamespace.MapFromKUID(s.obj.OwnerUID)),
			GID:  uint32(creds.UserNamespace.MapFromKGID(s.obj.OwnerGID)),
			CUID: uint32(creds.UserNamespace.MapFromKUID(s.obj.CreatorUID)),
			CGID: uint32(creds.UserNamespace.MapFromKGID(s.obj.CreatorGID)),
			Mode: mode | uint16(s.obj.Mode),
			Seq:  0, // IPC sequences not supported.
		},
		ShmSegsz:   s.size,
		ShmAtime:   s.attachTime.TimeT(),
		ShmDtime:   s.detachTime.TimeT(),
		ShmCtime:   s.changeTime.TimeT(),
		ShmCpid:    s.creatorPID,
		ShmLpid:    s.lastAttachDetachPID,
		ShmNattach: nattach,
	}

	return ds, nil
}

// Set modifies attributes for a segment. See shmctl(IPC_SET).
//
// +checklocksexclude:s.mu
func (s *Shm) Set(ctx context.Context, ds *linux.ShmidDS) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.obj.Set(ctx, &ds.ShmPerm); err != nil {
		return err
	}

	s.changeTime = ktime.NowFromContext(ctx)
	return nil
}

// MarkDestroyed marks a segment for destruction. The segment is actually
// destroyed once it has no references. MarkDestroyed may be called multiple
// times, and is safe to call after a segment has already been destroyed. See
// shmctl(IPC_RMID).
//
// +checklocksexclude:s.mu
// +checklocksexclude:s.registry.mu
func (s *Shm) MarkDestroyed(ctx context.Context) {
	s.registry.dissociateKey(s)

	s.mu.Lock()
	if s.pendingDestruction {
		s.mu.Unlock()
		return
	}
	s.pendingDestruction = true
	s.mu.Unlock()

	// Drop the self-reference so destruction occurs when all
	// external references are gone.
	//
	// N.B. This cannot be the final DecRef, as the caller also
	// holds a reference.
	s.DecRef(ctx)
}
