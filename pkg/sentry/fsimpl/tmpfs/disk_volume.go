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

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tmpfs

import (
	"errors"
	"fmt"
	stdfs "io/fs"
	"math"
	"os"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	gerrors "gvisor.dev/gvisor/pkg/errors"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/ttcvolume"
)

// diskVolumeError translates repository implementation failures at the guest
// syscall boundary. Linux errors retain their exact errno; graph/WAL
// invariants and other implementation errors are logged and become EIO. An
// arbitrary Go error must never escape into kernel.ExtractErrno, which treats
// unknown syscall errors as a Sentry invariant violation and panics.
func diskVolumeError(op string, err error) error {
	if err == nil {
		return nil
	}
	var linuxError *gerrors.Error
	if errors.As(err, &linuxError) {
		if linuxError == linuxerr.EIO {
			reportDiskVolumeFailure(op, err)
		}
		return linuxError
	}
	var syscallError syscall.Errno
	if errors.As(err, &syscallError) {
		if syscallError == syscall.EIO {
			reportDiskVolumeFailure(op, err)
		}
		return syscallError
	}
	var unixError unix.Errno
	if errors.As(err, &unixError) {
		if unixError == unix.EIO {
			reportDiskVolumeFailure(op, err)
		}
		return unixError
	}
	switch {
	case errors.Is(err, stdfs.ErrNotExist):
		return linuxerr.ENOENT
	case errors.Is(err, stdfs.ErrExist):
		return linuxerr.EEXIST
	case errors.Is(err, stdfs.ErrPermission):
		return linuxerr.EACCES
	case errors.Is(err, stdfs.ErrInvalid):
		return linuxerr.EINVAL
	}
	reportDiskVolumeFailure(op, err)
	return linuxerr.EIO
}

// reportDiskVolumeFailure preserves the full failing operation even when the
// underlying implementation already returned EIO. Sentries normally discard
// informational logs, but preserve fd 2 in the daemon-local panic log. An EIO
// without its repository cause is otherwise indistinguishable from a host
// storage failure at the guest syscall boundary.
func reportDiskVolumeFailure(op string, err error) {
	log.Warningf("TTC persistent filesystem %s failed: %v", op, err)
	fmt.Fprintf(os.Stderr, "TTC persistent filesystem %s failed: %v\n", op, err)
}

// registerDiskMaterializationHook keeps the guest's cached host descriptors
// and mappings coherent when a remotely inherited placeholder is atomically
// replaced by its authoritative whole-file contents.
func (fs *filesystem) registerDiskMaterializationHook() {
	if fs.diskVolume == nil || fs.diskVolume.Repository == nil {
		return
	}
	if fs.unregisterMaterializationHook != nil {
		fs.unregisterMaterializationHook()
	}
	fs.unregisterMaterializationHook = fs.diskVolume.Repository.RegisterMaterializationHook(fs.invalidateMaterializedObject)
}

// markDiskForkFile records that file owns runtime-local cache state which must
// be invalidated before the next repository seal. Entries are cleared by
// PrepareSave, so repeated forks cost only the files touched since the prior
// fork rather than every instantiated file in the namespace.
func (fs *filesystem) markDiskForkFile(file *regularFile) {
	if fs.diskVolume == nil {
		return
	}
	fs.liveForkMu.Lock()
	if fs.diskForkFiles == nil {
		fs.diskForkFiles = make(map[*regularFile]struct{})
	}
	fs.diskForkFiles[file] = struct{}{}
	fs.liveForkMu.Unlock()
}

func updateDiskCacheIndexLocked(index *map[string]map[*regularFile]struct{}, file *regularFile, oldObject, newObject string) {
	if oldObject == newObject {
		return
	}
	if oldObject != "" {
		files := (*index)[oldObject]
		delete(files, file)
		if len(files) == 0 {
			delete(*index, oldObject)
		}
	}
	if newObject != "" {
		if *index == nil {
			*index = make(map[string]map[*regularFile]struct{})
		}
		files := (*index)[newObject]
		if files == nil {
			files = make(map[*regularFile]struct{})
			(*index)[newObject] = files
		}
		files[file] = struct{}{}
	}
}

