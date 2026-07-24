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

package tmpfs

import (
	goContext "context"
	"fmt"
	"io"
	"math"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/fsmetric"
	"gvisor.dev/gvisor/pkg/sentry/fsutil"
	"gvisor.dev/gvisor/pkg/sentry/hostfd"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/usermem"
)

// regularFile is a regular (=S_IFREG) tmpfs file.
//
// +stateify savable
type regularFile struct {
	inode inode

	// memoryUsageKind is the memory accounting category under which pages backing
	// this regularFile's contents are accounted.
	memoryUsageKind usage.MemoryKind

	// mapsMu protects mappings.
	mapsMu sync.Mutex `state:"nosave"`

	// mappings tracks mappings of the file into memmap.MappingSpaces.
	//
	// Protected by mapsMu.
	mappings memmap.MappingSet

	// writableMappingPages tracks how many pages of virtual memory are mapped
	// as potentially writable from this file. If a page has multiple mappings,
	// each mapping is counted separately.
	//
	// This counter is susceptible to overflow as we can potentially count
	// mappings from many VMAs. We count pages rather than bytes to slightly
	// mitigate this.
	//
	// Protected by mapsMu.
	writableMappingPages uint64

	// dataMu protects the fields below.
	dataMu sync.RWMutex `state:"nosave"`

	// data maps offsets into the file to offsets into memFile that store
	// the file's data.
	//
	// Protected by dataMu.
	data fsutil.FileRangeSet

	// cow contains logical file ranges whose data ranges are also owned by at
	// least one sibling live-fork branch. It does not own MemoryFile refs.
	// Protected by dataMu.
	cow fsutil.FileRangeSet

	// seals represents file seals on this inode.
	//
	// Protected by dataMu.
	seals uint32

	// diskMmap maps the current persistent volume object. It is rebuilt after
	// restore and replaced whenever branch-generation CoW selects a new object.
	diskMmap   fsutil.MmapCachedFile `state:"nosave"`
	diskObject string                `state:"nosave"`

	// initiallyUnlinked is true if this file was created using NewZeroFile or
	// NewMemfd => newUnlinkedRegularFileDescription. initiallyUnlinked should
	// be true when the equivalent shmem file in Linux would use
	// shmem_anon_vm_ops rather than shmem_vm_ops.
	//
	// initiallyUnlinked is immutable, but stored here since it fits into
	// alignment padding.
	initiallyUnlinked bool

	// huge is true if pages in this file may be hugepage-backed.
	huge bool

	// size is the size of data.
	//
	// Protected by both dataMu and inode.mu; reading it requires holding
	// either mutex, while writing requires holding both AND using atomics.
	// Readers that do not require consistency (like Stat) may read the
	// value atomically without holding either lock.
	size atomicbitops.Uint64
}

func (fs *filesystem) newRegularFile(kuid auth.KUID, kgid auth.KGID, mode linux.FileMode, parentDir *directory) (*inode, error) {
	file := &regularFile{
		memoryUsageKind: fs.usage,
		seals:           linux.F_SEAL_SEAL,
	}
	if fs.diskVolume != nil {
		file.diskMmap.SetFD(-1)
	}
	err := file.inode.init(file, fs, kuid, kgid, linux.S_IFREG|mode, parentDir)
	if err != nil {
		return nil, err
	}
	file.inode.nlink = atomicbitops.FromUint32(1) // from parent directory
	fs.registerLiveForkFile(file)
	return &file.inode, nil
}

func (fs *filesystem) registerLiveForkFile(file *regularFile) {
	fs.liveForkMu.Lock()
	if fs.liveForkFiles == nil {
		fs.liveForkFiles = make(map[*regularFile]struct{})
	}
	fs.liveForkFiles[file] = struct{}{}
	fs.liveForkMu.Unlock()
}

func (fs *filesystem) unregisterLiveForkFile(file *regularFile) {
	fs.liveForkMu.Lock()
	delete(fs.liveForkFiles, file)
	fs.liveForkMu.Unlock()
}

// afterLoad is invoked by stateify.
func (rf *regularFile) afterLoad(ctx goContext.Context) {
	rf.inode.fs.registerLiveForkFile(rf)
	if rf.inode.fs.diskVolume != nil {
		rf.diskMmap.SetFD(-1)
		rf.diskObject = ""
	}
	if !pgalloc.IsLiveForkClone(ctx) {
		return
	}
	memCgID := pgalloc.MemoryCgroupIDFromContext(ctx)
	for seg := rf.data.FirstSegment(); seg.Ok(); seg = seg.NextSegment() {
		rf.inode.fs.mf.IncRef(seg.FileRange(), memCgID)
	}
}

// prepareLiveFork invalidates file-backed translations and marks every present
// page as shared with the impending child branch. Private PMAs must be
// preserved: they contain application-owned modifications, such as dynamic
// linker relocations, that are no longer represented by the tmpfs file.
func (rf *regularFile) prepareLiveFork() {
	rf.mapsMu.Lock()
	rf.mappings.InvalidateAll(memmap.InvalidateOpts{})
	if rf.inode.fs.diskVolume != nil {
		rf.mapsMu.Unlock()
		return
	}
	rf.dataMu.Lock()
	rf.cow.RemoveAll()
	for seg := rf.data.FirstSegment(); seg.Ok(); seg = seg.NextSegment() {
		rf.cow.Insert(rf.cow.FindGap(seg.Start()), seg.Range(), seg.Value())
	}
	rf.dataMu.Unlock()
	rf.mapsMu.Unlock()
}

// ensureDiskMmapLocked opens a branch-private object and installs it as the
// backing for future translations.
//
// Preconditions: rf.mapsMu is locked.
func (rf *regularFile) ensureDiskMmapLocked() error {
	volume := rf.inode.fs.diskVolume
	file, meta, _, err := volume.Repository.OpenDataFile(volume.Branch, rf.inode.ino, true)
	if err != nil {
		return err
	}
	defer file.Close()
	if rf.diskObject == meta.Object && rf.diskMmap.FD() >= 0 {
		return nil
	}
	newFD, err := unix.Dup(int(file.Fd()))
	if err != nil {
		return err
	}
	if err := rf.diskMmap.ReplaceFD(newFD); err != nil {
		unix.Close(newFD)
		return err
	}
	rf.diskObject = meta.Object
	return nil
}

// invalidateDiskObject revokes application translations after another write
// path caused repository object CoW. The next fault lazily installs the new
// object FD.
func (rf *regularFile) invalidateDiskObject() {
	rf.mapsMu.Lock()
	rf.mappings.InvalidateAll(memmap.InvalidateOpts{})
	rf.diskObject = ""
	rf.mapsMu.Unlock()
}

