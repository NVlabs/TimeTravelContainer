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
	goContext "context"
	"fmt"
	"io"
	"math"
	"sync"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/fsutil"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/usermem"
)

// fileCache is shared by all descriptions and hard links for a regular FUSE
// inode. This is required for Linux MAP_SHARED coherence.
//
// +stateify savable
type fileCache struct {
	// mapsMu protects mappings.
	mapsMu   sync.Mutex `state:"nosave"`
	mappings memmap.MappingSet

	// dataMu protects data and dirty.
	dataMu sync.RWMutex `state:"nosave"`
	data   fsutil.FileRangeSet
	dirty  fsutil.DirtySet

	// cow contains data ranges whose MemoryFile pages are still shared with
	// another live-fork branch. Values mirror data's MemoryFile offsets.
	cow fsutil.FileRangeSet

	// handles is reconstructed from open regularFileFDs after restore and is
	// used to select a writable FUSE handle for inode-wide page writeback.
	handlesMu sync.Mutex                  `state:"nosave"`
	handles   map[*regularFileFD]struct{} `state:"nosave"`
}

func (cache *fileCache) afterLoad(ctx goContext.Context) {
	mf := pgalloc.MemoryFileFromContext(ctx)
	if err := fsutil.RelocateFileRangeSet(ctx, mf, &cache.data); err != nil {
		panic("relocating FUSE mmap cache: " + err.Error())
	}
	if err := fsutil.RelocateFileRangeSet(ctx, mf, &cache.cow); err != nil {
		panic("relocating FUSE mmap CoW metadata: " + err.Error())
	}
	if cache.handles == nil {
		cache.handles = make(map[*regularFileFD]struct{})
	}
	if !pgalloc.LiveForkCloneNeedsPageRefs(ctx) {
		return
	}
	memCgID := pgalloc.MemoryCgroupIDFromContext(ctx)
	for seg := cache.data.FirstSegment(); seg.Ok(); seg = seg.NextSegment() {
		pgalloc.AcquireRestoreRef(ctx, mf, seg.FileRange(), memCgID)
	}
}

func (cache *fileCache) register(fd *regularFileFD) {
	cache.handlesMu.Lock()
	if cache.handles == nil {
		cache.handles = make(map[*regularFileFD]struct{})
	}
	cache.handles[fd] = struct{}{}
	cache.handlesMu.Unlock()
}

func (cache *fileCache) unregister(fd *regularFileFD) {
	cache.handlesMu.Lock()
	delete(cache.handles, fd)
	cache.handlesMu.Unlock()
}

func (cache *fileCache) writableHandle(preferred *regularFileFD) (*regularFileFD, bool) {
	if preferred != nil && preferred.statusFlags()&linux.O_ACCMODE != linux.O_RDONLY {
		return preferred, false
	}
	cache.handlesMu.Lock()
	defer cache.handlesMu.Unlock()
	for fd := range cache.handles {
		if fd.statusFlags()&linux.O_ACCMODE == linux.O_RDONLY {
			continue
		}
		if !fd.vfsfd.TryIncRef() {
			delete(cache.handles, fd)
			continue
		}
		return fd, true
	}
	return nil, false
}

func (cache *fileCache) dropAll(mf *pgalloc.MemoryFile) {
	cache.dataMu.Lock()
	cache.data.DropAll(mf)
	cache.cow.RemoveAll()
	cache.dirty.RemoveAllAndAccount()
	cache.dataMu.Unlock()
}

func (cache *fileCache) truncate(newSize, oldSize uint64, mf *pgalloc.MemoryFile) {
	if newSize >= oldSize {
		return
	}
	oldPageEnd, _ := hostarch.PageRoundUp(oldSize)
	newPageEnd, _ := hostarch.PageRoundUp(newSize)
	if oldPageEnd != newPageEnd {
		cache.mapsMu.Lock()
		cache.mappings.Invalidate(memmap.MappableRange{Start: uint64(newPageEnd), End: uint64(oldPageEnd)}, memmap.InvalidateOpts{InvalidatePrivate: true})
		cache.mapsMu.Unlock()
	}
	cache.dataMu.Lock()
	cache.data.Truncate(newSize, mf)
	if oldPageEnd != newPageEnd {
		truncated := memmap.MappableRange{Start: uint64(newPageEnd), End: uint64(oldPageEnd)}
		cache.cow.RemoveRange(truncated)
		cache.dirty.KeepClean(truncated)
	}
	cache.dataMu.Unlock()
}

