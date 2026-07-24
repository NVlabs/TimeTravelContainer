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
	"fmt"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/ttcvolume"
)

func (fs *filesystem) loadDiskVolume(ctx context.Context) (*dentry, error) {
	if fs.diskVolume == nil || fs.diskVolume.Repository == nil {
		return nil, fmt.Errorf("missing TTC volume repository")
	}
	if _, err := fs.diskVolume.Repository.Branch(fs.diskVolume.Branch); err != nil {
		return nil, err
	}
	loaded := make(map[uint64]*inode)
	visiting := make(map[uint64]bool)
	rootInode, err := fs.loadDiskInode(ttcvolume.RootInode, nil, loaded, visiting)
	if err != nil {
		return nil, err
	}
	rootDir, ok := rootInode.impl.(*directory)
	if !ok {
		return nil, fmt.Errorf("volume root inode %d is not a directory", ttcvolume.RootInode)
	}
	return &rootDir.dentry, nil
}

func (fs *filesystem) loadDiskInode(ino uint64, parent *directory, loaded map[uint64]*inode, visiting map[uint64]bool) (*inode, error) {
	if inode, ok := loaded[ino]; ok {
		if inode.isDir() {
			return nil, fmt.Errorf("volume directory inode %d has multiple parents", ino)
		}
		return inode, nil
	}
	if visiting[ino] {
		return nil, fmt.Errorf("volume inode cycle at %d", ino)
	}
	visiting[ino] = true
	defer delete(visiting, ino)
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
	default:
		return nil, fmt.Errorf("unsupported persistent inode %d type %q", ino, meta.Type)
	}
	if err != nil {
		return nil, err
	}
	fs.applyDiskMetadata(inode, meta)
	loaded[ino] = inode
	if dir, ok := inode.impl.(*directory); ok {
		entries, err := fs.diskVolume.Repository.ListDirectory(fs.diskVolume.Branch, ino)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if len(entry.Name) > fs.maxFilenameLen {
				return nil, fmt.Errorf("volume entry %q exceeds maximum filename length", entry.Name)
			}
			childInode, err := fs.loadDiskInode(entry.Inode, dir, loaded, visiting)
			if err != nil {
				return nil, fmt.Errorf("loading %q: %w", entry.Name, err)
			}
			var child *dentry
			if childDir, ok := childInode.impl.(*directory); ok {
				child = &childDir.dentry
				dir.inode.incRef() // child directory's ".." lifetime reference.
			} else {
				child = fs.newDentry(childInode)
			}
			dir.insertChildLocked(child, entry.Name)
		}
	}
	return inode, nil
}

func (fs *filesystem) applyDiskMetadata(inode *inode, meta ttcvolume.Inode) {
	fileType := inode.mode.Load() & linux.S_IFMT
	inode.mode.Store(fileType | meta.Mode&07777)
	inode.uid.Store(meta.UID)
	inode.gid.Store(meta.GID)
	inode.nlink.Store(meta.Nlink)
	inode.ino = meta.Number
	inode.atime.Store(meta.AtimeNsec)
	inode.btime.Store(meta.BtimeNsec)
	inode.ctime.Store(meta.CtimeNsec)
	inode.mtime.Store(meta.MtimeNsec)
	inode.xattrs.SetRawXattrs(meta.Xattrs)
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
	}
	return meta
}

func (fs *filesystem) persistDiskMetadata(inode *inode) error {
	if fs.diskVolume == nil {
		return nil
	}
	return fs.diskVolume.Repository.UpdateMetadata(fs.diskVolume.Branch, fs.diskMetadata(inode))
}

func (fs *filesystem) persistDiskWriteMetadata(inode *inode) error {
	if fs.diskVolume == nil {
		return nil
	}
	return fs.diskVolume.Repository.UpdateAfterWrite(fs.diskVolume.Branch, fs.diskMetadata(inode))
}
