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

package fuse

import (
	goContext "context"
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

// +stateify savable
type regularFileFD struct {
	fileDescription

	// offMu protects off.
	offMu sync.Mutex `state:"nosave"`

	// off is the file offset.
	// +checklocks:offMu
	off int64

	// mapsMu protects mappings.
	mapsMu sync.Mutex `state:"nosave"`

	// mappings tracks mappings of the file into memmap.MappingSpaces.
	//
	// Protected by mapsMu.
	mappings memmap.MappingSet

	// dataMu protects the fields below.
	dataMu sync.RWMutex `state:"nosave"`

	// data maps offsets into the file to offsets into memFile that store
	// the file's data.
	//
	// Protected by dataMu.
	data fsutil.FileRangeSet
}

// afterLoad is invoked by stateify.
func (fd *regularFileFD) afterLoad(ctx goContext.Context) {
	if err := fsutil.RelocateFileRangeSet(ctx, fd.inode().fs.mf, &fd.data); err != nil {
		panic("relocating FUSE mmap cache: " + err.Error())
	}
	if !pgalloc.LiveForkCloneNeedsPageRefs(ctx) {
		return
	}
	memCgID := pgalloc.MemoryCgroupIDFromContext(ctx)
	for seg := fd.data.FirstSegment(); seg.Ok(); seg = seg.NextSegment() {
		pgalloc.AcquireRestoreRef(ctx, fd.inode().fs.mf, seg.FileRange(), memCgID)
	}
}

// Release implements vfs.FileDescriptionImpl.Release.
func (fd *regularFileFD) Release(ctx context.Context) {
	fd.dataMu.Lock()
	fd.data.DropAll(fd.inode().fs.mf)
	fd.dataMu.Unlock()
	fd.fileDescription.Release(ctx)
}

// Seek implements vfs.FileDescriptionImpl.Allocate.
func (fd *regularFileFD) Allocate(ctx context.Context, mode, offset, length uint64) error {
	if mode & ^uint64(linux.FALLOC_FL_KEEP_SIZE|linux.FALLOC_FL_PUNCH_HOLE|linux.FALLOC_FL_ZERO_RANGE) != 0 {
		return linuxerr.EOPNOTSUPP
	}
	in := linux.FUSEFallocateIn{
		Fh:     fd.Fh,
		Offset: uint64(offset),
		Length: uint64(length),
		Mode:   uint32(mode),
	}
	i := fd.inode()
	if err := i.call(ctx, linux.FUSE_FALLOCATE, &in, nil); err != nil {
		return err
	}
	i.attrMu.Lock()
	defer i.attrMu.Unlock()
	if uint64(offset+length) > i.size.Load() {
		if err := i.reviseAttr(ctx, linux.FUSE_GETATTR_FH, fd.Fh); err != nil {
			return err
		}
		// If the offset after update is still too large, return error.
		if uint64(offset) >= i.size.Load() {
			return io.EOF
		}
	}
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
	defer inode.attrMu.Unlock()

	// Reading beyond EOF, update file size if outdated.
	if uint64(offset+size) > inode.size.Load() {
		if err := inode.reviseAttr(ctx, linux.FUSE_GETATTR_FH, fd.Fh); err != nil {
			return 0, err
		}
		// If the offset after update is still too large, return error.
		if uint64(offset) >= inode.size.Load() {
			return 0, io.EOF
		}
	}

	// Truncate the read with updated file size.
	fileSize := inode.size.Load()
	if uint64(offset+size) > fileSize {
		size = int64(fileSize) - offset
	}

	buffers, n, err := inode.fs.ReadInPages(ctx, fd, uint64(offset), uint32(size))
	if err != nil {
		return 0, err
	}

	// TODO(gvisor.dev/issue/3237): support indirect IO (e.g. caching),
	// store the bytes that were read ahead.

	// Update the number of bytes to copy for short read.
	if n < uint32(size) {
		size = int64(n)
	}

	// Copy the bytes read to the dst.
	// This loop is intended for fragmented reads.
	// For the majority of reads, this loop only execute once.
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

	// FUSE does not currently support server-driven page-cache invalidation.
	// Keep mmap coherent with writes performed through this Sentry by removing
	// affected translations and cached pages before issuing FUSE_WRITE.
	pgstart := hostarch.PageRoundDown(uint64(offset))
	pgend, ok := hostarch.PageRoundUp(uint64(offset) + uint64(src.NumBytes()))
	if !ok {
		return 0, offset, linuxerr.EINVAL
	}
	fd.mapsMu.Lock()
	fd.mappings.Invalidate(memmap.MappableRange{Start: pgstart, End: uint64(pgend)}, memmap.InvalidateOpts{})
	fd.mapsMu.Unlock()
	fd.dataMu.Lock()
	fd.data.Drop(memmap.MappableRange{Start: pgstart, End: uint64(pgend)}, inode.fs.mf)
	fd.dataMu.Unlock()

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
	// Writable shared mappings need FUSE writeback-cache and invalidation
	// support. Private executable/writable mappings (the AppImage use case) and
	// read-only shared mappings are backed by Sentry-owned cached pages.
	if !opts.Private && opts.MaxPerms.Write {
		return linuxerr.EOPNOTSUPP
	}
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
	fd.mapsMu.Lock()
	fd.mappings.AddMapping(ms, ar, offset, writable)
	fd.mapsMu.Unlock()
	return nil
}

// RemoveMapping implements memmap.Mappable.RemoveMapping.
func (fd *regularFileFD) RemoveMapping(ctx context.Context, ms memmap.MappingSpace, ar hostarch.AddrRange, offset uint64, writable bool) {
	fd.mapsMu.Lock()
	fd.mappings.RemoveMapping(ms, ar, offset, writable)
	fd.mapsMu.Unlock()
}

// CopyMapping implements memmap.Mappable.CopyMapping.
func (fd *regularFileFD) CopyMapping(ctx context.Context, ms memmap.MappingSpace, srcAR, dstAR hostarch.AddrRange, offset uint64, writable bool) error {
	return fd.AddMapping(ctx, ms, dstAR, offset, writable)
}

// Translate implements memmap.Mappable.Translate.
func (fd *regularFileFD) Translate(ctx context.Context, required, optional memmap.MappableRange, at hostarch.AccessType) ([]memmap.Translation, error) {
	inode := fd.inode()
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

	fd.dataMu.Lock()
	defer fd.dataMu.Unlock()
	_, fillErr := fd.data.Fill(ctx, required, optional, inode.size.Load(), inode.fs.mf, pgalloc.AllocOpts{
		Kind:    usage.PageCache,
		MemCgID: pgalloc.MemoryCgroupIDFromContext(ctx),
		Mode:    pgalloc.AllocateAndWritePopulate,
	}, fd.readToBlocksAt)

	var translations []memmap.Translation
	var translatedEnd uint64
	for seg := fd.data.FindSegment(required.Start); seg.Ok() && seg.Start() < required.End; seg, _ = seg.NextNonEmpty() {
		mr := seg.Range().Intersect(optional)
		translations = append(translations, memmap.Translation{
			Source: mr,
			File:   inode.fs.mf,
			Offset: seg.FileRangeOf(mr).Start,
			Perms:  hostarch.AnyAccess,
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

// InvalidateUnsavable implements memmap.Mappable.InvalidateUnsavable. FUSE
// cached pages can always be reconstructed by the cloned in-sandbox daemon;
// private PMAs remain intact because InvalidatePrivate is false.
func (fd *regularFileFD) InvalidateUnsavable(ctx context.Context) error {
	fd.mapsMu.Lock()
	fd.mappings.InvalidateAll(memmap.InvalidateOpts{})
	fd.mapsMu.Unlock()
	fd.dataMu.Lock()
	fd.data.DropAll(fd.inode().fs.mf)
	fd.dataMu.Unlock()
	return nil
}
