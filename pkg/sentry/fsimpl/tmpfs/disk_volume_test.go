// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tmpfs

import (
	"bytes"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/contexttest"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/ttcvolume"
	"gvisor.dev/gvisor/pkg/usermem"
)

// The sum of a volume's file sizes saturates: five sparse files of 4 EiB
// wrapped it, and shrinking them then panicked. Once nothing is left, neither
// is the sum.
func TestDiskBytesSaturate(t *testing.T) {
	var fs filesystem
	fs.adjustDiskBytes(0, 8192)
	for range 5 {
		fs.adjustDiskBytes(0, 1<<62)
	}
	if got := fs.diskBytes.Load(); got != math.MaxUint64 {
		t.Fatalf("five files of 4 EiB account %d bytes; want %d", got, uint64(math.MaxUint64))
	}
	for range 5 {
		fs.adjustDiskBytes(1<<62, 0)
	}
	fs.adjustDiskBytes(8192, 0)
	if got := fs.diskBytes.Load(); got != 0 {
		t.Errorf("no files account %d bytes", got)
	}
	// Short of saturating, the sum is exact.
	fs.adjustDiskBytes(0, 1)
	fs.adjustDiskBytes(0, 1<<40+1)
	fs.adjustDiskBytes(1<<40+1, 4096)
	if got, want := fs.diskBytes.Load(), uint64(2*4096); got != want {
		t.Errorf("the sum is %d; want %d", got, want)
	}
}