// prepareLiveFork revokes current translations and records every cached page
// as shared with the impending child branch.
func (cache *fileCache) prepareLiveFork() {
	cache.mapsMu.Lock()
	cache.mappings.InvalidateAll(memmap.InvalidateOpts{})
	cache.dataMu.Lock()
	cache.cow.RemoveAll()
	for seg := cache.data.FirstSegment(); seg.Ok(); seg = seg.NextSegment() {
		cache.cow.Insert(cache.cow.FindGap(seg.Start()), seg.Range(), seg.Value())
	}
	cache.dataMu.Unlock()
	cache.mapsMu.Unlock()
}

// copyOnWriteLocked gives this branch private MemoryFile pages for shared
// cached ranges before they are translated again.
//
// Preconditions: cache.dataMu is locked for writing and mr is page-aligned.
func (cache *fileCache) copyOnWriteLocked(ctx context.Context, fs *filesystem, mr memmap.MappableRange) error {
	memCgID := pgalloc.MemoryCgroupIDFromContext(ctx)
	for cowSeg := cache.cow.LowerBoundSegment(mr.Start); cowSeg.Ok() && cowSeg.Start() < mr.End; {
		if cowSeg.End() <= mr.Start {
			cowSeg = cowSeg.NextSegment()
			continue
		}
		cowRange := cowSeg.Range().Intersect(mr)
		cowSeg = cache.cow.Isolate(cowSeg, cowRange)
		dataSeg := cache.data.FindSegment(cowRange.Start)
		if !dataSeg.Ok() || !dataSeg.Range().IsSupersetOf(cowRange) {
			return fmt.Errorf("FUSE live-fork CoW range %v has no cache segment", cowRange)
		}
		dataSeg = cache.data.Isolate(dataSeg, cowRange)
		oldRange := dataSeg.FileRange()
		newRange, err := fs.mf.Allocate(cowRange.Length(), pgalloc.AllocOpts{
			Kind:    usage.PageCache,
			MemCgID: memCgID,
			Mode:    pgalloc.AllocateAndWritePopulate,
		})
		if err != nil {
			return err
		}
		mappings, mappingLease, err := memmap.MapInternalBatchWithLease(fs.mf, []memmap.InternalMappingRequest{
			{Range: oldRange, Access: hostarch.Read},
			{Range: newRange, Access: hostarch.Write},
		})
		if err != nil {
			fs.mf.DecRef(newRange)
			return err
		}
		oldIMS, newIMS := mappings[0], mappings[1]
		n, copyErr := safemem.CopySeq(newIMS, oldIMS)
		if mappingLease != nil {
			mappingLease.Release()
		}
		if copyErr != nil || n != cowRange.Length() {
			fs.mf.DecRef(newRange)
			if copyErr != nil {
				return copyErr
			}
			return io.ErrUnexpectedEOF
		}
		dataSeg.SetValue(newRange.Start)
		fs.mf.DecRef(oldRange)
		gap := cache.cow.Remove(cowSeg)
		if !gap.Ok() {
			break
		}
		cowSeg = gap.NextSegment()
	}
	return nil
}

// +stateify savable
type regularFileFD struct {
	fileDescription

	// offMu protects off.
	offMu sync.Mutex `state:"nosave"`

	// off is the file offset.
	// +checklocks:offMu
	off int64
}

// afterLoad is invoked by stateify.
func (fd *regularFileFD) afterLoad(ctx goContext.Context) {
	fd.inode().cache.register(fd)
}

// Release implements vfs.FileDescriptionImpl.Release.
func (fd *regularFileFD) Release(ctx context.Context) {
	fd.inode().cache.unregister(fd)
	if err := fd.writebackAll(ctx); err != nil {
		ctx.Warningf("writing back FUSE mmap data on release: %v", err)
	}
	fd.fileDescription.Release(ctx)
}

