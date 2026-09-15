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

package tmpfs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/contexttest"
	"gvisor.dev/gvisor/pkg/ttcvolume"
)

func TestInstallDiskVolumeRepositoryCanonicalizesRemoteIdentity(t *testing.T) {
	manager, err := ttcvolume.OpenManager(t.TempDir())
	if err != nil {
		t.Fatalf("OpenManager: %v", err)
	}
	repository, err := manager.Create("restore-identity-test")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer repository.Close()

	fs := &filesystem{diskVolume: &diskVolumeState{
		ResourceID: checkpoint.ResourceID{ContainerName: "ttc-volume", Path: "source-node-repository"},
	}}
	fs.installDiskVolumeRepository(repository, "remote-child")
	want := checkpoint.ResourceID{ContainerName: "ttc-volume", Path: repository.Info().ID}
	if got := fs.diskVolume.ResourceID; got != want {
		t.Fatalf("installed resource ID=%q, want target-local %q", got, want)
	}
	if fs.diskVolume.Repository != repository || fs.diskVolume.Branch != "remote-child" {
		t.Fatalf("installed disk volume=%+v", fs.diskVolume)
	}
}

func TestDiskVolumeErrorNeverLeaksUnknownSyscallError(t *testing.T) {
	if got := diskVolumeError("test invariant", errors.New("broken graph edge")); !linuxerr.Equals(linuxerr.EIO, got) {
		t.Fatalf("generic repository error translated to %v, want EIO", got)
	}
	if got := diskVolumeError("test errno", linuxerr.ENOSPC); !linuxerr.Equals(linuxerr.ENOSPC, got) {
		t.Fatalf("Linux error translated to %v, want ENOSPC", got)
	}
	pathErr := &os.PathError{Op: "open", Path: "object", Err: unix.EROFS}
	if got := diskVolumeError("test path errno", pathErr); !linuxerr.Equals(linuxerr.EROFS, got) {
		t.Fatalf("host path error translated to %v, want EROFS", got)
	}
	if got := diskVolumeError("test missing", os.ErrNotExist); !linuxerr.Equals(linuxerr.ENOENT, got) {
		t.Fatalf("portable missing error translated to %v, want ENOENT", got)
	}
}