func TestMaterializationDoesNotLockUnrelatedFile(t *testing.T) {
	fs := &filesystem{diskVolume: &diskVolumeState{}}
	target := &regularFile{}
	target.inode.fs = fs
	target.diskMapMu.Lock()
	target.setDiskMmapObjectLocked("target-object")
	target.diskMapMu.Unlock()

	unrelated := &regularFile{}
	unrelated.inode.fs = fs
	unrelated.mapsMu.Lock()
	unrelated.diskMapMu.Lock()
	unrelated.setDiskMmapObjectLocked("unrelated-object")
	unrelated.diskMapMu.Unlock()
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

// sync(2) fsyncs on the host only the files that a writable mapping may have
// written since a sync last took them, and every file once after a restore or
// a live fork; the writes of the others mark their objects dirty, and the
// repository syncs those.
func TestDiskVolumeSyncTakesMappedFiles(t *testing.T) {
	manager, err := ttcvolume.OpenManager(t.TempDir())
	if err != nil {
		t.Fatalf("OpenManager: %v", err)
	}
	repository, err := manager.Create("sync-mapped-test")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer repository.Close()
	ino, err := repository.CreateRegular(ttcvolume.MainBranch, ttcvolume.RootInode, "mapped", 0644, 0, 0)
	if err != nil {
		t.Fatalf("CreateRegular: %v", err)
	}
	if _, _, err := repository.WriteAt(ttcvolume.MainBranch, ino, make([]byte, hostarch.PageSize), 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
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
	file.size.Store(hostarch.PageSize)
	fs.liveForkFiles[file] = struct{}{}
	defer file.releaseDiskMap()

	if file.takeDiskMappedWrites() {
		t.Error("a file never mapped writable is taken")
	}
	ctx := contexttest.Context(t)
	ms := &recordingMappingSpace{}
	ar := hostarch.AddrRange{Start: 0x10000, End: 0x10000 + hostarch.PageSize}
	if err := file.AddMapping(ctx, ms, ar, 0, false); err != nil {
		t.Fatalf("AddMapping: %v", err)
	}
	if file.takeDiskMappedWrites() {
		t.Error("a file mapped read-only is taken")
	}
	file.RemoveMapping(ctx, ms, ar, 0, false)
	if err := file.AddMapping(ctx, ms, ar, 0, true); err != nil {
		t.Fatalf("AddMapping: %v", err)
	}
	for i := 0; i < 2; i++ {
		if !file.takeDiskMappedWrites() {
			t.Errorf("sync %d does not take a file mapped writable", i)
		}
	}
	if err := fs.syncDiskData(); err != nil {
		t.Fatalf("syncDiskData: %v", err)
	}
	file.RemoveMapping(ctx, ms, ar, 0, true)
	if !file.takeDiskMappedWrites() {
		t.Error("the first sync after a writable mapping went away does not take the file")
	}
	if file.takeDiskMappedWrites() {
		t.Error("a file unmapped and taken since is taken again")
	}
	// What a restore or a live fork loads may hold unsynced mapped writes.
	file.diskMappedWrites.Store(true)
	if !file.takeDiskMappedWrites() {
		t.Error("a loaded file is not taken")
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
	fs.liveForkFiles[file] = struct{}{}
	defer func() {
		file.invalidateDiskIO()
		file.releaseDiskMap()
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

	// A write fault of a writable mapping copies the sealed object. Translate
	// cannot lock the I/O cache, which it marks stale instead.
	file.diskMapMu.Lock()
	_, _, err = file.diskTranslationLocked(true)
	file.diskMapMu.Unlock()
	if err != nil {
		t.Fatalf("diskTranslationLocked: %v", err)
	}
	after, err := repository.Inode(ttcvolume.MainBranch, ino)
	if err != nil {
		t.Fatalf("Inode after mmap CoW: %v", err)
	}
	if after.Object == before.Object {
		t.Fatalf("mmap fault did not privatize shared object %q", before.Object)
	}
	if !file.diskIOStale.Load() {
		t.Fatal("mmap CoW did not mark the I/O cache stale")
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
	if current == cached || cached.Fd() != ^uintptr(0) {
		file.diskIOMu.Unlock()
		t.Fatalf("the I/O cache kept the pre-CoW object's FD %d", oldFD)
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
	ctx := contexttest.Context(t)
	ms := &recordingMappingSpace{}
	ar := hostarch.AddrRange{Start: 0x10000, End: 0x10000 + hostarch.PageSize}
	if err := file.AddMapping(ctx, ms, ar, 0, false); err != nil {
		t.Fatalf("AddMapping: %v", err)
	}
	file.diskMapMu.Lock()
	m, _, err := file.diskTranslationLocked(false)
	oldFD := -1
	if err == nil {
		oldFD = m.FD()
	}
	file.diskMapMu.Unlock()
	if err != nil {
		t.Fatalf("diskTranslationLocked: %v", err)
	}
	if oldFD < 0 {
		t.Fatal("mmap cache did not install a host FD")
	}

	// A fork that leaves the process.
	if mustPrepareLiveFork(t, file, false /* share */) {
		t.Fatal("prepareLiveFork kept the file's mapping")
	}
	if file.diskMap != nil {
		t.Fatal("prepareLiveFork retained the mmap object")
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
	file.diskMapMu.Lock()
	m, perms, err := file.diskTranslationLocked(false)
	file.diskMapMu.Unlock()
	if err != nil {
		t.Fatalf("reinstalling mmap object after fork: %v", err)
	}
	if m.FD() < 0 || !m.LiveForkShared() || perms != hostarch.ReadExecute {
		t.Fatalf("mmap cache after fork: FD %d, shared %t, perms %v; want the sealed object's, read-only", m.FD(), m.LiveForkShared(), perms)
	}
	fs.liveForkMu.Lock()
	_, trackedAfterRefault := fs.diskForkFiles[file]
	fs.liveForkMu.Unlock()
	if !trackedAfterRefault {
		t.Fatal("post-fork mmap refault was not tracked for the next lifecycle fence")
	}
	file.RemoveMapping(ctx, ms, ar, 0, false)
	file.releaseDiskMap()
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

func TestDiskVolumeStatFSReportsTheHostFilesystem(t *testing.T) {
	manager, err := ttcvolume.OpenManager(t.TempDir())
	if err != nil {
		t.Fatalf("OpenManager: %v", err)
	}
	repository, err := manager.Create("statfs-test")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer repository.Close()
	capacity, err := repository.Capacity()
	if err != nil {
		t.Fatalf("Capacity: %v", err)
	}
	if capacity.TotalBytes == 0 || capacity.AvailableBytes > capacity.TotalBytes {
		t.Fatalf("Capacity() = %+v, want a nonzero total no smaller than what is available", capacity)
	}

	fs := &filesystem{
		maxSizeInPages: getDefaultSizeLimit(true) / hostarch.PageSize,
		diskVolume:     &diskVolumeState{Repository: repository, Branch: ttcvolume.MainBranch},
	}
	st := fs.statFS()
	if want := capacity.TotalBytes / hostarch.PageSize; st.Blocks != want {
		t.Errorf("Blocks = %d, want the host's %d", st.Blocks, want)
	}
	if st.BlocksAvailable == 0 || st.BlocksAvailable > st.Blocks || st.BlocksFree > st.Blocks {
		t.Errorf("statFS() = %+v, want free and available blocks within the host's", st)
	}

	// A size limit is the volume's size.
	fs.maxSizeInPages = 256
	if st := fs.statFS(); st.Blocks != 256 || st.BlocksAvailable != 256 {
		t.Errorf("with a limit of 256 pages, statFS() = %+v", st)
	}
}

// newTestDiskRepository returns a repository with a file of contents, and the
// file's inode.
func newTestDiskRepository(t *testing.T, name, contents string) (*ttcvolume.Repository, uint64) {
	t.Helper()
	manager, err := ttcvolume.OpenManager(t.TempDir())
	if err != nil {
		t.Fatalf("OpenManager: %v", err)
	}
	repository, err := manager.Create(name)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { repository.Close() })
	ino, err := repository.CreateRegular(ttcvolume.MainBranch, ttcvolume.RootInode, "mapped", 0644, 0, 0)
	if err != nil {
		t.Fatalf("CreateRegular: %v", err)
	}
	if _, _, err := repository.WriteAt(ttcvolume.MainBranch, ino, []byte(contents), 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	return repository, ino
}

// newTestDiskFile returns the file ino, of size bytes, of a branch of
// repository, as a filesystem of the branch has it.
func newTestDiskFile(repository *ttcvolume.Repository, branch string, ino uint64, size int) *regularFile {
	fs := &filesystem{
		diskVolume:    &diskVolumeState{Repository: repository, Branch: branch},
		liveForkFiles: make(map[*regularFile]struct{}),
	}
	file := &regularFile{}
	file.inode.fs = fs
	file.inode.impl = file
	file.inode.ino = ino
	file.inode.mode.Store(uint32(linux.S_IFREG | 0644))
	file.inode.nlink.Store(1)
	file.size.Store(uint64(size))
	fs.liveForkFiles[file] = struct{}{}
	return file
}

// translatePage returns the translation of file's first page for at.
func translatePage(t *testing.T, file *regularFile, at hostarch.AccessType) memmap.Translation {
	t.Helper()
	page := memmap.MappableRange{Start: 0, End: hostarch.PageSize}
	ts, err := file.Translate(contexttest.Context(t), page, page, at)
	if err != nil || len(ts) != 1 {
		t.Fatalf("Translate(%v): %v, %v", at, ts, err)
	}
	return ts[0]
}

// readPage returns n bytes that translation t maps.
func readPage(t *testing.T, tr memmap.Translation, n int) string {
	t.Helper()
	bs, err := tr.File.MapInternal(tr.FileRange(), hostarch.Read)
	if err != nil {
		t.Fatalf("MapInternal: %v", err)
	}
	buf := make([]byte, n)
	if _, err := safemem.CopySeq(safemem.BlockSeqOf(safemem.BlockFromSafeSlice(buf)), bs); err != nil {
		t.Fatalf("reading a translation: %v", err)
	}
	return string(buf)
}

func TestLiveForkSharesSealedDiskMappings(t *testing.T) {
	const contents = "sealed contents"
	repository, ino := newTestDiskRepository(t, "shared-mapping-test", contents)
	parent := newTestDiskFile(repository, ttcvolume.MainBranch, ino, len(contents))
	ctx := contexttest.Context(t)
	ms := &recordingMappingSpace{}
	ar := hostarch.AddrRange{Start: 0x10000, End: 0x10000 + hostarch.PageSize}
	if err := parent.AddMapping(ctx, ms, ar, 0, false); err != nil {
		t.Fatalf("AddMapping: %v", err)
	}
	before := translatePage(t, parent, hostarch.Read)
	if before.File.(memmap.LiveForkSharedFile).LiveForkShared() {
		t.Fatal("a mapping of the branch's private object is shared")
	}

	// An in-process fork keeps the translations of a file that nothing maps
	// writable: what they map, the fork seals.
	if !mustPrepareLiveFork(t, parent, true /* share */) {
		t.Fatal("prepareLiveFork did not keep the file's mapping")
	}
	if ms.invalidated {
		t.Fatal("prepareLiveFork invalidated translations of the sealed object")
	}
	if !before.File.(memmap.LiveForkSharedFile).LiveForkShared() {
		t.Fatal("the fork's child cannot share the parent's translations")
	}
	if err := repository.Fork(ttcvolume.MainBranch, "child"); err != nil {
		t.Fatalf("Fork: %v", err)
	}
	object, err := repository.Inode(ttcvolume.MainBranch, ino)
	if err != nil {
		t.Fatalf("Inode: %v", err)
	}

	// Both branches map the sealed object with the parent's mapping, which
	// neither copies.
	child := newTestDiskFile(repository, "child", ino, len(contents))
	for _, file := range []*regularFile{parent, child} {
		tr := translatePage(t, file, hostarch.Read)
		if tr.File != before.File || tr.Perms != hostarch.ReadExecute {
			t.Fatalf("a branch maps %p, %v, want the shared mapping %p, read-only", tr.File, tr.Perms, before.File)
		}
		if got := readPage(t, tr, len(contents)); got != contents {
			t.Fatalf("the shared mapping reads %q, want %q", got, contents)
		}
	}
	after, err := repository.Inode(ttcvolume.MainBranch, ino)
	if err != nil {
		t.Fatalf("Inode: %v", err)
	}
	if after.Object != object.Object {
		t.Fatalf("reading copied the sealed object %q to %q", object.Object, after.Object)
	}

	parent.RemoveMapping(ctx, ms, ar, 0, false)
	parent.releaseDiskMap()
	child.releaseDiskMap()
	diskMappings.mu.Lock()
	_, ok := diskMappings.m[diskMappingKey{repository, object.Object}]
	diskMappings.mu.Unlock()
	if ok {
		t.Fatal("the shared mapping outlives the files that used it")
	}
}

func TestDiskVolumeWritableMappingAfterForkCopies(t *testing.T) {
	const contents = "sealed contents"
	repository, ino := newTestDiskRepository(t, "writable-mapping-test", contents)
	parent := newTestDiskFile(repository, ttcvolume.MainBranch, ino, len(contents))
	ctx := contexttest.Context(t)
	ms := &recordingMappingSpace{}
	ar := hostarch.AddrRange{Start: 0x10000, End: 0x10000 + hostarch.PageSize}
	if err := parent.AddMapping(ctx, ms, ar, 0, false); err != nil {
		t.Fatalf("AddMapping: %v", err)
	}
	sealed := translatePage(t, parent, hostarch.Read)
	mustPrepareLiveFork(t, parent, true /* share */)
	if err := repository.Fork(ttcvolume.MainBranch, "child"); err != nil {
		t.Fatalf("Fork: %v", err)
	}
	child := newTestDiskFile(repository, "child", ino, len(contents))
	if tr := translatePage(t, child, hostarch.Read); tr.File != sealed.File {
		t.Fatal("the child does not share the sealed object's mapping")
	}

	// A writable mapping makes the parent's translations of the sealed
	// object fault again, to map the copy that its writes make.
	wms := &recordingMappingSpace{}
	war := hostarch.AddrRange{Start: 0x20000, End: 0x20000 + hostarch.PageSize}
	if err := parent.AddMapping(ctx, wms, war, 0, true); err != nil {
		t.Fatalf("AddMapping(writable): %v", err)
	}
	if !ms.invalidated {
		t.Fatal("translations of the sealed object survive a writable mapping")
	}
	for _, at := range []hostarch.AccessType{hostarch.Read, hostarch.ReadWrite} {
		tr := translatePage(t, parent, at)
		if tr.File == sealed.File || tr.Perms != hostarch.AnyAccess {
			t.Fatalf("a %v translation of a file with writable mappings maps %p, %v; want the branch's object, writable", at, tr.File, tr.Perms)
		}
	}
	tr := translatePage(t, parent, hostarch.ReadWrite)
	bs, err := tr.File.MapInternal(tr.FileRange(), hostarch.Write)
	if err != nil {
		t.Fatalf("MapInternal(write): %v", err)
	}
	const written = "PARENT"
	if _, err := safemem.CopySeq(bs, safemem.BlockSeqOf(safemem.BlockFromSafeSlice([]byte(written)))); err != nil {
		t.Fatalf("writing: %v", err)
	}
	if got := readPage(t, translatePage(t, parent, hostarch.Read), len(written)); got != written {
		t.Fatalf("the parent reads %q, want what it wrote, %q", got, written)
	}
	if got := readPage(t, translatePage(t, child, hostarch.Read), len(contents)); got != contents {
		t.Fatalf("the child reads %q, want the sealed object's %q", got, contents)
	}
	parentInode, err := repository.Inode(ttcvolume.MainBranch, ino)
	if err != nil {
		t.Fatalf("Inode: %v", err)
	}
	childInode, err := repository.Inode("child", ino)
	if err != nil {
		t.Fatalf("Inode(child): %v", err)
	}
	if parentInode.Object == childInode.Object {
		t.Fatal("the parent's writable mapping did not copy the sealed object")
	}

	parent.RemoveMapping(ctx, ms, ar, 0, false)
	parent.RemoveMapping(ctx, wms, war, 0, true)
	parent.releaseDiskMap()
	child.releaseDiskMap()
}

func TestDiskVolumeTranslateDoesNotLockMapsMu(t *testing.T) {
	const contents = "contents"
	repository, ino := newTestDiskRepository(t, "translate-lock-test", contents)
	file := newTestDiskFile(repository, ttcvolume.MainBranch, ino, len(contents))
	defer file.releaseDiskMap()

	// A change of the file's object holds mapsMu while it invalidates
	// translations, which waits for every MemoryManager that maps the file,
	// and so for their faults, which Translate serves.
	file.mapsMu.Lock()
	done := make(chan error, 1)
	go func() {
		page := memmap.MappableRange{Start: 0, End: hostarch.PageSize}
		_, err := file.Translate(contexttest.Context(t), page, page, hostarch.Read)
		done <- err
	}()
	select {
	case err := <-done:
		file.mapsMu.Unlock()
		if err != nil {
			t.Fatalf("Translate: %v", err)
		}
	case <-time.After(time.Second):
		file.mapsMu.Unlock()
		<-done
		t.Fatal("Translate waited for mapsMu")
	}
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

// A file whose volume data ensureDiskData has found local is not looked up in
// its branch again; here, its branch goes missing, which a lookup would fail
// on, and the file's data is still ensured.
func TestDiskDataFoundLocalIsNotLookedUpAgain(t *testing.T) {
	manager, err := ttcvolume.OpenManager(t.TempDir())
	if err != nil {
		t.Fatalf("OpenManager: %v", err)
	}
	repository, err := manager.Create("local-data-test")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer repository.Close()
	ino, err := repository.CreateRegular(ttcvolume.MainBranch, ttcvolume.RootInode, "local", 0644, 0, 0)
	if err != nil {
		t.Fatalf("CreateRegular: %v", err)
	}
	data := []byte("local data")
	if _, _, err := repository.WriteAt(ttcvolume.MainBranch, ino, data, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	fs := &filesystem{diskVolume: &diskVolumeState{Repository: repository, Branch: ttcvolume.MainBranch}}
	newFile := func() *regularFile {
		file := &regularFile{}
		file.inode.fs = fs
		file.inode.impl = file
		file.inode.ino = ino
		file.inode.mode.Store(uint32(linux.S_IFREG | 0644))
		file.size.Store(uint64(len(data)))
		return file
	}
	file := newFile()
	if err := file.ensureDiskData(); err != nil || !file.diskDataLocal.Load() {
		t.Fatalf("ensureDiskData: %v, local=%t", err, file.diskDataLocal.Load())
	}
	fs.diskVolume.Branch = "missing"
	if err := newFile().ensureDiskData(); err == nil {
		t.Fatal("a file not yet found local was ensured without its branch")
	}
	if err := file.ensureDiskData(); err != nil {
		t.Fatalf("a file found local was looked up again: %v", err)
	}
}