// OnClose implements vfs.FileDescriptionImpl.OnClose.
func (fd *regularFileFD) OnClose(ctx context.Context) error {
	if err := fd.writebackAll(ctx); err != nil {
		return err
	}
	return fd.fileDescription.OnClose(ctx)
}

// Sync implements vfs.FileDescriptionImpl.Sync.
func (fd *regularFileFD) Sync(ctx context.Context, opts vfs.SyncOptions) error {
	if err := fd.writebackAll(ctx); err != nil {
		return err
	}
	return fd.fileDescription.Sync(ctx, opts)
}

func (fd *regularFileFD) writebackAll(ctx context.Context) error {
	writer, decRef := fd.inode().cache.writableHandle(fd)
	if writer == nil {
		return nil
	}
	if decRef {
		defer writer.vfsfd.DecRef(ctx)
	}
	return writer.writebackAllUsingHandle(ctx)
}

func (fd *regularFileFD) writebackAllUsingHandle(ctx context.Context) error {
	inode := fd.inode()
	inode.attrMu.Lock()
	size := inode.size.Load()
	inode.attrMu.Unlock()
	cache := inode.cache
	cache.dataMu.Lock()
	defer cache.dataMu.Unlock()
	return fsutil.SyncDirtyAll(ctx, &cache.data, &cache.dirty, size, inode.fs.mf, fd.writeFromBlocksAt)
}

// writebackRange writes dirty cached pages in mr. The caller supplies a file
// size snapshot so that callers already holding inode.attrMu do not attempt to
// acquire it recursively.
func (fd *regularFileFD) writebackRange(ctx context.Context, mr memmap.MappableRange, size uint64) error {
	inode := fd.inode()
	cache := inode.cache
	cache.dataMu.Lock()
	defer cache.dataMu.Unlock()
	return fsutil.SyncDirty(ctx, mr, &cache.data, &cache.dirty, size, inode.fs.mf, fd.writeFromBlocksAt)
}

// writeFromBlocksAt writes cached mmap pages through the FUSE handle.
func (fd *regularFileFD) writeFromBlocksAt(ctx context.Context, srcs safemem.BlockSeq, offset uint64) (uint64, error) {
	fs := fd.inode().fs
	maxWrite := uint32(fs.conn.maxPages) << hostarch.PageShift
	if maxWrite > fs.conn.maxWrite {
		maxWrite = fs.conn.maxWrite
	}
	var done uint64
	for !srcs.IsEmpty() {
		writeSize := srcs.NumBytes()
		if writeSize > uint64(maxWrite) {
			writeSize = uint64(maxWrite)
		}
		if !fs.conn.bigWrites && writeSize > hostarch.PageSize {
			writeSize = hostarch.PageSize
		}
		data := make([]byte, writeSize)
		n, err := safemem.CopySeq(safemem.BlockSeqOf(safemem.BlockFromSafeSlice(data)), srcs.TakeFirst64(writeSize))
		if err != nil {
			return done, err
		}
		in := linux.FUSEWritePayloadIn{
			Header: linux.FUSEWriteIn{
				Fh:         fd.Fh,
				Offset:     offset + done,
				Size:       uint32(n),
				WriteFlags: linux.FUSE_WRITE_CACHE,
				Flags:      fd.statusFlags(),
			},
			Payload: data[:n],
		}
		var out linux.FUSEWriteOut
		if err := fd.inode().call(ctx, linux.FUSE_WRITE, &in, &out); err != nil {
			return done, err
		}
		if out.Size > uint32(n) {
			return done, linuxerr.EIO
		}
		done += uint64(out.Size)
		srcs = srcs.DropFirst64(uint64(out.Size))
		if out.Size != uint32(n) {
			return done, io.ErrShortWrite
		}
	}
	return done, nil
}

