// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcvolume

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// A guest fsync is scoped to the branch's metadata: it makes the WAL durable
// without syncing other files' payloads, which a full fence still does.
func TestBranchSyncLeavesOtherFilesDataToTheFullFence(t *testing.T) {
	manager := openTestManager(t)
	repo, err := manager.Create("scoped-fsync")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	small, err := repo.CreateRegular(MainBranch, RootInode, "small", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	large, err := repo.CreateRegular(MainBranch, RootInode, "large", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	write(t, repo, MainBranch, small, "committed", 0)
	write(t, repo, MainBranch, large, "not yet", 0)
	if err := repo.SyncData(MainBranch, small, false); err != nil {
		t.Fatal(err)
	}
	if err := repo.SyncBranchMetadata(MainBranch); err != nil {
		t.Fatal(err)
	}
	largeInode, err := repo.Inode(MainBranch, large)
	if err != nil {
		t.Fatal(err)
	}
	repo.dirtyMu.Lock()
	_, stillDirty := repo.dirtyData[largeInode.Object]
	repo.dirtyMu.Unlock()
	if !stillDirty {
		t.Fatal("a scoped fsync synced another file's payload; that is the full fence's job")
	}
	if err := repo.SyncMetadata(); err != nil {
		t.Fatal(err)
	}
	repo.dirtyMu.Lock()
	remaining := len(repo.dirtyData)
	repo.dirtyMu.Unlock()
	if remaining != 0 {
		t.Fatalf("the full fence left %d dirty payloads", remaining)
	}
}

// The fence that deletes garbage makes durable the WAL proving it unreachable,
// and not the payloads of files still live, which the full fence syncs.
func TestReclaimFenceLeavesLiveFilesDataToTheFullFence(t *testing.T) {
	manager := openTestManager(t)
	repo, err := manager.Create("reclaim-fence")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	live, err := repo.CreateRegular(MainBranch, RootInode, "live", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	write(t, repo, MainBranch, live, "not yet", 0)
	liveInode, err := repo.Inode(MainBranch, live)
	if err != nil {
		t.Fatal(err)
	}
	for iteration := 0; iteration < reclaimBatchObjects+100; iteration++ {
		ino, err := repo.CreateRegular(MainBranch, RootInode, "temporary", 0600, 0, 0)
		if err != nil {
			t.Fatalf("iteration %d create: %v", iteration, err)
		}
		if err := repo.Unlink(MainBranch, RootInode, "temporary"); err != nil {
			t.Fatalf("iteration %d unlink: %v", iteration, err)
		}
		if err := repo.ReapOrphan(MainBranch, ino); err != nil {
			t.Fatalf("iteration %d reap: %v", iteration, err)
		}
	}
	repo.dirtyMu.Lock()
	reclaiming := len(repo.pendingReclaims)
	_, stillDirty := repo.dirtyData[liveInode.Object]
	repo.dirtyMu.Unlock()
	if reclaiming >= reclaimBatchObjects {
		t.Fatalf("the automatic fence left %d objects to reclaim, want fewer than %d", reclaiming, reclaimBatchObjects)
	}
	if !stillDirty {
		t.Fatal("the fence that deleted garbage synced a live file's payload; that is the full fence's job")
	}
	if err := repo.SyncMetadata(); err != nil {
		t.Fatal(err)
	}
	repo.dirtyMu.Lock()
	remaining := len(repo.dirtyData)
	repo.dirtyMu.Unlock()
	if remaining != 0 {
		t.Fatalf("the full fence left %d dirty payloads", remaining)
	}
}

// A new file has nothing to sync but its name until something opens it to
// write: then, and not before, the next full fence syncs its payload.
func TestFenceSyncsANewFileOnceOpenedToWrite(t *testing.T) {
	root := t.TempDir()
	manager, err := OpenManager(root)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := manager.Create("new-files")
	if err != nil {
		t.Fatal(err)
	}
	dirty := func(object string) bool {
		repo.dirtyMu.Lock()
		defer repo.dirtyMu.Unlock()
		_, ok := repo.dirtyData[object]
		return ok
	}
	empty, err := repo.CreateRegularInode(MainBranch, RootInode, "empty", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if read, err := repo.Inode(MainBranch, empty.Number); err != nil || !reflect.DeepEqual(read, empty) {
		t.Fatalf("created %+v, read (%+v, %v)", empty, read, err)
	}
	written, err := repo.CreateRegularInode(MainBranch, RootInode, "written", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if dirty(empty.Object) || dirty(written.Object) {
		t.Fatal("a new empty file's payload is to be synced")
	}
	file, _, _, err := repo.OpenDataFile(MainBranch, written.Number, true)
	if err != nil {
		t.Fatal(err)
	}
	if !dirty(written.Object) {
		t.Fatal("a file opened to write is not to be synced")
	}
	if _, err := file.WriteAt([]byte("bytes"), 0); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := repo.SyncMetadata(); err != nil {
		t.Fatal(err)
	}
	if dirty(written.Object) {
		t.Fatal("the full fence left the written file's payload unsynced")
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	if repo, err = manager.Open("new-files"); err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	file, _, _, err = repo.OpenDataFile(MainBranch, empty.Number, false)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil || info.Size() != 0 {
		t.Fatalf("the empty file after a reopen: (%v, %v)", info, err)
	}
}

// What a scoped fsync made durable is what a reopen after a crash sees: the
// file's name, size and bytes, without any later fence having run.
func TestBranchSyncIsRecoverableWithoutAFullFence(t *testing.T) {
	root := t.TempDir()
	manager, err := OpenManager(root)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := manager.Create("scoped-recovery")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	ino, err := repo.CreateRegular(MainBranch, RootInode, "database", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	write(t, repo, MainBranch, ino, "journal page", 0)
	if err := repo.SyncData(MainBranch, ino, false); err != nil {
		t.Fatal(err)
	}
	if err := repo.SyncBranchMetadata(MainBranch); err != nil {
		t.Fatal(err)
	}
	// Close would take a full fence, so the "crash" is a copy of the root
	// taken while the repository is still open.
	crashed := t.TempDir()
	copyTree(t, root, crashed)
	recoveredManager, err := OpenManager(crashed)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := recoveredManager.Open("scoped-recovery")
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	got, err := recovered.Lookup(MainBranch, RootInode, "database")
	if err != nil {
		t.Fatalf("the fsynced file's name was not durable: %v", err)
	}
	if data := readAll(t, recovered, MainBranch, got); data != "journal page" {
		t.Fatalf("recovered %q after a scoped fsync, want %q", data, "journal page")
	}
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0700)
		case info.Mode().IsRegular():
			source, err := os.Open(path)
			if err != nil {
				return err
			}
			defer source.Close()
			destination, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
			if err != nil {
				return err
			}
			if _, err := io.Copy(destination, source); err != nil {
				destination.Close()
				return err
			}
			return destination.Close()
		default:
			// Sockets and other special files are runtime state, not data.
			return nil
		}
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A name marked dirty while a full fence syncs those it found, as a reader
// registering a deferred object marks its pending marker without the
// repository's lock, stays dirty for the next fence, which syncs it.
func TestFullSyncKeepsWhatWasMarkedDirtyMeanwhile(t *testing.T) {
	manager := openTestManager(t)
	repo, err := manager.Create("marked-meanwhile")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.mkdirAll("remote/aa/bb", 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join("remote", "aa", "bb", "marker")
	file, err := repo.openFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	afterNamesSynced = func(r *Repository) {
		if r == repo {
			r.markDirtyFile(marker)
		}
	}
	defer func() { afterNamesSynced = nil }()
	if err := repo.SyncMetadata(); err != nil {
		t.Fatal(err)
	}
	afterNamesSynced = nil
	repo.dirtyMu.Lock()
	_, dirty := repo.dirtyFiles[marker]
	repo.dirtyMu.Unlock()
	if !dirty {
		t.Fatal("a name marked dirty during a full fence was forgotten unsynced")
	}
	if err := repo.SyncMetadata(); err != nil {
		t.Fatal(err)
	}
	repo.dirtyMu.Lock()
	_, dirty = repo.dirtyFiles[marker]
	repo.dirtyMu.Unlock()
	if dirty {
		t.Fatal("the next full fence did not sync the name marked dirty")
	}
}