// setDiskMmapObjectLocked updates the mmap cache identity and its exact
// materialization index. rf.diskMapMu must be held.
func (rf *regularFile) setDiskMmapObjectLocked(object string) {
	fs := rf.inode.fs
	if fs != nil && fs.diskVolume != nil {
		fs.liveForkMu.Lock()
		updateDiskCacheIndexLocked(&fs.diskMmapObjectFiles, rf, rf.diskObject, object)
		fs.liveForkMu.Unlock()
	}
	rf.diskObject = object
}

// setDiskIOObjectLocked updates the read/write cache identity and its exact
// materialization index. rf.diskIOMu must be held.
func (rf *regularFile) setDiskIOObjectLocked(object string) {
	fs := rf.inode.fs
	if fs != nil && fs.diskVolume != nil {
		fs.liveForkMu.Lock()
		updateDiskCacheIndexLocked(&fs.diskIOObjectFiles, rf, rf.diskIOObject, object)
		fs.liveForkMu.Unlock()
	}
	rf.diskIOObject = object
}

func (fs *filesystem) markDiskInodeDirty(node *inode) {
	fs.diskDirtyMu.Lock()
	node.diskMetadataGeneration.Add(1)
	if fs.diskDirtyInodes == nil {
		fs.diskDirtyInodes = make(map[*inode]struct{})
	}
	fs.diskDirtyInodes[node] = struct{}{}
	fs.diskDirtyMu.Unlock()
}

func (fs *filesystem) finishDiskInodeMetadata(node *inode, generation uint64) {
	fs.diskDirtyMu.Lock()
	if node.diskMetadataGeneration.CompareAndSwap(generation, 0) {
		delete(fs.diskDirtyInodes, node)
	}
	fs.diskDirtyMu.Unlock()
}

func (fs *filesystem) markDiskFileDirty(file *regularFile) {
	fs.diskDirtyMu.Lock()
	file.diskMetadataDirty.Add(1)
	if fs.diskDirtyFiles == nil {
		fs.diskDirtyFiles = make(map[*regularFile]struct{})
	}
	fs.diskDirtyFiles[file] = struct{}{}
	fs.diskDirtyMu.Unlock()
}

func (fs *filesystem) finishDiskFileMetadata(file *regularFile, generation uint64) {
	fs.diskDirtyMu.Lock()
	if file.diskMetadataDirty.CompareAndSwap(generation, 0) {
		delete(fs.diskDirtyFiles, file)
	}
	fs.diskDirtyMu.Unlock()
}

func (fs *filesystem) invalidateMaterializedObject(object string) {
	if fs.diskVolume != nil && fs.diskVolume.Repository != nil {
		forgetSharedDiskMapping(fs.diskVolume.Repository, object)
	}
	fs.liveForkMu.Lock()
	selected := make(map[*regularFile]struct{})
	for file := range fs.diskMmapObjectFiles[object] {
		selected[file] = struct{}{}
	}
	for file := range fs.diskIOObjectFiles[object] {
		selected[file] = struct{}{}
	}
	files := make([]*regularFile, 0, len(selected))
	for file := range selected {
		files = append(files, file)
	}
	fs.liveForkMu.Unlock()
	for _, file := range files {
		file.mapsMu.Lock()
		file.diskIOMu.Lock()
		if file.diskIOObject == object {
			file.invalidateDiskIOLocked()
		}
		file.diskIOMu.Unlock()
		file.diskMapMu.Lock()
		mapped := file.diskObject == object
		if mapped {
			file.releaseDiskMapLocked()
		}
		file.diskMapMu.Unlock()
		if mapped {
			// Preserve already-private application pages while forcing clean
			// translations to fault against the newly installed backing inode.
			file.mappings.InvalidateAll(memmap.InvalidateOpts{})
		}
		file.mapsMu.Unlock()
	}
}