// Allocate implements vfs.FileDescriptionImpl.Allocate.
func (fd *regularFileFD) Allocate(ctx context.Context, mode, offset, length uint64) error {
	if mode&^uint64(linux.FALLOC_FL_KEEP_SIZE|linux.FALLOC_FL_PUNCH_HOLE|linux.FALLOC_FL_ZERO_RANGE) != 0 {
		return linuxerr.EOPNOTSUPP
	}
	in := linux.FUSEFallocateIn{
		Fh:     fd.Fh,
		Offset: offset,
		Length: length,
		Mode:   uint32(mode),
	}
	i := fd.inode()
	if err := i.call(ctx, linux.FUSE_FALLOCATE, &in, nil); err != nil {
		return err
	}
	i.attrMu.Lock()
	defer i.attrMu.Unlock()
	if mode&linux.FALLOC_FL_KEEP_SIZE == 0 {
		if newSize := offset + length; newSize > i.size.Load() {
			i.size.Store(newSize)
			i.fs.conn.attributeVersion.Add(1)
		}
	}
	i.touchCMtime()
	return nil
}

// Seek implements vfs.FileDescriptionImpl.Seek.
func (fd *regularFileFD) Seek(ctx context.Context, offset int64, whence int32) (int64, error) {
	fd.offMu.Lock()
	defer fd.offMu.Unlock()
	inode := fd.inode()
	inode.attrMu.Lock()
	defer inode.attrMu.Unlock()
	switch whence {
	case linux.SEEK_SET:
		// use offset as specified
	case linux.SEEK_CUR:
		offset += fd.off
	case linux.SEEK_END:
		offset += int64(inode.size.Load())
	default:
		return 0, linuxerr.EINVAL
	}
	if offset < 0 {
		return 0, linuxerr.EINVAL
	}
	fd.off = offset
	return offset, nil
}

// PRead implements vfs.FileDescriptionImpl.PRead.
func (fd *regularFileFD) PRead(ctx context.Context, dst usermem.IOSequence, offset int64, opts vfs.ReadOptions) (int64, error) {
	if offset < 0 {
		return 0, linuxerr.EINVAL
	}

	// Check that flags are supported.
	//
	// TODO(gvisor.dev/issue/2601): Support select preadv2 flags.
	if opts.Flags&^linux.RWF_HIPRI != 0 {
		return 0, linuxerr.EOPNOTSUPP
	}

	size := dst.NumBytes()
	if size == 0 {
		// Early return if count is 0.
		return 0, nil
	}
	if size > math.MaxUint32 {
		// FUSE only supports uint32 for size.
		// Overflow.
		return 0, linuxerr.EINVAL
	}

	// TODO(gvisor.dev/issue/3678): Add direct IO support.

	inode := fd.inode()
	inode.attrMu.Lock()

	// Reading beyond EOF, update file size if outdated.
	if uint64(offset+size) > inode.size.Load() {
		if err := inode.reviseAttr(ctx, linux.FUSE_GETATTR_FH, fd.Fh); err != nil {
			inode.attrMu.Unlock()
			return 0, err
		}
		// If the offset after update is still too large, return error.
		if uint64(offset) >= inode.size.Load() {
			inode.attrMu.Unlock()
			return 0, io.EOF
		}
	}

	// Truncate the read with updated file size.
	fileSize := inode.size.Load()
	if uint64(offset+size) > fileSize {
		size = int64(fileSize) - offset
	}
	inode.attrMu.Unlock()
	if fd.DirectIO {
		return fd.preadDirect(ctx, dst, uint64(offset), size)
	}

	reader := fuseCacheReader{ctx: ctx, fd: fd, off: uint64(offset), fileSize: fileSize}
	return dst.TakeFirst64(size).CopyOutFrom(ctx, &reader)
}

func (fd *regularFileFD) preadDirect(ctx context.Context, dst usermem.IOSequence, offset uint64, size int64) (int64, error) {
	buffers, n, err := fd.inode().fs.ReadInPages(ctx, fd, offset, uint32(size))
	if err != nil {
		return 0, err
	}
	if n < uint32(size) {
		size = int64(n)
	}
	var copied int64
	for _, buffer := range buffers {
		toCopy := int64(len(buffer))
		if copied+toCopy > size {
			toCopy = size - copied
		}
		cp, err := dst.DropFirst64(copied).CopyOut(ctx, buffer[:toCopy])
		if err != nil {
			return 0, err
		}
		if int64(cp) != toCopy {
			return 0, linuxerr.EIO
		}
		copied += toCopy
	}
	return copied, nil
}

type fuseCacheReader struct {
	ctx      context.Context
	fd       *regularFileFD
	off      uint64
	fileSize uint64
}