// copyOnWriteLocked gives rf private backing for shared pages in mr.
//
// Preconditions:
//   - rf.dataMu is locked for writing.
//   - mr is page-aligned.
func (rf *regularFile) copyOnWriteLocked(ctx context.Context, mr memmap.MappableRange) error {
	memCgID := pgalloc.MemoryCgroupIDFromContext(ctx)
	for cowSeg := rf.cow.LowerBoundSegment(mr.Start); cowSeg.Ok() && cowSeg.Start() < mr.End; {
		cowRange := cowSeg.Range().Intersect(mr)
		cowSeg = rf.cow.Isolate(cowSeg, cowRange)
		dataSeg := rf.data.FindSegment(cowRange.Start)
		if !dataSeg.Ok() || !dataSeg.Range().IsSupersetOf(cowRange) {
			return fmt.Errorf("tmpfs live-fork CoW range %v has no data segment", cowRange)
		}
		dataSeg = rf.data.Isolate(dataSeg, cowRange)
		oldRange := dataSeg.FileRange()
		newRange, err := rf.inode.fs.mf.Allocate(cowRange.Length(), pgalloc.AllocOpts{
			Kind:    rf.memoryUsageKind,
			MemCgID: memCgID,
			Mode:    pgalloc.AllocateAndWritePopulate,
			Huge:    rf.huge && hostarch.IsHugePageAligned(cowRange.Start) && hostarch.IsHugePageAligned(cowRange.End),
		})
		if err != nil {
			return err
		}
		oldIMS, err := rf.inode.fs.mf.MapInternal(oldRange, hostarch.Read)
		if err != nil {
			rf.inode.fs.mf.DecRef(newRange)
			return err
		}
		newIMS, err := rf.inode.fs.mf.MapInternal(newRange, hostarch.Write)
		if err != nil {
			rf.inode.fs.mf.DecRef(newRange)
			return err
		}
		if n, err := safemem.CopySeq(newIMS, oldIMS); err != nil || n != cowRange.Length() {
			rf.inode.fs.mf.DecRef(newRange)
			if err != nil {
				return err
			}
			return io.ErrUnexpectedEOF
		}
		dataSeg.SetValue(newRange.Start)
		rf.inode.fs.mf.DecRef(oldRange)
		cowSeg = rf.cow.Remove(cowSeg).NextSegment()
	}
	return nil
}

// newUnlinkedRegularFileDescription creates a regular file on the tmpfs
// filesystem represented by mount and returns an FD representing that file.
// The new file is not reachable by path traversal from any other file.
//
// newUnlinkedRegularFileDescription is analogous to Linux's
// mm/shmem.c:__shmem_file_setup().
//
// Preconditions: mount must be a tmpfs mount.
func newUnlinkedRegularFileDescription(ctx context.Context, creds *auth.Credentials, mount *vfs.Mount, name string) (*regularFileFD, error) {
	fs, ok := mount.Filesystem().Impl().(*filesystem)
	if !ok {
		panic("tmpfs.newUnlinkedRegularFileDescription() called with non-tmpfs mount")
	}

	inode, err := fs.newRegularFile(creds.EffectiveKUID, creds.EffectiveKGID, 0777, nil /* parentDir */)
	if err != nil {
		return nil, err
	}
	inode.impl.(*regularFile).initiallyUnlinked = true
	d := fs.newDentry(inode)
	defer d.DecRef(ctx)
	d.name = name

	fd := &regularFileFD{}
	fd.Init(&inode.locks)
	flags := uint32(linux.O_RDWR)
	if err := fd.vfsfd.Init(fd, flags, creds, mount, &d.vfsd, &vfs.FileDescriptionOptions{}); err != nil {
		return nil, err
	}
	return fd, nil
}

// NewZeroFile creates a new regular file and file description as for
// mmap(MAP_SHARED | MAP_ANONYMOUS). The file has the given size and is
// initially (implicitly) filled with zeroes.
//
// Preconditions: mount must be a tmpfs mount.
func NewZeroFile(ctx context.Context, creds *auth.Credentials, mount *vfs.Mount, size uint64) (*vfs.FileDescription, error) {
	// Compare mm/shmem.c:shmem_zero_setup().
	fd, err := newUnlinkedRegularFileDescription(ctx, creds, mount, "dev/zero")
	if err != nil {
		return nil, err
	}
	rf := fd.inode().impl.(*regularFile)
	rf.memoryUsageKind = usage.Anonymous
	rf.huge = true
	rf.size.Store(size)
	return &fd.vfsfd, err
}

// NewMemfd creates a new regular file and file description as for
// memfd_create.
//
// Preconditions: mount must be a tmpfs mount.
func NewMemfd(ctx context.Context, creds *auth.Credentials, mount *vfs.Mount, allowSeals bool, name string) (*vfs.FileDescription, error) {
	fd, err := newUnlinkedRegularFileDescription(ctx, creds, mount, name)
	if err != nil {
		return nil, err
	}
	if allowSeals {
		fd.inode().impl.(*regularFile).seals = 0
	}
	return &fd.vfsfd, nil
}

// truncate grows or shrinks the file to the given size. It unconditionally
// updates mtime and ctime.
func (rf *regularFile) truncate(newSize uint64) error {
	rf.inode.mu.Lock()
	defer rf.inode.mu.Unlock()
	if err := rf.truncateNoTimeUpdateLocked(newSize); err != nil {
		return err
	}
	rf.inode.touchCMtimeLocked()
	if rf.inode.fs.diskVolume != nil {
		return rf.inode.fs.persistDiskMetadata(&rf.inode)
	}
	return nil
}

// Preconditions:
//   - rf.inode.mu must be held.
//   - rf.dataMu must be locked for writing.
//   - newSize > rf.size.
func (rf *regularFile) growLocked(newSize uint64) error {
	// Can we grow the file?
	if rf.seals&linux.F_SEAL_GROW != 0 {
		return linuxerr.EPERM
	}
	rf.size.Store(newSize)
	return nil
}