func (fs *filesystem) loadDiskVolume(ctx context.Context) (*dentry, error) {
	if fs.diskVolume == nil || fs.diskVolume.Repository == nil {
		return nil, fmt.Errorf("missing TTC volume repository")
	}
	if _, err := fs.diskVolume.Repository.Branch(fs.diskVolume.Branch); err != nil {
		return nil, err
	}
	fs.diskInodesMu.Lock()
	fs.diskInodes = make(map[uint64]*inode)
	fs.diskInodesMu.Unlock()
	rootInode, err := fs.loadDiskInode(ttcvolume.RootInode, nil)
	if err != nil {
		return nil, err
	}
	if rootDir, ok := rootInode.impl.(*directory); ok {
		inodes, bytes, err := fs.diskVolume.Repository.BranchUsage(fs.diskVolume.Branch)
		if err != nil {
			return nil, err
		}
		fs.inodesUsed.Store(inodes)
		fs.diskBytes.Store(bytes)
		fs.diskBytesSaturated.Store(bytes == math.MaxUint64)
		return &rootDir.dentry, nil
	}
	return fs.newDentry(rootInode), nil
}

func (fs *filesystem) loadDiskInode(ino uint64, parent *directory) (*inode, error) {
	fs.diskInodesMu.Lock()
	if inode, ok := fs.diskInodes[ino]; ok {
		fs.diskInodesMu.Unlock()
		if inode.isDir() {
			return nil, fmt.Errorf("volume directory inode %d has multiple parents", ino)
		}
		return inode, nil
	}
	fs.diskInodesMu.Unlock()
	meta, err := fs.diskVolume.Repository.Inode(fs.diskVolume.Branch, ino)
	if err != nil {
		return nil, err
	}
	var inode *inode
	switch meta.Type {
	case "regular":
		inode, err = fs.newRegularFile(auth.KUID(meta.UID), auth.KGID(meta.GID), linux.FileMode(meta.Mode), parent)
		if err == nil {
			inode.impl.(*regularFile).size.Store(uint64(meta.Size))
		}
	case "directory":
		var dir *directory
		dir, err = fs.newDirectory(auth.KUID(meta.UID), auth.KGID(meta.GID), linux.FileMode(meta.Mode), parent)
		if err == nil {
			inode = &dir.inode
		}
	case "symlink":
		inode, err = fs.newSymlink(auth.KUID(meta.UID), auth.KGID(meta.GID), linux.FileMode(meta.Mode), meta.Target, parent)
	case "fifo":
		inode, err = fs.newNamedPipe(auth.KUID(meta.UID), auth.KGID(meta.GID), linux.FileMode(meta.Mode), parent)
	case "char":
		inode, err = fs.newDeviceFileLocked(auth.KUID(meta.UID), auth.KGID(meta.GID), linux.S_IFCHR|linux.FileMode(meta.Mode), meta.DevMajor, meta.DevMinor, parent)
	case "block":
		inode, err = fs.newDeviceFileLocked(auth.KUID(meta.UID), auth.KGID(meta.GID), linux.S_IFBLK|linux.FileMode(meta.Mode), meta.DevMajor, meta.DevMinor, parent)
	case "socket":
		inode, err = fs.newSocketFile(auth.KUID(meta.UID), auth.KGID(meta.GID), linux.S_IFSOCK|linux.FileMode(meta.Mode), nil, parent)
	default:
		return nil, fmt.Errorf("unsupported persistent inode %d type %q", ino, meta.Type)
	}
	if err != nil {
		return nil, err
	}
	fs.applyDiskMetadata(inode, meta)
	if meta.Type == "regular" {
		fs.adjustDiskBytes(0, uint64(meta.Size))
	}
	fs.diskInodesMu.Lock()
	fs.diskInodes[ino] = inode
	fs.diskInodesMu.Unlock()
	if dir, ok := inode.impl.(*directory); ok {
		entries, err := fs.diskVolume.Repository.ListDirectory(fs.diskVolume.Branch, ino)
		if err != nil {
			return nil, err
		}
		dir.numChildren.Store(int64(len(entries)))
	}
	return inode, nil
}

