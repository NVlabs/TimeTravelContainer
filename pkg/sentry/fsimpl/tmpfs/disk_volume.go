// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tmpfs

import (
	"errors"
	"fmt"
	stdfs "io/fs"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/ttcvolume"
)

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
	if fs.diskVolume == nil {
		return nil, linuxerr.ENOENT
	}
	ino, err := fs.diskVolume.Repository.Lookup(fs.diskVolume.Branch, dir.inode.ino, name)
	if err != nil {
		if errors.Is(err, stdfs.ErrNotExist) {
			return nil, linuxerr.ENOENT
		}
		return nil, err
	}
	childInode, err := fs.loadDiskInode(ino, dir)
	if err != nil {
		return nil, fmt.Errorf("loading %q: %w", name, err)
	}
	var child *dentry
	if childDir, ok := childInode.impl.(*directory); ok {
		child = &childDir.dentry
		dir.inode.incRef() // child directory's ".." lifetime reference.
	} else {
		child = fs.newDentry(childInode)
	}
	// insertChildLocked updates the logical child count; the count already
	// came from repository metadata, so insert only into the runtime cache.
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
	return len(entries) == 0, err
}

func pageRoundedDiskBytes(size uint64) uint64 {
	if size == 0 {
		return 0
	}
	return (size + hostarch.PageSize - 1) &^ (hostarch.PageSize - 1)
}

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
			next = current + newBytes - oldBytes
		} else {
			delta := oldBytes - newBytes
			if delta > current {
				panic(fmt.Sprintf("ttcvol disk byte accounting underflow: current=%d delta=%d", current, delta))
			}
			next = current - delta
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
	var visit func(*dentry)
	visit = func(d *dentry) {
		inodes[d.inode.ino] = d.inode
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
	}
	fs.liveForkMu.Unlock()
	fs.diskInodesMu.Lock()
	fs.diskInodes = inodes
	fs.diskInodesMu.Unlock()
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
}

// pruneDiskNamespaceForSave makes state serialization proportional to live
// runtime references instead of to the number of closed paths cached by this
// process. The complete source cache is restored by
// restoreDiskNamespaceAfterSave; the clone resolves omitted entries from its
// already-forked repository branch.
func (fs *filesystem) pruneDiskNamespaceForSave() {
	if fs.diskVolume == nil || fs.diskSaveChildren != nil {
		return
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.diskSaveChildren = make(map[*directory]map[string]*dentry)
	var collect func(*directory)
	collect = func(dir *directory) {
		original := dir.childMap
		fs.diskSaveChildren[dir] = original
		for _, child := range original {
			if childDir, ok := child.inode.impl.(*directory); ok {
				collect(childDir)
			}
		}
	}
	collect(fs.root.inode.impl.(*directory))
	filtered := make(map[*directory]map[string]*dentry, len(fs.diskSaveChildren))
	for dir, original := range fs.diskSaveChildren {
		kept := make(map[string]*dentry)
		for name, child := range original {
			if diskDentryNeedsSave(child) {
				kept[name] = child
			}
		}
		if len(kept) == 0 {
			kept = nil
		}
		filtered[dir] = kept
	}
	for dir, kept := range filtered {
		dir.childMap = kept
		dir.childList.Reset()
		for _, child := range kept {
			child.SetPrev(nil)
			child.SetNext(nil)
			dir.childList.PushBack(child)
		}
	}
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
		return err
	}
	inode.diskMetadataGeneration.CompareAndSwap(generation, 0)
	return nil
}

func (fs *filesystem) persistDiskWriteMetadata(inode *inode) error {
	if fs.diskVolume == nil {
		return nil
	}
	generation := inode.diskMetadataGeneration.Load()
	if err := fs.diskVolume.Repository.UpdateAfterWrite(fs.diskVolume.Branch, fs.diskMetadata(inode)); err != nil {
		return err
	}
	inode.diskMetadataGeneration.CompareAndSwap(generation, 0)
	return nil
}

// flushDiskWriteMetadata publishes metadata deferred by ordinary regular-file
// writes. liveForkFiles contains every live regular file, including unlinked
// files that remain reachable through an open description.
func (fs *filesystem) flushDiskWriteMetadata() error {
	if fs.diskVolume == nil {
		return nil
	}
	fs.liveForkMu.Lock()
	for file := range fs.liveForkFiles {
		if err := file.flushDiskMetadata(); err != nil {
			fs.liveForkMu.Unlock()
			return err
		}
	}
	fs.liveForkMu.Unlock()

	fs.diskInodesMu.Lock()
	inodes := make([]*inode, 0, len(fs.diskInodes))
	for _, node := range fs.diskInodes {
		inodes = append(inodes, node)
	}
	fs.diskInodesMu.Unlock()
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
	if len(pending) == 0 {
		return nil
	}
	updates := make([]ttcvolume.Inode, len(pending))
	for idx := range pending {
		updates[idx] = pending[idx].metadata
	}
	if err := fs.diskVolume.Repository.UpdateMetadataBatch(fs.diskVolume.Branch, updates); err != nil {
		return err
	}
	for _, update := range pending {
		update.inode.diskMetadataGeneration.CompareAndSwap(update.generation, 0)
	}
	return nil
}