// truncateNoTimeUpdateLocked grows or shrinks the file to the given size.
// Callers are responsible for updating timestamps.
//
// Preconditions: rf.inode.mu must be held.
func (rf *regularFile) truncateNoTimeUpdateLocked(newSize uint64) error {
	oldSize := rf.size.RacyLoad()
	if newSize == oldSize {
		// Nothing to do.
		return nil
	}
	if rf.inode.fs.diskVolume != nil {
		if newSize > oldSize {
			if rf.seals&linux.F_SEAL_GROW != 0 {
				return linuxerr.EPERM
			}
		} else if rf.seals&linux.F_SEAL_SHRINK != 0 {
			return linuxerr.EPERM
		}
		cloned, err := rf.inode.fs.diskVolume.Repository.Truncate(rf.inode.fs.diskVolume.Branch, rf.inode.ino, int64(newSize))
		if err != nil {
			return err
		}
		rf.size.Store(newSize)
		rf.inode.fs.adjustDiskBytes(oldSize, newSize)
		rf.mapsMu.Lock()
		if cloned {
			rf.mappings.InvalidateAll(memmap.InvalidateOpts{InvalidatePrivate: true})
			rf.diskObject = ""
		} else if newSize < oldSize {
			rf.mappings.Invalidate(memmap.MappableRange{offsetPageEnd(int64(newSize)), offsetPageEnd(int64(oldSize))}, memmap.InvalidateOpts{InvalidatePrivate: true})
		}
		rf.mapsMu.Unlock()
		return nil
	}

	// Need to hold inode.mu and dataMu while modifying size.
	rf.dataMu.Lock()
	if newSize > oldSize {
		err := rf.growLocked(newSize)
		rf.dataMu.Unlock()
		return err
	}

	// We are shrinking the file. First check if this is allowed.
	if rf.seals&linux.F_SEAL_SHRINK != 0 {
		rf.dataMu.Unlock()
		return linuxerr.EPERM
	}
	if newSize&uint64(hostarch.PageSize-1) != 0 {
		pageStart := uint64(hostarch.PageRoundDown(newSize))
		if err := rf.copyOnWriteLocked(context.Background(), memmap.MappableRange{Start: pageStart, End: pageStart + hostarch.PageSize}); err != nil {
			rf.dataMu.Unlock()
			return err
		}
	}

	rf.size.Store(newSize)
	rf.dataMu.Unlock()

	// Invalidate past translations of truncated pages.
	oldpgend := offsetPageEnd(int64(oldSize))
	newpgend := offsetPageEnd(int64(newSize))
	if newpgend < oldpgend {
		rf.mapsMu.Lock()
		rf.mappings.Invalidate(memmap.MappableRange{newpgend, oldpgend}, memmap.InvalidateOpts{
			// Compare Linux's mm/shmem.c:shmem_setattr() =>
			// mm/memory.c:unmap_mapping_range(evencows=1).
			InvalidatePrivate: true,
		})
		rf.mapsMu.Unlock()
	}

	// We are now guaranteed that there are no translations of truncated pages,
	// and can remove them.
	rf.dataMu.Lock()
	rf.cow.RemoveRange(memmap.MappableRange{newpgend, oldpgend})
	decPages := rf.data.Truncate(newSize, rf.inode.fs.mf)
	rf.dataMu.Unlock()
	rf.inode.fs.unaccountPages(decPages)
	return nil
}

// AddMapping implements memmap.Mappable.AddMapping.
func (rf *regularFile) AddMapping(ctx context.Context, ms memmap.MappingSpace, ar hostarch.AddrRange, offset uint64, writable bool) error {
	rf.mapsMu.Lock()
	defer rf.mapsMu.Unlock()
	if rf.inode.fs.diskVolume != nil {
		rf.diskMmap.AddMapping(ar, offset)
		rf.mappings.AddMapping(ms, ar, offset, writable)
		return nil
	}
	rf.dataMu.RLock()
	defer rf.dataMu.RUnlock()

	// Reject writable mapping if F_SEAL_WRITE is set.
	if rf.seals&linux.F_SEAL_WRITE != 0 && writable {
		return linuxerr.EPERM
	}

	rf.mappings.AddMapping(ms, ar, offset, writable)
	if writable {
		pagesBefore := rf.writableMappingPages

		// ar is guaranteed to be page aligned per memmap.Mappable.
		rf.writableMappingPages += uint64(ar.Length() / hostarch.PageSize)

		if rf.writableMappingPages < pagesBefore {
			panic(fmt.Sprintf("Overflow while mapping potentially writable pages pointing to a tmpfs file. Before %v, after %v", pagesBefore, rf.writableMappingPages))
		}
	}

	return nil
}

// RemoveMapping implements memmap.Mappable.RemoveMapping.
func (rf *regularFile) RemoveMapping(ctx context.Context, ms memmap.MappingSpace, ar hostarch.AddrRange, offset uint64, writable bool) {
	rf.mapsMu.Lock()
	defer rf.mapsMu.Unlock()

	rf.mappings.RemoveMapping(ms, ar, offset, writable)
	if rf.inode.fs.diskVolume != nil {
		rf.diskMmap.RemoveMapping(ar, offset)
		return
	}

	if writable {
		pagesBefore := rf.writableMappingPages

		// ar is guaranteed to be page aligned per memmap.Mappable.
		rf.writableMappingPages -= uint64(ar.Length() / hostarch.PageSize)

		if rf.writableMappingPages > pagesBefore {
			panic(fmt.Sprintf("Underflow while unmapping potentially writable pages pointing to a tmpfs file. Before %v, after %v", pagesBefore, rf.writableMappingPages))
		}
	}
}

// CopyMapping implements memmap.Mappable.CopyMapping.
func (rf *regularFile) CopyMapping(ctx context.Context, ms memmap.MappingSpace, srcAR, dstAR hostarch.AddrRange, offset uint64, writable bool) error {
	return rf.AddMapping(ctx, ms, dstAR, offset, writable)
}

