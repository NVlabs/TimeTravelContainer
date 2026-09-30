// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tmpfs

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/contexttest"
	"gvisor.dev/gvisor/pkg/ttcvolume"
	"gvisor.dev/gvisor/pkg/usermem"
)

func TestMaterializationDoesNotLockUnrelatedFile(t *testing.T) {
	fs := &filesystem{diskVolume: &diskVolumeState{}}
	target := &regularFile{}
	target.inode.fs = fs
	target.diskMmap.SetFD(-1)
	target.mapsMu.Lock()
	target.setDiskMmapObjectLocked("target-object")
	target.mapsMu.Unlock()

	unrelated := &regularFile{}
	unrelated.inode.fs = fs
	unrelated.diskMmap.SetFD(-1)
	unrelated.mapsMu.Lock()
	unrelated.setDiskMmapObjectLocked("unrelated-object")
	// Model an unrelated write that holds mapsMu while waiting for the remote
	// client. Materializing target-object must not wait for this file.
	done := make(chan struct{})
	go func() {
		fs.invalidateMaterializedObject("target-object")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		unrelated.mapsMu.Unlock()
		t.Fatal("materialization waited for an unrelated file cache")
	}
	if target.diskObject != "" {
		unrelated.mapsMu.Unlock()
		t.Fatalf("target cache was not invalidated: %q", target.diskObject)
	}
	if unrelated.diskObject != "unrelated-object" {
		unrelated.mapsMu.Unlock()
		t.Fatalf("unrelated cache changed: %q", unrelated.diskObject)
	}
	unrelated.mapsMu.Unlock()
}

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