// diskChildLocked returns name from dir, instantiating its inode and dentry
// from the persistent repository on the first lookup. filesystem.mu must be
// held for writing.
func (fs *filesystem) diskChildLocked(dir *directory, name string) (*dentry, error) {
	if child, ok := dir.childMap[name]; ok {
		return child, nil
	}
	if fs.diskVolume == nil || dir.diskComplete {
		return nil, linuxerr.ENOENT
	}
	ino, err := fs.diskVolume.Repository.Lookup(fs.diskVolume.Branch, dir.inode.ino, name)
	if err != nil {
		if errors.Is(err, stdfs.ErrNotExist) {
			return nil, linuxerr.ENOENT
		}
		return nil, diskVolumeError("lookup", err)
	}
	childInode, err := fs.loadDiskInode(ino, dir)
	if err != nil {
		return nil, diskVolumeError("load lookup inode", fmt.Errorf("loading %q: %w", name, err))
	}
	var child *dentry
	if childDir, ok := childInode.impl.(*directory); ok {
		child = &childDir.dentry
		dir.inode.incRef() // child directory's ".." lifetime reference.
	} else {
		child = fs.newDentry(childInode)
	}
	// insertChildLocked updates the logical child count; the count already
	// came from repository metadata, so insert only into the runtime cache,
	// under a name of its own: see doCreateAt.
	name = strings.Clone(name)
	genericSetParentAndName(fs, child, &dir.dentry, name)
	if dir.childMap == nil {
		dir.childMap = make(map[string]*dentry)
	}
	dir.childMap[name] = child
	dir.iterMu.Lock()
	dir.childList.PushBack(child)
	dir.iterMu.Unlock()
	return child, nil
}

func (fs *filesystem) diskDirectoryEmptyLocked(dir *directory) (bool, error) {
	if fs.diskVolume == nil {
		return len(dir.childMap) == 0, nil
	}
	entries, err := fs.diskVolume.Repository.ListDirectory(fs.diskVolume.Branch, dir.inode.ino)
	return len(entries) == 0, diskVolumeError("test directory emptiness", err)
}

func pageRoundedDiskBytes(size uint64) uint64 {
	if size == 0 {
		return 0
	}
	return (size + hostarch.PageSize - 1) &^ (hostarch.PageSize - 1)
}

// waitOutHandoff runs barrier, a repository durability barrier for a guest's
// fsync or sync, waiting out an authority handoff, which refuses to publish
// metadata for its moment (see ttcvolume.WaitOutAuthorityHandoff): on a
// cluster, a guest's fsync failed with EIO while a checkpoint of its branch
// was handed to another node. A stop interrupts the wait, as for a fork, and
// the syscall restarts: the barrier is idempotent.
func waitOutHandoff(ctx context.Context, barrier func() error) error {
	return ttcvolume.WaitOutAuthorityHandoff(barrier, func(interval time.Duration) error {
		if _, err := ctx.BlockWithTimeout(nil, true, interval); err != nil && !linuxerr.Equals(linuxerr.ETIMEDOUT, err) {
			return linuxerr.ERESTARTNOINTR
		}
		return nil
	})
}

// waitOutHandoffUninterruptibly is waitOutHandoff for a barrier that cannot
// restart, such as an O_SYNC write's, which has written.
func waitOutHandoffUninterruptibly(barrier func() error) error {
	return ttcvolume.WaitOutAuthorityHandoff(barrier, func(interval time.Duration) error {
		time.Sleep(interval)
		return nil
	})
}

// diskBytesLog logs disk byte accounting that went wrong.
var diskBytesLog = log.BasicRateLimitedLogger(time.Minute)

// adjustDiskBytes accounts a regular file changing size from oldSize to
// newSize. The sum saturates rather than wrapping, as five sparse files of 4
// EiB, which a guest truncates files to in a moment, wrapped it. A file then
// shrinking by more than the sum panicked, which took down every branch of
// the family: the sum is only reported (AllocatedBytes), so it stops at zero.
func (fs *filesystem) adjustDiskBytes(oldSize, newSize uint64) {
	oldBytes := pageRoundedDiskBytes(oldSize)
	newBytes := pageRoundedDiskBytes(newSize)
	if oldBytes == newBytes {
		return
	}
	for {
		current := fs.diskBytes.Load()
		var next uint64
		if newBytes > oldBytes {
			next = ttcvolume.AddBytes(current, newBytes-oldBytes)
			if next == math.MaxUint64 {
				fs.diskBytesSaturated.Store(true)
			}
		} else {
			delta := oldBytes - newBytes
			if delta > current && !fs.diskBytesSaturated.Load() {
				// Short of a sum that saturated, files account more
				// than they added.
				diskBytesLog.Warningf("ttcvol disk byte accounting: a file shrank by %d bytes, with %d bytes accounted", delta, current)
			}
			next = current - min(delta, current)
		}
		if fs.diskBytes.CompareAndSwap(current, next) {
			return
		}
	}
}