func TestDiskVolumeMmapCoWClosesStaleIOObject(t *testing.T) {
	manager, err := ttcvolume.OpenManager(t.TempDir())
	if err != nil {
		t.Fatalf("OpenManager: %v", err)
	}
	repository, err := manager.Create("mmap-cow-cache-test")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer repository.Close()
	ino, err := repository.CreateRegular(ttcvolume.MainBranch, ttcvolume.RootInode, "mapped", 0644, 0, 0)
	if err != nil {
		t.Fatalf("CreateRegular: %v", err)
	}
	const contents = "mapped object contents"
	if _, _, err := repository.WriteAt(ttcvolume.MainBranch, ino, []byte(contents), 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	before, err := repository.Inode(ttcvolume.MainBranch, ino)
	if err != nil {
		t.Fatalf("Inode before fork: %v", err)
	}
	if err := repository.Fork(ttcvolume.MainBranch, "child"); err != nil {
		t.Fatalf("Fork: %v", err)
	}

	fs := &filesystem{
		diskVolume:    &diskVolumeState{Repository: repository, Branch: ttcvolume.MainBranch},
		liveForkFiles: make(map[*regularFile]struct{}),
	}
	file := &regularFile{}
	file.inode.fs = fs
	file.inode.impl = file
	file.inode.ino = ino
	file.inode.mode.Store(uint32(linux.S_IFREG | 0644))
	file.inode.nlink.Store(1)
	file.size.Store(uint64(len(contents)))
	file.diskMmap.SetFD(-1)
	fs.liveForkFiles[file] = struct{}{}
	defer func() {
		file.invalidateDiskIO()
		file.diskMmap.MappableRelease()
	}()

	// Model an ELF-header or ordinary read after the fork. This caches the
	// still-shared object before the first mmap fault selects a private object.
	file.diskIOMu.Lock()
	cached, _, err := file.openDiskIOLocked(false, nil)
	if err != nil {
		file.diskIOMu.Unlock()
		t.Fatalf("opening read cache: %v", err)
	}
	oldFD := int(cached.Fd())
	if file.diskIOObject != before.Object {
		file.diskIOMu.Unlock()
		t.Fatalf("cached object=%q, want pre-CoW object %q", file.diskIOObject, before.Object)
	}
	file.diskIOMu.Unlock()

	file.mapsMu.Lock()
	err = file.ensureDiskMmapLocked()
	file.mapsMu.Unlock()
	if err != nil {
		t.Fatalf("ensureDiskMmapLocked: %v", err)
	}
	after, err := repository.Inode(ttcvolume.MainBranch, ino)
	if err != nil {
		t.Fatalf("Inode after mmap CoW: %v", err)
	}
	if after.Object == before.Object {
		t.Fatalf("mmap fault did not privatize shared object %q", before.Object)
	}
	file.diskIOMu.Lock()
	if file.diskIOFile != nil || file.diskIOObject != "" {
		file.diskIOMu.Unlock()
		t.Fatalf("mmap CoW retained stale I/O cache: file=%v object=%q", file.diskIOFile, file.diskIOObject)
	}
	file.diskIOMu.Unlock()
	if _, err := unix.FcntlInt(uintptr(oldFD), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
		t.Fatalf("pre-CoW cached FD %d remains open: %v", oldFD, err)
	}

	if err := repository.RemoveBranch("child"); err != nil {
		t.Fatalf("RemoveBranch(child): %v", err)
	}
	if err := repository.GarbageCollect(); err != nil {
		t.Fatalf("GarbageCollect: %v", err)
	}
	oldPath := filepath.Join(repository.HostPath(), "objects", before.Object[:2], before.Object[2:4], before.Object)
	if _, err := os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unreachable pre-CoW object survived collection: %v", err)
	}

	file.diskIOMu.Lock()
	current, _, err := file.openDiskIOLocked(false, nil)
	if err != nil {
		file.diskIOMu.Unlock()
		t.Fatalf("opening current read cache: %v", err)
	}
	buf := make([]byte, len(contents))
	n, readErr := current.ReadAt(buf, 0)
	currentObject := file.diskIOObject
	file.diskIOMu.Unlock()
	if readErr != nil || n != len(buf) || string(buf) != contents {
		t.Fatalf("reading current object: n=%d data=%q err=%v", n, buf, readErr)
	}
	if currentObject != after.Object {
		t.Fatalf("current read cache object=%q, want %q", currentObject, after.Object)
	}
}