func TestDiskVolumeReadDataPathDoesNotTakeInodeLock(t *testing.T) {
	manager, err := ttcvolume.OpenManager(t.TempDir())
	if err != nil {
		t.Fatalf("OpenManager: %v", err)
	}
	repository, err := manager.Create("read-lock-order-test")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer repository.Close()
	ino, err := repository.CreateRegular(ttcvolume.MainBranch, ttcvolume.RootInode, "growing", 0644, 0, 0)
	if err != nil {
		t.Fatalf("CreateRegular: %v", err)
	}
	want := bytes.Repeat([]byte("x"), 64<<10)
	if _, _, err := repository.WriteAt(ttcvolume.MainBranch, ino, want, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	fs := &filesystem{diskVolume: &diskVolumeState{Repository: repository, Branch: ttcvolume.MainBranch}}
	file := &regularFile{}
	file.inode.fs = fs
	file.inode.impl = file
	file.inode.ino = ino
	file.inode.mode.Store(uint32(linux.S_IFREG | 0644))
	file.inode.nlink.Store(1)
	file.size.Store(uint64(len(want)))
	defer file.invalidateDiskIO()
	ctx := contexttest.Context(t)

	// A growing write holds inode.mu and then takes diskIOMu. The read data
	// path must therefore finish without inode.mu; PRead applies atime only
	// after preadDisk has released diskIOMu. The former reverse order deadlocked
	// a reader against the final growing write of a large file.
	file.inode.mu.Lock()
	type result struct {
		data        []byte
		n           int64
		err         error
		updateAtime bool
	}
	done := make(chan result, 1)
	go func() {
		got := make([]byte, len(want))
		n, err, updateAtime := file.preadDisk(ctx, usermem.BytesIOSequence(got), 0)
		done <- result{data: got, n: n, err: err, updateAtime: updateAtime}
	}()
	select {
	case got := <-done:
		file.inode.mu.Unlock()
		if got.err != nil || got.n != int64(len(want)) || !got.updateAtime {
			t.Fatalf("preadDisk: n=%d updateAtime=%t err=%v", got.n, got.updateAtime, got.err)
		}
		if !bytes.Equal(got.data, want) {
			t.Fatal("preadDisk data mismatch")
		}
	case <-time.After(time.Second):
		file.inode.mu.Unlock()
		t.Fatal("disk read data path waited for inode.mu")
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

func TestDiskVolumeFlushSkipsAnInodeReapedAfterItWasTaken(t *testing.T) {
	manager, err := ttcvolume.OpenManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := manager.Create("reaped-flush-test")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	fs := &filesystem{
		diskVolume:    &diskVolumeState{Repository: repository, Branch: ttcvolume.MainBranch},
		diskInodes:    make(map[uint64]*inode),
		liveForkFiles: make(map[*regularFile]struct{}),
	}
	fs.inodesUsed.Store(2)
	newFile := func(name string) *regularFile {
		ino, err := repository.CreateRegular(ttcvolume.MainBranch, ttcvolume.RootInode, name, 0600, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		file := &regularFile{}
		file.inode.fs = fs
		file.inode.impl = file
		file.inode.ino = ino
		file.inode.mode.Store(uint32(linux.S_IFREG | 0600))
		file.inode.nlink.Store(1)
		file.inode.refs.InitRefs()
		file.diskMmap.SetFD(-1)
		fs.diskInodes[ino] = &file.inode
		fs.liveForkFiles[file] = struct{}{}
		return file
	}
	// A file replaced by a rename, as `mv new name` replaces it, whose last
	// reference is dropped: the repository reaps it.
	replaced := newFile("replaced")
	if err := repository.Unlink(ttcvolume.MainBranch, ttcvolume.RootInode, "replaced"); err != nil {
		t.Fatal(err)
	}
	replaced.inode.nlink.Store(0)
	replaced.inode.decRef(contexttest.Context(t))
	// A flush took it as dirty just before, beside a live file it must update.
	kept := newFile("kept")
	kept.inode.mode.Store(uint32(linux.S_IFREG | 0640))
	fs.markDiskInodeDirty(&kept.inode)
	fs.markDiskInodeDirty(&replaced.inode)
	fs.markDiskFileDirty(replaced)
	if err := fs.flushDiskWriteMetadata(); err != nil {
		t.Fatalf("flushDiskWriteMetadata with a reaped inode: %v", err)
	}
	if meta, err := repository.Inode(ttcvolume.MainBranch, kept.inode.ino); err != nil || meta.Mode != 0640 {
		t.Fatalf("live inode after the flush: mode %o, %v; want 0640", meta.Mode, err)
	}
	// A live inode the repository lacks is still an error.
	lost := newFile("lost")
	if err := repository.Unlink(ttcvolume.MainBranch, ttcvolume.RootInode, "lost"); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReapOrphan(ttcvolume.MainBranch, lost.inode.ino); err != nil {
		t.Fatal(err)
	}
	fs.markDiskInodeDirty(&lost.inode)
	if err := fs.flushDiskWriteMetadata(); err == nil {
		t.Fatal("flushDiskWriteMetadata of a live inode the repository lacks succeeded")
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

// A directory this filesystem made holds every child it has on its volume:
// a name missing from it is none, even one the volume has, as nothing but
// this filesystem names files in a running branch. A directory it did not
// make asks the volume.
func TestDiskChildOfADirectoryItMadeIsNotLookedUp(t *testing.T) {
	manager, err := ttcvolume.OpenManager(t.TempDir())
	if err != nil {
		t.Fatalf("OpenManager: %v", err)
	}
	repository, err := manager.Create("complete-directory-test")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer repository.Close()
	ino, err := repository.CreateRegular(ttcvolume.MainBranch, ttcvolume.RootInode, "behind", 0644, 0, 0)
	if err != nil {
		t.Fatalf("CreateRegular: %v", err)
	}
	file := &regularFile{}
	fs := &filesystem{
		diskVolume: &diskVolumeState{Repository: repository, Branch: ttcvolume.MainBranch},
		diskInodes: map[uint64]*inode{ino: &file.inode},
	}
	file.inode.fs = fs
	file.inode.impl = file
	file.inode.ino = ino
	file.inode.mode.Store(uint32(linux.S_IFREG | 0644))
	dir := &directory{}
	dir.inode.fs = fs
	dir.inode.ino = ttcvolume.RootInode
	dir.dentry.inode = &dir.inode
	dir.dentry.vfsd.Init(&dir.dentry)

	dir.diskComplete = true
	if _, err := fs.diskChildLocked(dir, "behind"); !linuxerr.Equals(linuxerr.ENOENT, err) {
		t.Fatalf("a directory the filesystem made looked a name up in the volume: %v", err)
	}
	dir.diskComplete = false
	child, err := fs.diskChildLocked(dir, "behind")
	if err != nil || child.inode != &file.inode {
		t.Fatalf("a directory the filesystem did not make found (%v, %v), want the volume's file", child, err)
	}
}