func (fs *filesystem) applyDiskMetadata(node *inode, meta ttcvolume.Inode) {
	fileType := node.mode.Load() & linux.S_IFMT
	node.mode.Store(fileType | meta.Mode&07777)
	node.uid.Store(meta.UID)
	node.gid.Store(meta.GID)
	node.nlink.Store(meta.Nlink)
	node.ino = meta.Number
	node.atime.Store(meta.AtimeNsec)
	node.btime.Store(meta.BtimeNsec)
	node.ctime.Store(meta.CtimeNsec)
	node.mtime.Store(meta.MtimeNsec)
	node.xattrs.SetRawXattrs(meta.Xattrs)
	fs.diskInodesMu.Lock()
	if fs.diskInodes == nil {
		fs.diskInodes = make(map[uint64]*inode)
	}
	fs.diskInodes[meta.Number] = node
	fs.diskInodesMu.Unlock()
}

func (fs *filesystem) rebuildDiskInodeCache() {
	inodes := make(map[uint64]*inode)
	saveDentries := make(map[*dentry]struct{})
	var visit func(*dentry)
	visit = func(d *dentry) {
		inodes[d.inode.ino] = d.inode
		if d != fs.root && (d.diskSaveRefs.Load() != 0 || d.diskSavePersistent.Load() != 0) {
			saveDentries[d] = struct{}{}
		}
		if dir, ok := d.inode.impl.(*directory); ok {
			for _, child := range dir.childMap {
				visit(child)
			}
		}
	}
	visit(fs.root)
	fs.liveForkMu.Lock()
	for file := range fs.liveForkFiles {
		inodes[file.inode.ino] = &file.inode
		// A restored regular file is present in the state graph only because it
		// carries runtime state. Conservatively invalidate it at the next seal;
		// subsequent seals use the touched-since-last-seal registry normally.
		if fs.diskForkFiles == nil {
			fs.diskForkFiles = make(map[*regularFile]struct{})
		}
		fs.diskForkFiles[file] = struct{}{}
	}
	fs.liveForkMu.Unlock()
	fs.diskInodesMu.Lock()
	fs.diskInodes = inodes
	fs.diskInodesMu.Unlock()
	fs.diskSaveMu.Lock()
	fs.diskSaveDentries = saveDentries
	fs.diskSaveMu.Unlock()
}

func (fs *filesystem) unregisterDiskInode(node *inode) {
	if fs.diskVolume == nil {
		return
	}
	fs.diskInodesMu.Lock()
	if fs.diskInodes[node.ino] == node {
		delete(fs.diskInodes, node.ino)
	}
	fs.diskInodesMu.Unlock()
	fs.diskDirtyMu.Lock()
	delete(fs.diskDirtyInodes, node)
	if file, ok := node.impl.(*regularFile); ok {
		delete(fs.diskDirtyFiles, file)
	}
	fs.diskDirtyMu.Unlock()
}

// pruneDiskNamespaceForSave makes state serialization proportional to live
// runtime references instead of to the number of closed paths cached by this
// process. The complete source cache is restored by
// restoreDiskNamespaceAfterSave; the clone resolves omitted entries from its
// already-forked repository branch.
func (fs *filesystem) pruneDiskNamespaceForSave() int {
	if fs.diskVolume == nil || fs.diskSaveChildren != nil {
		return 0
	}
	root, ok := fs.root.inode.impl.(*directory)
	if !ok {
		// A ttcvol may expose one regular file as its mount root. Such a
		// filesystem has no cached directory namespace to prune; the root is
		// already retained by the mount itself.
		return 0
	}
	fs.diskSaveMu.Lock()
	candidates := make([]*dentry, 0, len(fs.diskSaveDentries))
	for d := range fs.diskSaveDentries {
		candidates = append(candidates, d)
	}
	fs.diskSaveMu.Unlock()
	fs.mu.Lock()
	defer fs.mu.Unlock()
	filtered := map[*directory]map[string]*dentry{root: nil}
	for _, candidate := range candidates {
		child := candidate
		for child != fs.root {
			parent := child.parent.Load()
			if parent == nil {
				break
			}
			parentDir, ok := parent.inode.impl.(*directory)
			if !ok || parentDir.childMap[child.name] != child {
				break
			}
			children := filtered[parentDir]
			if children == nil {
				children = make(map[string]*dentry)
				filtered[parentDir] = children
			}
			children[child.name] = child
			child = parent
		}
	}
	fs.diskSaveChildren = make(map[*directory]map[string]*dentry, len(filtered))
	for dir, kept := range filtered {
		fs.diskSaveChildren[dir] = dir.childMap
		dir.childMap = kept
		dir.childList.Reset()
		for _, child := range kept {
			child.SetPrev(nil)
			child.SetNext(nil)
			dir.childList.PushBack(child)
		}
	}
	return len(candidates)
}