// Translate implements memmap.Mappable.Translate.
func (rf *regularFile) Translate(ctx context.Context, required, optional memmap.MappableRange, at hostarch.AccessType) ([]memmap.Translation, error) {
	if rf.inode.fs.diskVolume != nil {
		rf.mapsMu.Lock()
		defer rf.mapsMu.Unlock()
		// Privatize on the first mapping fault after a fork, including a read
		// fault. This preserves coherent MAP_SHARED views without invalidating
		// another MappingSpace from inside an MM fault lock.
		if err := rf.ensureDiskMmapLocked(); err != nil {
			return nil, &memmap.BusError{err}
		}
		pgend := offsetPageEnd(int64(rf.size.Load()))
		if required.Start >= pgend {
			return nil, &memmap.BusError{io.EOF}
		}
		if optional.End > pgend {
			optional.End = pgend
		}
		if required.End > pgend {
			required.End = pgend
		}
		return []memmap.Translation{{
			Source: optional,
			File:   &rf.diskMmap,
			Offset: optional.Start,
			Perms:  hostarch.AnyAccess,
		}}, nil
	}
	memCgID := pgalloc.MemoryCgroupIDFromContext(ctx)
	mayHuge := rf.huge && rf.inode.fs.mf.HugepagesEnabled()

	rf.dataMu.Lock()
	defer rf.dataMu.Unlock()

	// Constrain translations to f.attr.Size (rounded up) to prevent
	// translation to pages that may be concurrently truncated.
	pgend := offsetPageEnd(int64(rf.size.RacyLoad()))
	var beyondEOF bool
	if required.End > pgend {
		if required.Start >= pgend {
			return nil, &memmap.BusError{io.EOF}
		}
		beyondEOF = true
		required.End = pgend
	}
	if optional.End > pgend {
		optional.End = pgend
	}

	// Detach required mapped pages on their first post-fork fault. Translate
	// cannot invalidate other MappingSpaces while called under MM fault locks,
	// so detaching on first access (including reads) ensures that no stale
	// translation exists within this branch when a later shared write occurs.
	if err := rf.copyOnWriteLocked(ctx, required); err != nil {
		return nil, &memmap.BusError{err}
	}

	// fillRange bounds how far Translate allocates ("fills") ahead of the
	// faulting page. It is kept small to bound speculative allocation against
	// tmpfs size limits (see filesystem.accountPages): a fault that touches a
	// hole must not commit an unbounded number of pages and prematurely exhaust
	// the mount's size limit. This matches gofer.maxFillRange().
	const maxFillBytes = 64 << 10 // 64 KiB
	fillRange := maxOptionalRange(required, optional, maxFillBytes)
	pagesToFill := rf.data.PagesToFill(required, fillRange)
	if !rf.inode.fs.accountPages(pagesToFill) {
		// If we can not accommodate pagesToFill pages, then retry with just
		// the required range. Because fillRange may be larger than required.
		// Only error out if even the required range can not be allocated for.
		pagesToFill = rf.data.PagesToFill(required, required)
		if !rf.inode.fs.accountPages(pagesToFill) {
			return nil, &memmap.BusError{linuxerr.ENOSPC}
		}
		fillRange = required
	}
	pagesAlloced, cerr := rf.data.Fill(ctx, required, fillRange, rf.size.RacyLoad(), rf.inode.fs.mf, pgalloc.AllocOpts{
		Kind:    rf.memoryUsageKind,
		MemCgID: memCgID,
		Huge:    mayHuge,
	}, nil)
	// rf.data.Fill() may fail mid-way. We still want to account any pages that
	// were allocated, irrespective of an error.
	rf.inode.fs.adjustPageAcct(pagesToFill, pagesAlloced)

	// translateRange bounds how far Translate extends the returned translation
	// over pages that are *already* present, beyond the allocation-bounded
	// fillRange. Extending a translation over present pages does not allocate,
	// so it can use a much larger window than fillRange without affecting tmpfs
	// size accounting. Under gVisor each translation becomes a pma that requires
	// establishing a host AddressSpace mapping (platform.AddressSpace.MapFile),
	// so returning larger translations substantially reduces the number of such
	// mappings (and faults) during sequential access of large mmap'd files and
	// /dev/shm segments.
	const maxTranslateBytes = 2 << 20 // 2 MiB
	translateRange := maxOptionalRange(required, optional, maxTranslateBytes)
	if !rf.cow.IsEmpty() {
		// Do not speculatively translate pages that have not detached yet.
		translateRange = required
	}

	var ts []memmap.Translation
	var translatedEnd uint64
	for seg := rf.data.FindSegment(required.Start); seg.Ok() && seg.Start() < required.End; seg, _ = seg.NextNonEmpty() {
		segMR := seg.Range().Intersect(translateRange)
		ts = append(ts, memmap.Translation{
			Source: segMR,
			File:   rf.inode.fs.mf,
			Offset: seg.FileRangeOf(segMR).Start,
			Perms:  hostarch.AnyAccess,
		})
		translatedEnd = segMR.End
	}

	// Don't return the error returned by f.data.Fill if it occurred outside of
	// required.
	if translatedEnd < required.End && cerr != nil {
		return ts, &memmap.BusError{cerr}
	}
	if beyondEOF {
		return ts, &memmap.BusError{io.EOF}
	}
	return ts, nil
}

// maxOptionalRange returns the largest subrange of optional that contains
// required and is at most maxBytes long, or required itself if it already
// exceeds maxBytes. It is used to bound how far Translate fills/translates
// ahead of required. This applies the same logic as gofer.maxFillRange().
//
// Preconditions: optional.IsSupersetOf(required).
func maxOptionalRange(required, optional memmap.MappableRange, maxBytes uint64) memmap.MappableRange {
	if required.Length() >= maxBytes {
		return required
	}
	if optional.Length() <= maxBytes {
		return optional
	}
	optional.Start = required.Start
	if optional.Length() <= maxBytes {
		return optional
	}
	optional.End = optional.Start + maxBytes
	return optional
}

// InvalidateUnsavable implements memmap.Mappable.InvalidateUnsavable.
func (*regularFile) InvalidateUnsavable(context.Context) error {
	return nil
}

// +stateify savable
type regularFileFD struct {
	fileDescription

	// off is the file offset. off is accessed using atomic memory operations.
	// offMu serializes operations that may mutate off.
	off   int64
	offMu sync.Mutex `state:"nosave"`
}

// Release implements vfs.FileDescriptionImpl.Release.
func (fd *regularFileFD) Release(context.Context) {
	// noop
}

// Allocate implements vfs.FileDescriptionImpl.Allocate.
func (fd *regularFileFD) Allocate(ctx context.Context, mode, offset, length uint64) error {
	f := fd.inode().impl.(*regularFile)

	// To be consistent with Linux, inode.mu must be locked throughout.
	f.inode.mu.Lock()
	defer f.inode.mu.Unlock()
	end := offset + length
	if end < offset {
		return linuxerr.EFBIG
	}
	if f.inode.fs.diskVolume != nil {
		oldSize := f.size.Load()
		cloned, err := f.inode.fs.diskVolume.Repository.Allocate(
			f.inode.fs.diskVolume.Branch, f.inode.ino, int64(offset), int64(length))
		if err != nil {
			return err
		}
		if end > f.size.Load() {
			f.size.Store(end)
		}
		f.inode.fs.adjustDiskBytes(oldSize, f.size.Load())
		if cloned {
			f.invalidateDiskObject()
		}
		f.inode.touchCMtimeLocked()
		return f.inode.fs.persistDiskMetadata(&f.inode)
	}
	memCgID := pgalloc.MemoryCgroupIDFromContext(ctx)
	pgEnd, ok := hostarch.PageRoundUp(end)
	if !ok {
		return linuxerr.EFBIG
	}
	// Allocate in chunks for the following reasons:
	// 1. Size limit may permit really large fallocate, which can take a long
	//    time to execute on the host. This can cause watchdog to timeout and
	//    crash the system. Watchdog needs petting.
	// 2. Linux allocates folios iteratively while checking for interrupts. In
	//    gVisor, we need to manually check for interrupts between chunks.
	const chunkSize = 4 << 30 // 4 GiB
	for curPgStart := hostarch.PageRoundDown(offset); curPgStart < pgEnd; {
		curPgEnd := pgEnd
		newSize := end
		if curPgEnd-curPgStart > chunkSize {
			curPgEnd = curPgStart + chunkSize
			newSize = curPgEnd
		}
		required := memmap.MappableRange{Start: curPgStart, End: curPgEnd}
		if err := f.allocateLocked(ctx, mode, newSize, required, memCgID); err != nil {
			return err
		}
		// This loop can take a long time to process, so periodically check for
		// interrupts. This also pets the watchdog.
		if ctx.Interrupted() {
			return linuxerr.EINTR
		}
		// Advance curPgStart.
		curPgStart = curPgEnd
	}
	return nil
}

