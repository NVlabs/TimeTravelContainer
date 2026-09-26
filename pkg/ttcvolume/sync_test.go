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

package ttcvolume

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
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