func (fs *filesystem) restoreDiskNamespaceAfterSave() {
	if fs.diskSaveChildren == nil {
		return
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for dir, original := range fs.diskSaveChildren {
		dir.childMap = original
		dir.childList.Reset()
		for _, child := range original {
			child.SetPrev(nil)
			child.SetNext(nil)
			dir.childList.PushBack(child)
		}
	}
	fs.diskSaveChildren = nil
}

func (fs *filesystem) diskMetadata(inode *inode) ttcvolume.Inode {
	meta := ttcvolume.Inode{
		Number:    inode.ino,
		Mode:      inode.mode.Load() & 07777,
		UID:       inode.uid.Load(),
		GID:       inode.gid.Load(),
		Nlink:     inode.nlink.Load(),
		AtimeNsec: inode.atime.Load(),
		BtimeNsec: inode.btime.Load(),
		CtimeNsec: inode.ctime.Load(),
		MtimeNsec: inode.mtime.Load(),
		Xattrs:    inode.xattrs.RawXattrs(),
	}
	switch impl := inode.impl.(type) {
	case *regularFile:
		meta.Type = "regular"
		meta.Size = int64(impl.size.Load())
	case *directory:
		meta.Type = "directory"
	case *symlink:
		meta.Type = "symlink"
		meta.Target = impl.target
	case *namedPipe:
		meta.Type = "fifo"
	case *deviceFile:
		if impl.kind == vfs.BlockDevice {
			meta.Type = "block"
		} else {
			meta.Type = "char"
		}
		meta.DevMajor = impl.major
		meta.DevMinor = impl.minor
	case *socketFile:
		meta.Type = "socket"
	}
	return meta
}

func (fs *filesystem) persistDiskMetadata(inode *inode) error {
	if fs.diskVolume == nil {
		return nil
	}
	generation := inode.diskMetadataGeneration.Load()
	if err := fs.diskVolume.Repository.UpdateMetadata(fs.diskVolume.Branch, fs.diskMetadata(inode)); err != nil {
		return diskVolumeError("update inode metadata", err)
	}
	fs.finishDiskInodeMetadata(inode, generation)
	return nil
}

func (fs *filesystem) persistDiskWriteMetadata(inode *inode) error {
	return diskVolumeError("publish written inode metadata", fs.persistDiskWriteMetadataRaw(inode))
}

// persistDiskWriteMetadataRaw is the control-plane form of
// persistDiskWriteMetadata. Internal lifecycle operations must preserve the
// repository's diagnostic instead of translating it to a guest errno; syscall
// callers use persistDiskWriteMetadata above.
func (fs *filesystem) persistDiskWriteMetadataRaw(inode *inode) error {
	if fs.diskVolume == nil {
		return nil
	}
	generation := inode.diskMetadataGeneration.Load()
	if err := fs.diskVolume.Repository.UpdateAfterWrite(fs.diskVolume.Branch, fs.diskMetadata(inode)); err != nil {
		return err
	}
	fs.finishDiskInodeMetadata(inode, generation)
	return nil
}

// diskInodeReaped reports whether err, from the repository about node, says
// that node is gone because it was destroyed meanwhile. A flush takes the dirty
// inodes before it updates them, and the last reference to an unlinked inode
// may be dropped in between, which reaps the inode from the repository: its
// metadata and data no longer matter.
func diskInodeReaped(node *inode, err error) bool {
	// The low half of the count is the real references; a concurrent
	// TryIncRef adds a speculative one above it, even to a destroyed inode.
	return errors.Is(err, stdfs.ErrNotExist) && int32(node.refs.ReadRefs()) == 0
}

// flushDiskWriteMetadata publishes metadata deferred by ordinary regular-file
// writes. liveForkFiles contains every live regular file, including unlinked
// files that remain reachable through an open description.
func (fs *filesystem) flushDiskWriteMetadata() error {
	return diskVolumeError("flush write metadata", fs.flushDiskWriteMetadataRaw())
}

// flushDiskWriteMetadataRaw is used by lifecycle operations, where preserving
// the complete repository diagnostic is more useful than a guest errno.
func (fs *filesystem) flushDiskWriteMetadataRaw() error {
	if fs.diskVolume == nil {
		return nil
	}
	fs.diskDirtyMu.Lock()
	files := make([]*regularFile, 0, len(fs.diskDirtyFiles))
	for file := range fs.diskDirtyFiles {
		files = append(files, file)
	}
	inodes := make([]*inode, 0, len(fs.diskDirtyInodes))
	for node := range fs.diskDirtyInodes {
		inodes = append(inodes, node)
	}
	fs.diskDirtyMu.Unlock()
	for _, file := range files {
		if err := file.flushDiskMetadataRaw(); err != nil {
			return err
		}
	}
	type pendingMetadata struct {
		inode      *inode
		generation uint64
		metadata   ttcvolume.Inode
	}
	pending := make([]pendingMetadata, 0)
	for _, node := range inodes {
		generation := node.diskMetadataGeneration.Load()
		if generation == 0 {
			continue
		}
		node.mu.Lock()
		pending = append(pending, pendingMetadata{
			inode: node, generation: generation, metadata: fs.diskMetadata(node),
		})
		node.mu.Unlock()
	}
	for len(pending) != 0 {
		updates := make([]ttcvolume.Inode, len(pending))
		for idx := range pending {
			updates[idx] = pending[idx].metadata
		}
		err := fs.diskVolume.Repository.UpdateMetadataBatch(fs.diskVolume.Branch, updates)
		if err == nil {
			break
		}
		// Update the others without the inodes reaped since they were taken.
		live := pending[:0]
		for _, update := range pending {
			if !diskInodeReaped(update.inode, err) {
				live = append(live, update)
			}
		}
		if len(live) == len(pending) {
			return fmt.Errorf("updating inode metadata batch: %w", err)
		}
		pending = live
	}
	for _, update := range pending {
		fs.finishDiskInodeMetadata(update.inode, update.generation)
	}
	return nil
}

// syncDiskData synchronizes the data objects of live regular files written
// through MAP_SHARED mappings, which modify the donated host mapping without
// passing through regularFileFD.PWrite. Writes, truncations and allocations
// mark their objects dirty, and the repository's dirty-object set covers
// those. It synchronized every live file, opening and fsyncing each on the
// host: a sync(2) cost as much again for every file a process held open.
func (fs *filesystem) syncDiskData() error {
	if fs.diskVolume == nil {
		return nil
	}
	fs.liveForkMu.Lock()
	files := make([]*regularFile, 0, len(fs.liveForkFiles))
	for file := range fs.liveForkFiles {
		files = append(files, file)
	}
	fs.liveForkMu.Unlock()
	for _, file := range files {
		if !file.takeDiskMappedWrites() {
			continue
		}
		if err := fs.diskVolume.Repository.SyncData(fs.diskVolume.Branch, file.inode.ino, false); err != nil && !diskInodeReaped(&file.inode, err) {
			// A later sync must take the file again.
			file.diskMappedWrites.Store(true)
			return diskVolumeError("sync regular-file data", err)
		}
	}
	return nil
}

// takeDiskMappedWrites returns true if rf may hold bytes written through a
// writable mapping that no sync has taken: it has one, or had one since a
// sync last took it.
func (rf *regularFile) takeDiskMappedWrites() bool {
	if rf.diskMappedWrites.Swap(false) {
		return true
	}
	rf.diskMapMu.Lock()
	defer rf.diskMapMu.Unlock()
	return rf.writableMappingPages != 0
}