// Preconditions:
// - rf.inode.mu is locked.
// - required must be page-aligned.
// - required.Start < newSize <= required.End.
func (rf *regularFile) allocateLocked(ctx context.Context, mode, newSize uint64, required memmap.MappableRange, memCgID uint32) error {
	rf.dataMu.Lock()
	defer rf.dataMu.Unlock()

	// We must allocate pages in the range specified by offset and length.
	// Even if newSize <= oldSize, there might not be actual memory backing this
	// range, so any gaps must be filled by calling f.data.Fill().
	// "After a successful call, subsequent writes into the range
	// specified by offset and len are guaranteed not to fail because of
	// lack of disk space."  - fallocate(2)
	pagesToFill := rf.data.PagesToFill(required, required)
	if !rf.inode.fs.accountPages(pagesToFill) {
		return linuxerr.ENOSPC
	}
	// Given our definitions in pgalloc, fallocate(2) semantics imply that pages
	// in the MemoryFile must be committed, in addition to being allocated.
	allocMode := pgalloc.AllocateAndCommit
	if !rf.inode.fs.mf.IsDiskBacked() {
		// Upgrade to AllocateAndWritePopulate for memory(shmem)-backed files. We
		// take a more aggressive approach in populating pages for memory-backed
		// MemoryFiles. shmem pages are subject to swap rather than disk writeback.
		// They are not likely to be swapped before they are written to. Hence it
		// is beneficial to populate (in addition to commit) shmem pages to avoid
		// faulting page-by-page when these pages are written to in the future.
		allocMode = pgalloc.AllocateAndWritePopulate
	}
	pagesAlloced, err := rf.data.Fill(ctx, required, required, newSize, rf.inode.fs.mf, pgalloc.AllocOpts{
		Kind:    rf.memoryUsageKind,
		MemCgID: memCgID,
		Mode:    allocMode,
		Huge:    rf.huge && rf.inode.fs.mf.HugepagesEnabled(),
	}, nil /* r */)
	// f.data.Fill() may fail mid-way. We still want to account any pages that
	// were allocated, irrespective of an error.
	rf.inode.fs.adjustPageAcct(pagesToFill, pagesAlloced)
	if err != nil && err != io.EOF {
		return err
	}

	oldSize := rf.size.Load()
	if oldSize >= newSize {
		return nil
	}
	return rf.growLocked(newSize)
}

// PRead implements vfs.FileDescriptionImpl.PRead.
func (fd *regularFileFD) PRead(ctx context.Context, dst usermem.IOSequence, offset int64, opts vfs.ReadOptions) (int64, error) {
	start := fsmetric.StartReadWait()
	defer fsmetric.FinishReadWait(fsmetric.TmpfsReadWait, start)
	fsmetric.TmpfsReads.Increment()

	if offset < 0 {
		return 0, linuxerr.EINVAL
	}

	// Check that flags are supported. RWF_DSYNC/RWF_SYNC can be ignored since
	// all state is in-memory.
	//
	// TODO(gvisor.dev/issue/2601): Support select preadv2 flags.
	if opts.Flags&^(linux.RWF_HIPRI|linux.RWF_DSYNC|linux.RWF_SYNC) != 0 {
		return 0, linuxerr.EOPNOTSUPP
	}

	if dst.NumBytes() == 0 {
		return 0, nil
	}
	f := fd.inode().impl.(*regularFile)
	if f.inode.fs.diskVolume != nil {
		file, _, _, err := f.inode.fs.diskVolume.Repository.OpenDataFile(f.inode.fs.diskVolume.Branch, f.inode.ino, false)
		if err != nil {
			return 0, err
		}
		defer file.Close()
		const maxBounce = 64 << 10
		buf := make([]byte, min(dst.NumBytes(), int64(maxBounce)))
		var done int64
		for dst.NumBytes() != 0 {
			want := min(dst.NumBytes(), int64(len(buf)))
			n, readErr := file.ReadAt(buf[:want], offset+done)
			if n != 0 {
				copied, copyErr := dst.TakeFirst64(int64(n)).CopyOut(ctx, buf[:n])
				done += int64(copied)
				dst = dst.DropFirst(copied)
				if copyErr != nil {
					return done, copyErr
				}
				if copied != n {
					return done, io.ErrShortWrite
				}
			}
			if readErr != nil {
				if readErr == io.EOF && done != 0 {
					readErr = nil
				}
				oldAtime := f.inode.atime.Load()
				f.inode.touchAtime(fd.vfsfd.Mount())
				if f.inode.atime.Load() != oldAtime {
					_ = f.inode.fs.persistDiskMetadata(&f.inode)
				}
				return done, readErr
			}
			if n == 0 {
				return done, io.ErrNoProgress
			}
		}
		oldAtime := f.inode.atime.Load()
		f.inode.touchAtime(fd.vfsfd.Mount())
		if f.inode.atime.Load() != oldAtime {
			_ = f.inode.fs.persistDiskMetadata(&f.inode)
		}
		return done, nil
	}
	// memCgID can be 0 here because regularFileReadWriter.ReadToBlocks() never
	// allocates from pgalloc.
	rw := getRegularFileReadWriter(f, offset, 0)
	n, err := dst.CopyOutFrom(ctx, rw)
	putRegularFileReadWriter(rw)
	fd.inode().touchAtime(fd.vfsfd.Mount())
	return n, err
}