// ReadToBlocks implements safemem.Reader.ReadToBlocks.
func (r *fuseCacheReader) ReadToBlocks(dsts safemem.BlockSeq) (uint64, error) {
	if dsts.IsEmpty() {
		return 0, nil
	}
	inode := r.fd.inode()
	end := r.fileSize
	if r.off >= end {
		return 0, io.EOF
	}
	if requestedEnd := r.off + dsts.NumBytes(); requestedEnd > r.off && requestedEnd < end {
		end = requestedEnd
	}
	pageEnd, _ := hostarch.PageRoundUp(end)
	required := memmap.MappableRange{Start: hostarch.PageRoundDown(r.off), End: uint64(pageEnd)}
	cache := inode.cache
	cache.dataMu.Lock()
	defer cache.dataMu.Unlock()
	_, fillErr := cache.data.Fill(r.ctx, required, required, r.fileSize, inode.fs.mf, pgalloc.AllocOpts{
		Kind:                        usage.PageCache,
		MemCgID:                     pgalloc.MemoryCgroupIDFromContext(r.ctx),
		Mode:                        pgalloc.AllocateAndWritePopulate,
		ReaderMayWaitForApplication: true,
	}, r.fd.readToBlocksAt)

	var done uint64
	for seg := cache.data.FindSegment(r.off); seg.Ok() && seg.Start() < end; seg, _ = seg.NextNonEmpty() {
		mr := seg.Range().Intersect(memmap.MappableRange{Start: r.off, End: end})
		ims, mappingLease, err := memmap.MapInternalWithLease(inode.fs.mf, seg.FileRangeOf(mr), hostarch.Read)
		if err != nil {
			return done, err
		}
		n, err := safemem.CopySeq(dsts.DropFirst64(done), ims)
		if mappingLease != nil {
			mappingLease.Release()
		}
		done += n
		r.off += n
		if err != nil {
			return done, err
		}
	}
	if r.off < end && fillErr != nil {
		return done, fillErr
	}
	return done, nil
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

// Write implements vfs.FileDescriptionImpl.Write.
func (fd *regularFileFD) Write(ctx context.Context, src usermem.IOSequence, opts vfs.WriteOptions) (int64, error) {
	fd.offMu.Lock()
	n, off, err := fd.pwrite(ctx, src, fd.off, opts)
	fd.off = off
	fd.offMu.Unlock()
	return n, err
}

// pwrite returns the number of bytes written, final offset and error. The
// final offset should be ignored by PWrite.
func (fd *regularFileFD) pwrite(ctx context.Context, src usermem.IOSequence, offset int64, opts vfs.WriteOptions) (int64, int64, error) {
	if offset < 0 {
		return 0, offset, linuxerr.EINVAL
	}

	// Check that flags are supported.
	//
	// TODO(gvisor.dev/issue/2601): Support select preadv2 flags.
	if opts.Flags&^linux.RWF_HIPRI != 0 {
		return 0, offset, linuxerr.EOPNOTSUPP
	}

	inode := fd.inode()
	inode.attrMu.Lock()
	defer inode.attrMu.Unlock()

	// If the file is opened with O_APPEND, update offset to file size.
	// Note: since our Open() implements the interface of kernfs,
	// and kernfs currently does not support O_APPEND, this will never
	// be true before we switch out from kernfs.
	if fd.vfsfd.StatusFlags()&linux.O_APPEND != 0 {
		// Locking inode.metadataMu is sufficient for reading size
		offset = int64(inode.size.Load())
	}

	srclen := src.NumBytes()
	if srclen > math.MaxUint32 {
		// FUSE only supports uint32 for size.
		// Overflow.
		return 0, offset, linuxerr.EINVAL
	}
	if end := offset + srclen; end < offset {
		// Overflow.
		return 0, offset, linuxerr.EINVAL
	}

	limit, err := vfs.CheckLimit(ctx, offset, srclen)
	if err != nil {
		return 0, offset, err
	}
	if limit == 0 {
		// Return before causing any side effects.
		return 0, offset, nil
	}
	src = src.TakeFirst64(limit)

	// Write back dirty mmap pages that overlap this write, then remove the
	// affected cache pages and translations. This preserves ordering between
	// MAP_SHARED stores and write(2), while private mappings retain their CoW
	// pages as required by Linux.
	pgstart := hostarch.PageRoundDown(uint64(offset))
	pgend, ok := hostarch.PageRoundUp(uint64(offset) + uint64(src.NumBytes()))
	if !ok {
		return 0, offset, linuxerr.EINVAL
	}
	mr := memmap.MappableRange{Start: pgstart, End: uint64(pgend)}
	if err := fd.writebackRange(ctx, mr, inode.size.Load()); err != nil {
		return 0, offset, err
	}
	cache := inode.cache
	var freed []memmap.FileRange
	cache.dataMu.Lock()
	cache.dirty.KeepClean(mr)
	cache.cow.RemoveRange(mr)
	cache.data.RemoveRangeWith(mr, func(seg fsutil.FileRangeIterator) {
		freed = append(freed, seg.FileRange())
	})
	cache.dataMu.Unlock()
	cache.mapsMu.Lock()
	cache.mappings.Invalidate(mr, memmap.InvalidateOpts{})
	cache.mapsMu.Unlock()
	for _, fr := range freed {
		inode.fs.mf.DecRef(fr)
	}

	n, offset, err := inode.fs.Write(ctx, fd, offset, src)
	if n == 0 {
		// We have checked srclen != 0 previously.
		if err != nil {
			return 0, offset, err
		}
		// If err == nil, then it's a short write and we return EIO.
		return 0, offset, linuxerr.EIO
	}

	if offset > int64(inode.size.Load()) {
		inode.size.Store(uint64(offset))
		inode.fs.conn.attributeVersion.Add(1)
	}
	inode.touchCMtime()
	return n, offset, err
}

// ConfigureMMap implements vfs.FileDescriptionImpl.ConfigureMMap.
func (fd *regularFileFD) ConfigureMMap(ctx context.Context, opts *memmap.MMapOpts) error {
	opts.SentryOwnedContent = true
	return vfs.GenericConfigureMMap(&fd.vfsfd, fd, opts)
}

// ValidateLiveForkMapping implements memmap.LiveForkMappingValidator.
func (fd *regularFileFD) ValidateLiveForkMapping() error {
	if _, ok := fd.inode().fs.conn.fuseConn.(*deviceConn); !ok {
		return linuxerr.EOPNOTSUPP
	}
	return nil
}

// AddMapping implements memmap.Mappable.AddMapping.
func (fd *regularFileFD) AddMapping(ctx context.Context, ms memmap.MappingSpace, ar hostarch.AddrRange, offset uint64, writable bool) error {
	cache := fd.inode().cache
	cache.mapsMu.Lock()
	cache.mappings.AddMapping(ms, ar, offset, writable)
	cache.mapsMu.Unlock()
	return nil
}

// RemoveMapping implements memmap.Mappable.RemoveMapping.
func (fd *regularFileFD) RemoveMapping(ctx context.Context, ms memmap.MappingSpace, ar hostarch.AddrRange, offset uint64, writable bool) {
	cache := fd.inode().cache
	cache.mapsMu.Lock()
	unmapped := cache.mappings.RemoveMapping(ms, ar, offset, writable)
	cache.mapsMu.Unlock()
	if len(unmapped) == 0 {
		return
	}
	cache.dataMu.Lock()
	for _, mr := range unmapped {
		cache.dirty.AllowClean(mr)
	}
	cache.dataMu.Unlock()
}

// CopyMapping implements memmap.Mappable.CopyMapping.
func (fd *regularFileFD) CopyMapping(ctx context.Context, ms memmap.MappingSpace, srcAR, dstAR hostarch.AddrRange, offset uint64, writable bool) error {
	return fd.AddMapping(ctx, ms, dstAR, offset, writable)
}

// Translate implements memmap.Mappable.Translate.
func (fd *regularFileFD) Translate(ctx context.Context, required, optional memmap.MappableRange, at hostarch.AccessType) ([]memmap.Translation, error) {
	inode := fd.inode()
	cache := inode.cache
	inode.attrMu.Lock()
	defer inode.attrMu.Unlock()

	pgend, _ := hostarch.PageRoundUp(inode.size.Load())
	var beyondEOF bool
	if required.End > uint64(pgend) {
		if required.Start >= uint64(pgend) {
			return nil, &memmap.BusError{Err: io.EOF}
		}
		beyondEOF = true
		required.End = uint64(pgend)
	}
	if optional.End > uint64(pgend) {
		optional.End = uint64(pgend)
	}
	// Bound one fault's FUSE traffic. This is large enough for executable
	// readahead while avoiding an unbounded pause behind a userspace daemon.
	const maxReadahead = 64 << 10
	if optional.End-required.Start > maxReadahead {
		optional.End = required.Start + maxReadahead
	}

	cache.dataMu.Lock()
	defer cache.dataMu.Unlock()
	if err := cache.copyOnWriteLocked(ctx, inode.fs, required); err != nil {
		return nil, &memmap.BusError{Err: err}
	}
	_, fillErr := cache.data.Fill(ctx, required, optional, inode.size.Load(), inode.fs.mf, pgalloc.AllocOpts{
		Kind:                        usage.PageCache,
		MemCgID:                     pgalloc.MemoryCgroupIDFromContext(ctx),
		Mode:                        pgalloc.AllocateAndWritePopulate,
		ReaderMayWaitForApplication: true,
	}, fd.readToBlocksAt)

	var translations []memmap.Translation
	var translatedEnd uint64
	translateRange := optional
	if !cache.cow.IsEmpty() {
		// Do not return translations for adjacent pages that have not detached
		// from another branch yet.
		translateRange = required
	}
	for seg := cache.data.FindSegment(required.Start); seg.Ok() && seg.Start() < required.End; seg, _ = seg.NextNonEmpty() {
		mr := seg.Range().Intersect(translateRange)
		perms := hostarch.ReadExecute
		if at.Write {
			// We cannot observe individual host stores. Keep pages dirty while
			// they have writable translations, then write them through FUSE on
			// msync/fsync/unmap/save.
			cache.dirty.KeepDirty(mr)
			perms.Write = true
		}
		translations = append(translations, memmap.Translation{
			Source: mr,
			File:   inode.fs.mf,
			Offset: seg.FileRangeOf(mr).Start,
			Perms:  perms,
		})
		translatedEnd = mr.End
	}
	if translatedEnd < required.End && fillErr != nil {
		return translations, &memmap.BusError{Err: fillErr}
	}
	if beyondEOF {
		return translations, &memmap.BusError{Err: io.EOF}
	}
	return translations, nil
}

// readToBlocksAt fills MemoryFile pages from FUSE_READ responses.
// inode.attrMu is held by Translate.
func (fd *regularFileFD) readToBlocksAt(ctx context.Context, dsts safemem.BlockSeq, offset uint64) (uint64, error) {
	if dsts.NumBytes() > math.MaxUint32 {
		return 0, linuxerr.EINVAL
	}
	buffers, n, err := fd.inode().fs.ReadInPages(ctx, fd, offset, uint32(dsts.NumBytes()))
	if n == 0 {
		return 0, err
	}
	var copied uint64
	for _, buffer := range buffers {
		want := uint64(len(buffer))
		if want > dsts.NumBytes()-copied {
			want = dsts.NumBytes() - copied
		}
		n, copyErr := safemem.CopySeq(dsts.DropFirst64(copied).TakeFirst64(want), safemem.BlockSeqOf(safemem.BlockFromSafeSlice(buffer[:want])))
		copied += n
		if copyErr != nil || copied == dsts.NumBytes() {
			return copied, copyErr
		}
	}
	if copied < dsts.NumBytes() && err == nil {
		err = io.EOF
	}
	return copied, err
}

// InvalidateUnsavable implements memmap.Mappable.InvalidateUnsavable. Cached
// pages and dirty state are serializable Sentry memory. In particular, this
// method must not issue FUSE requests: save runs after guest tasks, including
// the FUSE daemon, have been paused.
func (fd *regularFileFD) InvalidateUnsavable(ctx context.Context) error {
	cache := fd.inode().cache
	cache.mapsMu.Lock()
	cache.mappings.InvalidateAll(memmap.InvalidateOpts{})
	cache.mapsMu.Unlock()
	return nil
}