func TestPrepareLiveForkClosesDiskMmapObject(t *testing.T) {
	manager, err := ttcvolume.OpenManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := manager.Create("fork-mmap-cache-test")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	ino, err := repository.CreateRegular(ttcvolume.MainBranch, ttcvolume.RootInode, "mapped", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.WriteAt(ttcvolume.MainBranch, ino, []byte("page"), 0); err != nil {
		t.Fatal(err)
	}
	fs := &filesystem{diskVolume: &diskVolumeState{Repository: repository, Branch: ttcvolume.MainBranch}}
	file := &regularFile{}
	file.inode.fs = fs
	file.inode.impl = file
	file.inode.ino = ino
	file.size.Store(4)
	file.diskMmap.SetFD(-1)
	ctx := contexttest.Context(t)
	ms := &recordingMappingSpace{}
	ar := hostarch.AddrRange{Start: 0x10000, End: 0x10000 + hostarch.PageSize}
	if err := file.AddMapping(ctx, ms, ar, 0, false); err != nil {
		t.Fatalf("AddMapping: %v", err)
	}
	file.mapsMu.Lock()
	err = file.ensureDiskMmapLocked()
	oldFD := file.diskMmap.FD()
	file.mapsMu.Unlock()
	if err != nil {
		t.Fatalf("ensureDiskMmapLocked: %v", err)
	}
	if oldFD < 0 {
		t.Fatal("mmap cache did not install a host FD")
	}

	file.prepareLiveFork()
	if got := file.diskMmap.FD(); got != -1 {
		t.Fatalf("prepareLiveFork retained mmap FD %d", got)
	}
	if _, err := unix.FcntlInt(uintptr(oldFD), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
		t.Fatalf("prepareLiveFork left old mmap FD %d open: %v", oldFD, err)
	}
	if !ms.invalidated {
		t.Fatal("prepareLiveFork did not invalidate the application mapping")
	}
	// Filesystem PrepareSave removes the current invalidation set before it
	// calls prepareLiveFork. Existing VMAs survive, so their next fault reaches
	// Translate without another AddMapping call. Model that boundary here.
	fs.liveForkMu.Lock()
	fs.diskForkFiles = nil
	fs.liveForkMu.Unlock()
	if err := repository.Fork(ttcvolume.MainBranch, "child"); err != nil {
		t.Fatalf("Fork: %v", err)
	}
	file.mapsMu.Lock()
	err = file.ensureDiskMmapLocked()
	file.mapsMu.Unlock()
	if err != nil {
		t.Fatalf("reinstalling mmap object after fork: %v", err)
	}
	if got := file.diskMmap.FD(); got < 0 {
		t.Fatal("mmap cache did not install a new FD after fork")
	}
	fs.liveForkMu.Lock()
	_, trackedAfterRefault := fs.diskForkFiles[file]
	fs.liveForkMu.Unlock()
	if !trackedAfterRefault {
		t.Fatal("post-fork mmap refault was not tracked for the next lifecycle fence")
	}
	file.RemoveMapping(ctx, ms, ar, 0, false)
	file.diskMmap.MappableRelease()
}

func TestDiskVolumeWriteMetadataIsCoalesced(t *testing.T) {
	manager, err := ttcvolume.OpenManager(t.TempDir())
	if err != nil {
		t.Fatalf("OpenManager: %v", err)
	}
	repository, err := manager.Create("writeback-test")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer repository.Close()
	ino, err := repository.CreateRegular(ttcvolume.MainBranch, ttcvolume.RootInode, "download", 0644, 0, 0)
	if err != nil {
		t.Fatalf("CreateRegular: %v", err)
	}
	branchBefore, err := repository.Branch(ttcvolume.MainBranch)
	if err != nil {
		t.Fatal(err)
	}

	fs := &filesystem{
		diskVolume:    &diskVolumeState{Repository: repository, Branch: ttcvolume.MainBranch},
		liveForkFiles: make(map[*regularFile]struct{}),
	}
	file := &regularFile{}
	file.inode.fs = fs
	file.inode.impl = file
	file.inode.ino = ino
	file.inode.mode.Store(uint32(linux.S_IFREG | 0644))
	file.inode.nlink.Store(1)
	file.size.Store(8 << 20)
	fs.liveForkFiles[file] = struct{}{}

	file.inode.mu.Lock()
	file.markDiskMetadataDirtyLocked()
	file.inode.mu.Unlock()
	meta, err := repository.Inode(ttcvolume.MainBranch, ino)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Size != 0 {
		t.Fatalf("metadata was published before a flush: size=%d", meta.Size)
	}
	branchDirty, err := repository.Branch(ttcvolume.MainBranch)
	if err != nil {
		t.Fatal(err)
	}
	if branchDirty.Root != branchBefore.Root {
		t.Fatalf("ordinary write changed repository root: before=%q after=%q", branchBefore.Root, branchDirty.Root)
	}

	if err := fs.flushDiskWriteMetadata(); err != nil {
		t.Fatalf("flushDiskWriteMetadata: %v", err)
	}
	meta, err = repository.Inode(ttcvolume.MainBranch, ino)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Size != 8<<20 {
		t.Fatalf("flushed size=%d, want %d", meta.Size, 8<<20)
	}
	if file.diskMetadataDirty.Load() != 0 {
		t.Fatal("successful flush left inode dirty")
	}
	branchFlushed, err := repository.Branch(ttcvolume.MainBranch)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.flushDiskWriteMetadata(); err != nil {
		t.Fatalf("second flushDiskWriteMetadata: %v", err)
	}
	branchAgain, err := repository.Branch(ttcvolume.MainBranch)
	if err != nil {
		t.Fatal(err)
	}
	if branchAgain.Root != branchFlushed.Root {
		t.Fatalf("clean flush rewrote repository root: before=%q after=%q", branchFlushed.Root, branchAgain.Root)
	}
}

func TestDiskVolumeReapedOrphanLeavesNoSaveCache(t *testing.T) {
	manager, err := ttcvolume.OpenManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := manager.Create("orphan-save-cache-test")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	ino, err := repository.CreateRegular(ttcvolume.MainBranch, ttcvolume.RootInode, "temporary", 0600, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Unlink(ttcvolume.MainBranch, ttcvolume.RootInode, "temporary"); err != nil {
		t.Fatal(err)
	}

	fs := &filesystem{
		diskVolume:    &diskVolumeState{Repository: repository, Branch: ttcvolume.MainBranch},
		diskInodes:    make(map[uint64]*inode),
		liveForkFiles: make(map[*regularFile]struct{}),
	}
	fs.inodesUsed.Store(1)
	file := &regularFile{}
	file.inode.fs = fs
	file.inode.impl = file
	file.inode.ino = ino
	file.inode.mode.Store(uint32(linux.S_IFREG | 0600))
	file.inode.nlink.Store(0)
	file.inode.refs.InitRefs()
	file.diskMmap.SetFD(-1)
	fs.diskInodes[ino] = &file.inode
	fs.liveForkFiles[file] = struct{}{}
	fs.markDiskFileDirty(file)
	fs.markDiskInodeDirty(&file.inode)

	file.inode.decRef(contexttest.Context(t))
	if _, ok := fs.diskInodes[ino]; ok {
		t.Fatal("reaped orphan remained in disk inode save cache")
	}
	if _, ok := fs.liveForkFiles[file]; ok {
		t.Fatal("reaped orphan remained in live-fork file cache")
	}
	if _, err := repository.Inode(ttcvolume.MainBranch, ino); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reaped orphan repository lookup: %v", err)
	}
	if err := fs.flushDiskWriteMetadata(); err != nil {
		t.Fatalf("save-time metadata flush after orphan reaping: %v", err)
	}
}

func TestDiskNamespaceSavePrunesClosedPathsAndRestoresSource(t *testing.T) {
	fs := &filesystem{diskVolume: &diskVolumeState{}}
	newDir := func() *directory {
		dir := &directory{}
		dir.inode.fs = fs
		dir.inode.impl = dir
		dir.inode.refs.InitRefs()
		dir.dentry.inode = &dir.inode
		return dir
	}
	newFile := func() *dentry {
		link := &symlink{}
		link.inode.fs = fs
		link.inode.impl = link
		link.inode.refs.InitRefs()
		return &dentry{inode: &link.inode}
	}

	root := newDir()
	fs.root = &root.dentry
	inactive := newFile()
	active := newFile()
	active.IncRef() // An open FD, cwd, mount, or equivalent VFS owner.
	subdir := newDir()
	subdir.dentry.IncRef() // Keep the directory itself live.
	root.childMap = map[string]*dentry{
		"inactive": inactive,
		"active":   active,
		"subdir":   &subdir.dentry,
	}
	for name, child := range root.childMap {
		child.name = name
		child.parent.Store(&root.dentry)
		root.childList.PushBack(child)
	}

	fs.pruneDiskNamespaceForSave()
	if _, ok := root.childMap["inactive"]; ok {
		t.Fatal("closed persistent path remained in serialized namespace")
	}
	if root.childMap["active"] != active || root.childMap["subdir"] != &subdir.dentry {
		t.Fatalf("runtime-referenced paths were pruned: %+v", root.childMap)
	}
	fs.restoreDiskNamespaceAfterSave()
	if len(root.childMap) != 3 || root.childMap["inactive"] != inactive {
		t.Fatalf("source namespace was not restored: %+v", root.childMap)
	}
}

func TestDiskNamespaceSaveAcceptsRegularFileRoot(t *testing.T) {
	fs := &filesystem{diskVolume: &diskVolumeState{}}
	file := &regularFile{}
	file.inode.fs = fs
	file.inode.impl = file
	file.inode.refs.InitRefs()
	fs.root = &dentry{inode: &file.inode}

	fs.pruneDiskNamespaceForSave()
	if fs.diskSaveChildren != nil {
		t.Fatalf("regular-file root unexpectedly created saved namespace: %+v", fs.diskSaveChildren)
	}
	fs.restoreDiskNamespaceAfterSave()
}