// Read implements vfs.FileDescriptionImpl.Read.
func (fd *regularFileFD) Read(ctx context.Context, dst usermem.IOSequence, opts vfs.ReadOptions) (int64, error) {
	fd.offMu.Lock()
	n, err := fd.PRead(ctx, dst, fd.off, opts)
	fd.off += n
	fd.offMu.Unlock()
	return n, err
}

// PWrite implements vfs.FileDescriptionImpl.PWrite.
func (fd *regularFileFD) PWrite(ctx context.Context, src usermem.IOSequence, offset int64, opts vfs.WriteOptions) (int64, error) {
	n, _, err := fd.pwrite(ctx, src, offset, opts)
	return n, err
}

// pwrite returns the number of bytes written, final offset and error. The
// final offset should be ignored by PWrite.
func (fd *regularFileFD) pwrite(ctx context.Context, src usermem.IOSequence, offset int64, opts vfs.WriteOptions) (written, finalOff int64, err error) {
	if offset < 0 {
		return 0, offset, linuxerr.EINVAL
	}

	// Check that flags are supported. RWF_DSYNC/RWF_SYNC can be ignored since
	// all state is in-memory.
	//
	// TODO(gvisor.dev/issue/2601): Support select preadv2 flags.
	if opts.Flags&^(linux.RWF_HIPRI|linux.RWF_DSYNC|linux.RWF_SYNC) != 0 {
		return 0, offset, linuxerr.EOPNOTSUPP
	}

	srclen := src.NumBytes()
	if srclen == 0 {
		return 0, offset, nil
	}
	f := fd.inode().impl.(*regularFile)
	f.inode.mu.Lock()
	defer f.inode.mu.Unlock()
	// If the file is opened with O_APPEND, update offset to file size.
	if fd.vfsfd.StatusFlags()&linux.O_APPEND != 0 {
		// Locking f.inode.mu is sufficient for reading f.size.
		offset = int64(f.size.RacyLoad())
	}
	end := offset + srclen
	if end < offset {
		// Overflow.
		return 0, offset, linuxerr.EINVAL
	}

	srclen, err = vfs.CheckLimit(ctx, offset, srclen)
	if err != nil {
		return 0, offset, err
	}
	src = src.TakeFirst64(srclen)

	if f.inode.fs.diskVolume != nil {
		oldSize := f.size.Load()
		file, _, cloned, err := f.inode.fs.diskVolume.Repository.OpenDataFile(
			f.inode.fs.diskVolume.Branch, f.inode.ino, true)
		if err != nil {
			return 0, offset, err
		}
		defer file.Close()
		if cloned {
			f.invalidateDiskObject()
		}
		const maxBounce = 64 << 10
		buf := make([]byte, min(srclen, int64(maxBounce)))
		var done int64
		for src.NumBytes() != 0 {
			want := min(src.NumBytes(), int64(len(buf)))
			n, readErr := src.TakeFirst64(want).CopyIn(ctx, buf[:want])
			if n != 0 {
				written, writeErr := file.WriteAt(buf[:n], offset+done)
				done += int64(written)
				src = src.DropFirst(written)
				if writeErr != nil {
					return done, offset + done, writeErr
				}
				if written != n {
					return done, offset + done, io.ErrShortWrite
				}
			}
			if readErr != nil {
				return done, offset + done, readErr
			}
			if n == 0 {
				return done, offset + done, io.ErrNoProgress
			}
		}
		if end := uint64(offset + done); end > f.size.Load() {
			f.size.Store(end)
		}
		f.inode.fs.adjustDiskBytes(oldSize, f.size.Load())
		f.inode.touchCMtimeLocked()
		for {
			old := f.inode.mode.Load()
			if f.inode.mode.CompareAndSwap(old, vfs.ClearSUIDAndSGID(old)) {
				break
			}
		}
		f.inode.xattrs.KillPriv()
		status := fd.vfsfd.StatusFlags()
		if opts.Flags&(linux.RWF_DSYNC|linux.RWF_SYNC) != 0 || status&(linux.O_DSYNC|linux.O_SYNC) != 0 {
			dataOnly := opts.Flags&linux.RWF_SYNC == 0 && status&linux.O_SYNC == 0
			var err error
			if dataOnly {
				err = unix.Fdatasync(int(file.Fd()))
			} else {
				err = file.Sync()
			}
			if err != nil {
				return done, offset + done, err
			}
		}
		if err := f.inode.fs.persistDiskWriteMetadata(&f.inode); err != nil {
			return done, offset + done, err
		}
		return done, offset + done, nil
	}

	// Perform the write.
	rw := getRegularFileReadWriter(f, offset, pgalloc.MemoryCgroupIDFromContext(ctx))
	pageEnd, _ := hostarch.Addr(end).RoundUp()
	writeRange := memmap.MappableRange{Start: uint64(hostarch.Addr(offset).RoundDown()), End: uint64(pageEnd)}
	f.dataMu.RLock()
	shared := !f.cow.IsEmptyRange(writeRange)
	f.dataMu.RUnlock()
	var n int64
	if shared {
		// WriteFromBlocks must invalidate this file's mappings before it
		// detaches live-fork-shared pages. CopyInTo may call WriteFromBlocks
		// while holding the source MemoryManager's activeMu for reading. If
		// this file is mapped in that same MemoryManager, invalidation needs
		// activeMu for writing and deadlocks recursively. Bounce only CoW
		// writes so all source-MM locks are released before invalidation.
		n, err = copyInToWithBounce(ctx, src, rw)
	} else {
		n, err = src.CopyInTo(ctx, rw)
	}

	f.inode.touchCMtimeLocked()
	for {
		old := f.inode.mode.Load()
		new := vfs.ClearSUIDAndSGID(old)
		if swapped := f.inode.mode.CompareAndSwap(old, new); swapped {
			break
		}
	}
	f.inode.xattrs.KillPriv()
	putRegularFileReadWriter(rw)
	return n, n + offset, err
}

// copyInToWithBounce copies src to dst without calling dst while src's
// user-memory implementation may hold MemoryManager locks.
func copyInToWithBounce(ctx context.Context, src usermem.IOSequence, dst safemem.Writer) (int64, error) {
	const maxBounce = 64 << 10
	buf := make([]byte, min(src.NumBytes(), maxBounce))
	var done int64
	for src.NumBytes() != 0 {
		want := min(src.NumBytes(), int64(len(buf)))
		n, readErr := src.TakeFirst64(want).CopyIn(ctx, buf[:want])
		if n != 0 {
			written, writeErr := dst.WriteFromBlocks(safemem.BlockSeqOf(safemem.BlockFromSafeSlice(buf[:n])))
			done += int64(written)
			if writeErr != nil {
				return done, writeErr
			}
			if written != uint64(n) {
				// safemem.Writer permits a partial write without an error, and
				// regularFileReadWriter uses that for partial ENOSPC writes.
				return done, nil
			}
			src = src.DropFirst(n)
		}
		if readErr != nil {
			return done, readErr
		}
		if n == 0 {
			return done, io.ErrNoProgress
		}
	}
	return done, nil
}

// Write implements vfs.FileDescriptionImpl.Write.
func (fd *regularFileFD) Write(ctx context.Context, src usermem.IOSequence, opts vfs.WriteOptions) (int64, error) {
	fd.offMu.Lock()
	n, off, err := fd.pwrite(ctx, src, fd.off, opts)
	fd.off = off
	fd.offMu.Unlock()
	return n, err
}

// Seek implements vfs.FileDescriptionImpl.Seek.
func (fd *regularFileFD) Seek(ctx context.Context, offset int64, whence int32) (int64, error) {
	fd.offMu.Lock()
	defer fd.offMu.Unlock()
	switch whence {
	case linux.SEEK_SET:
		// use offset as specified
	case linux.SEEK_CUR:
		offset += fd.off
	case linux.SEEK_END:
		offset += int64(fd.inode().impl.(*regularFile).size.Load())
	default:
		return 0, linuxerr.EINVAL
	}
	if offset < 0 {
		return 0, linuxerr.EINVAL
	}
	fd.off = offset
	return offset, nil
}

// ConfigureMMap implements vfs.FileDescriptionImpl.ConfigureMMap.
func (fd *regularFileFD) ConfigureMMap(ctx context.Context, opts *memmap.MMapOpts) error {
	file := fd.inode().impl.(*regularFile)
	opts.SentryOwnedContent = true
	if file.initiallyUnlinked {
		opts.NameMut = memmap.NameMutAnonShmem
	}
	return vfs.GenericConfigureMMap(&fd.vfsfd, file, opts)
}

// offsetPageEnd returns the file offset rounded up to the nearest
// page boundary. offsetPageEnd panics if rounding up causes overflow,
// which shouldn't be possible given that offset is an int64.
func offsetPageEnd(offset int64) uint64 {
	end, ok := hostarch.Addr(offset).RoundUp()
	if !ok {
		panic("impossible overflow")
	}
	return uint64(end)
}

// regularFileReadWriter implements safemem.Reader and Safemem.Writer.
type regularFileReadWriter struct {
	file *regularFile

	// Offset into the file to read/write at. Note that this may be
	// different from the FD offset if PRead/PWrite is used.
	off uint64

	// memCgID is the memory cgroup ID used for accounting the allocated
	// pages.
	memCgID uint32
}

var regularFileReadWriterPool = sync.Pool{
	New: func() any {
		return &regularFileReadWriter{}
	},
}

func getRegularFileReadWriter(file *regularFile, offset int64, memCgID uint32) *regularFileReadWriter {
	rw := regularFileReadWriterPool.Get().(*regularFileReadWriter)
	rw.file = file
	rw.off = uint64(offset)
	rw.memCgID = memCgID
	return rw
}

func putRegularFileReadWriter(rw *regularFileReadWriter) {
	rw.file = nil
	regularFileReadWriterPool.Put(rw)
}

// ReadToBlocks implements safemem.Reader.ReadToBlocks.
func (rw *regularFileReadWriter) ReadToBlocks(dsts safemem.BlockSeq) (uint64, error) {
	rw.file.dataMu.RLock()
	defer rw.file.dataMu.RUnlock()
	size := rw.file.size.RacyLoad()

	// Compute the range to read (limited by file size and overflow-checked).
	if rw.off >= size {
		return 0, io.EOF
	}
	end := size
	if rend := rw.off + dsts.NumBytes(); rend > rw.off && rend < end {
		end = rend
	}

	var done uint64
	seg, gap := rw.file.data.Find(uint64(rw.off))
	for rw.off < end {
		mr := memmap.MappableRange{uint64(rw.off), uint64(end)}
		switch {
		case seg.Ok():
			// Get internal mappings.
			ims, err := rw.file.inode.fs.mf.MapInternal(seg.FileRangeOf(seg.Range().Intersect(mr)), hostarch.Read)
			if err != nil {
				return done, err
			}

			// Copy from internal mappings.
			n, err := safemem.CopySeq(dsts, ims)
			done += n
			rw.off += uint64(n)
			dsts = dsts.DropFirst64(n)
			if err != nil {
				return done, err
			}

			// Continue.
			seg, gap = seg.NextNonEmpty()

		case gap.Ok():
			// Tmpfs holes are zero-filled.
			gapmr := gap.Range().Intersect(mr)
			dst := dsts.TakeFirst64(gapmr.Length())
			n, err := safemem.ZeroSeq(dst)
			done += n
			rw.off += uint64(n)
			dsts = dsts.DropFirst64(n)
			if err != nil {
				return done, err
			}

			// Continue.
			seg, gap = gap.NextSegment(), fsutil.FileRangeGapIterator{}
		}
	}
	return done, nil
}

// WriteFromBlocks implements safemem.Writer.WriteFromBlocks.
//
// Preconditions: rw.file.inode.mu must be held.
func (rw *regularFileReadWriter) WriteFromBlocks(srcs safemem.BlockSeq) (uint64, error) {
	if srcs.IsEmpty() {
		return 0, nil
	}

	// Compute the range to write (overflow-checked).
	end := rw.off + srcs.NumBytes()
	if end <= rw.off {
		end = math.MaxInt64
	}
	initialPageEnd, _ := hostarch.Addr(end).RoundUp()
	initialRange := memmap.MappableRange{uint64(hostarch.Addr(rw.off).RoundDown()), uint64(initialPageEnd)}
	rw.file.mapsMu.Lock()
	rw.file.dataMu.RLock()
	shared := !rw.file.cow.IsEmptyRange(initialRange)
	rw.file.dataMu.RUnlock()
	if shared {
		rw.file.mappings.Invalidate(initialRange, memmap.InvalidateOpts{InvalidatePrivate: true})
	}
	rw.file.dataMu.Lock()
	defer rw.file.dataMu.Unlock()
	defer rw.file.mapsMu.Unlock()

	// Check if seals prevent either file growth or all writes.
	switch {
	case rw.file.seals&linux.F_SEAL_WRITE != 0: // Write sealed
		return 0, linuxerr.EPERM
	case end > rw.file.size.RacyLoad() && rw.file.seals&linux.F_SEAL_GROW != 0: // Grow sealed
		// When growth is sealed, Linux effectively allows writes which would
		// normally grow the file to partially succeed up to the current EOF,
		// rounded down to the page boundary before the EOF.
		//
		// This happens because writes (and thus the growth check) for tmpfs
		// files proceed page-by-page on Linux, and the final write to the page
		// containing EOF fails, resulting in a partial write up to the start of
		// that page.
		//
		// To emulate this behaviour, artificially truncate the write to the
		// start of the page containing the current EOF.
		//
		// See Linux, mm/filemap.c:generic_perform_write() and
		// mm/shmem.c:shmem_write_begin().
		if pgstart := uint64(hostarch.Addr(rw.file.size.RacyLoad()).RoundDown()); end > pgstart {
			end = pgstart
		}
		if end <= rw.off {
			// Truncation would result in no data being written.
			return 0, linuxerr.EPERM
		}
	}

	// Page-aligned mr for when we need to allocate memory. RoundUp can't
	// overflow since end is an int64.
	pgstartaddr := hostarch.Addr(rw.off).RoundDown()
	pgendaddr, _ := hostarch.Addr(end).RoundUp()
	pgMR := memmap.MappableRange{uint64(pgstartaddr), uint64(pgendaddr)}
	if err := rw.file.copyOnWriteLocked(context.Background(), pgMR); err != nil {
		return 0, err
	}
	fs := rw.file.inode.fs
	mayHuge := rw.file.huge && fs.mf.HugepagesEnabled()

	var (
		done   uint64
		retErr error
	)
	seg, gap := rw.file.data.Find(uint64(rw.off))
	for rw.off < end {
		mr := memmap.MappableRange{uint64(rw.off), uint64(end)}
		switch {
		case seg.Ok():
			n, err := rw.writeToMF(seg.FileRangeOf(seg.Range().Intersect(mr)), srcs)
			done += n
			rw.off += uint64(n)
			srcs = srcs.DropFirst64(n)
			if err != nil {
				retErr = err
				goto exitLoop
			}

			// Continue.
			seg, gap = seg.NextNonEmpty()

		case gap.Ok():
			// Allocate memory for the write.
			gapMR := gap.Range().Intersect(pgMR)
			pagesToFill := gapMR.Length() / hostarch.PageSize
			pagesReserved := fs.accountPagesPartial(pagesToFill)
			if pagesReserved == 0 {
				if done == 0 {
					retErr = linuxerr.ENOSPC
					goto exitLoop
				}
				retErr = nil
				goto exitLoop
			}
			gapMR.End = gapMR.Start + (hostarch.PageSize * pagesReserved)
			allocMode := pgalloc.AllocateAndWritePopulate
			if fs.mf.IsDiskBacked() {
				// Don't populate pages for disk-backed files. Benchmarking showed that
				// disk-backed pages are likely to be written back to disk before we
				// can write to them. The pages fault again on write anyways. In total,
				// prepopulating disk-backed pages deteriorates performance as it fails
				// to eliminate future page faults and we also additionally incur
				// useless disk writebacks.
				allocMode = pgalloc.AllocateCallerIndirectCommit
			}
			fr, err := fs.mf.Allocate(gapMR.Length(), pgalloc.AllocOpts{
				Kind:    rw.file.memoryUsageKind,
				MemCgID: rw.memCgID,
				Mode:    allocMode,
				// TODO: If mayHuge is true and gap spans at least one aligned
				// hugepage, but either start or end are not hugepage-aligned,
				// consider allocating small pages on either end and huge pages
				// in the middle.
				Huge: mayHuge && hostarch.IsHugePageAligned(gapMR.Start) && hostarch.IsHugePageAligned(gapMR.End),
			})
			if err != nil {
				retErr = err
				fs.unaccountPages(pagesReserved)
				goto exitLoop
			}

			// Write to that memory as usual.
			seg, gap = rw.file.data.Insert(gap, gapMR, fr.Start), fsutil.FileRangeGapIterator{}
		default:
			panic("unreachable")
		}
	}
exitLoop:
	// If the write ends beyond the file's previous size, it causes the
	// file to grow.
	if rw.off > rw.file.size.RacyLoad() {
		rw.file.size.Store(rw.off)
	}

	return done, retErr
}

func (rw *regularFileReadWriter) writeToMF(fr memmap.FileRange, srcs safemem.BlockSeq) (uint64, error) {
	if rw.file.inode.fs.mf.IsDiskBacked() {
		// Disk-backed files are not prepopulated. The safemem.CopySeq() approach
		// used below incurs a lot of page faults without page prepopulation, which
		// causes a lot of context switching. Use write(2) host syscall instead,
		// which makes one context switch and faults all the pages that are touched
		// during the write.
		fd, err := rw.file.inode.fs.mf.DataFD(fr)
		if err != nil {
			return 0, err
		}
		return hostfd.Pwritev2(
			int32(fd),                     // fd
			srcs.TakeFirst64(fr.Length()), // srcs
			int64(fr.Start),               // offset
			0,                             // flags
		)
	}
	// Get internal mappings.
	ims, err := rw.file.inode.fs.mf.MapInternal(fr, hostarch.Write)
	if err != nil {
		return 0, err
	}
	// Copy to internal mappings.
	return safemem.CopySeq(ims, srcs)
}

// GetSeals returns the current set of seals on a memfd inode.
func GetSeals(fd *vfs.FileDescription) (uint32, error) {
	f, ok := fd.Impl().(*regularFileFD)
	if !ok {
		return 0, linuxerr.EINVAL
	}
	rf := f.inode().impl.(*regularFile)
	rf.dataMu.RLock()
	defer rf.dataMu.RUnlock()
	return rf.seals, nil
}

// AddSeals adds new file seals to a memfd inode.
func AddSeals(fd *vfs.FileDescription, val uint32) error {
	f, ok := fd.Impl().(*regularFileFD)
	if !ok {
		return linuxerr.EINVAL
	}
	rf := f.inode().impl.(*regularFile)
	rf.mapsMu.Lock()
	defer rf.mapsMu.Unlock()
	rf.dataMu.Lock()
	defer rf.dataMu.Unlock()

	if rf.seals&linux.F_SEAL_SEAL != 0 {
		// Seal applied which prevents addition of any new seals.
		return linuxerr.EPERM
	}

	// F_SEAL_WRITE can only be added if there are no active writable maps.
	if rf.seals&linux.F_SEAL_WRITE == 0 && val&linux.F_SEAL_WRITE != 0 {
		if rf.writableMappingPages > 0 {
			return linuxerr.EBUSY
		}
	}

	// Seals can only be added, never removed.
	rf.seals |= val
	return nil
}
