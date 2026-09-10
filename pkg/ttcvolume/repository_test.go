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
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
)

func openTestManager(t *testing.T) *Manager {
	t.Helper()
	manager, err := OpenManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func readAll(t *testing.T, repo *Repository, branch string, ino uint64) string {
	t.Helper()
	inode, err := repo.Inode(branch, ino)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, inode.Size)
	n, err := repo.ReadAt(branch, ino, data, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	return string(data[:n])
}

func write(t *testing.T, repo *Repository, branch string, ino uint64, value string, offset int64) bool {
	t.Helper()
	n, cloned, err := repo.WriteAt(branch, ino, []byte(value), offset)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(value) {
		t.Fatalf("WriteAt wrote %d bytes, want %d", n, len(value))
	}
	return cloned
}

func TestForkWritesDivergeAcrossGenerations(t *testing.T) {
	manager := openTestManager(t)
	repo, err := manager.Create("state")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	ino, err := repo.CreateRegular("main", rootInode, "value", 0644, 1000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if cloned := write(t, repo, "main", ino, "base", 0); cloned {
		t.Fatal("initial private write unexpectedly cloned")
	}
	beforeFork, err := repo.Inode("main", ino)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Fork("main", "child"); err != nil {
		t.Fatal(err)
	}
	if cloned := write(t, repo, "child", ino, "CH", 0); !cloned {
		t.Fatal("child's first post-fork write did not clone")
	}
	if got := readAll(t, repo, "main", ino); got != "base" {
		t.Fatalf("parent after child write=%q, want base", got)
	}
	if got := readAll(t, repo, "child", ino); got != "CHse" {
		t.Fatalf("child=%q, want CHse", got)
	}
	if cloned := write(t, repo, "main", ino, "PA", 0); !cloned {
		t.Fatal("parent's first post-fork write did not clone")
	}
	if cloned := write(t, repo, "main", ino, "!", 2); cloned {
		t.Fatal("second write in one generation cloned again")
	}
	if got := readAll(t, repo, "main", ino); got != "PA!e" {
		t.Fatalf("parent=%q, want PA!e", got)
	}
	if got := readAll(t, repo, "child", ino); got != "CHse" {
		t.Fatalf("child changed after parent write: %q", got)
	}
	parent, _ := repo.Inode("main", ino)
	child, _ := repo.Inode("child", ino)
	if parent.Object == child.Object || parent.Object == beforeFork.Object || child.Object == beforeFork.Object {
		t.Fatalf("objects did not split three ways: before=%q parent=%q child=%q", beforeFork.Object, parent.Object, child.Object)
	}
}

func TestJournalRecoversMetadata(t *testing.T) {
	manager := openTestManager(t)
	repo, err := manager.Create("packed-nodes")
	if err != nil {
		t.Fatal(err)
	}
	ino, err := repo.CreateRegular(MainBranch, RootInode, "value", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	write(t, repo, MainBranch, ino, "packed", 0)
	if err := repo.SyncMetadata(); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	repo, err = manager.Open("packed-nodes")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	gotIno, err := repo.Lookup(MainBranch, RootInode, "value")
	if err != nil {
		t.Fatal(err)
	}
	if gotIno != ino {
		t.Fatalf("recovered inode = %d, want %d", gotIno, ino)
	}
	if got := readAll(t, repo, MainBranch, ino); got != "packed" {
		t.Fatalf("recovered data = %q, want packed", got)
	}
}

func TestJournalDiscardsTornTailBeforeNextTransaction(t *testing.T) {
	manager := openTestManager(t)
	repo, err := manager.Create("torn-journal")
	if err != nil {
		t.Fatal(err)
	}
	branch, err := repo.Branch(MainBranch)
	if err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(repo.path, "layers", branch.Root, "journal")
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(journal, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("TTCJ\x80")); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	repo, err = manager.Open("torn-journal")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateRegular(MainBranch, RootInode, "after-crash", 0644, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	repo, err = manager.Open("torn-journal")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if _, err := repo.Lookup(MainBranch, RootInode, "after-crash"); err != nil {
		t.Fatalf("transaction after torn tail was not replayed: %v", err)
	}
}

func TestRegularFileRepositoryRootForks(t *testing.T) {
	manager := openTestManager(t)
	repo, err := manager.Create("regular-root")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.InitializeRootRegular(0640, 1000, 1001); err != nil {
		t.Fatal(err)
	}
	root, err := repo.Inode(MainBranch, RootInode)
	if err != nil {
		t.Fatal(err)
	}
	if root.Type != "regular" || root.Mode != 0640 || root.UID != 1000 || root.GID != 1001 {
		t.Fatalf("regular root=%+v", root)
	}
	if cloned := write(t, repo, MainBranch, RootInode, "base", 0); cloned {
		t.Fatal("initial root write unexpectedly cloned")
	}
	if err := repo.Fork(MainBranch, "child"); err != nil {
		t.Fatal(err)
	}
	if cloned := write(t, repo, "child", RootInode, "CH", 0); !cloned {
		t.Fatal("child regular-root write did not clone")
	}
	if got := readAll(t, repo, MainBranch, RootInode); got != "base" {
		t.Fatalf("parent root=%q", got)
	}
	if got := readAll(t, repo, "child", RootInode); got != "CHse" {
		t.Fatalf("child root=%q", got)
	}
}

func TestActualReflinkPathSucceedsOrFailsClosed(t *testing.T) {
	manager := openTestManager(t)
	repo, err := manager.Create("actual-reflink")
	if err != nil {
		t.Fatal(err)
	}
	ino, err := repo.CreateRegular(MainBranch, RootInode, "value", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	write(t, repo, MainBranch, ino, "base", 0)
	if err := repo.Fork(MainBranch, "child"); err != nil {
		t.Fatal(err)
	}

	// Exercise ioctl(FICLONE) even when OpenManager automatically selected the
	// full-copy fallback for this test filesystem.
	repo.copyMode = CopyModeReflink
	n, cloned, writeErr := repo.WriteAt("child", ino, []byte("CH"), 0)
	switch manager.Capability().Mode {
	case CopyModeReflink:
		if writeErr != nil {
			t.Fatalf("FICLONE-capable filesystem rejected branch write: %v", writeErr)
		}
		if n != 2 || !cloned {
			t.Fatalf("reflink branch write = n %d, cloned %t; want 2, true", n, cloned)
		}
		if got := readAll(t, repo, MainBranch, ino); got != "base" {
			t.Fatalf("reflink child write changed parent to %q", got)
		}
		if got := readAll(t, repo, "child", ino); got != "CHse" {
			t.Fatalf("reflink child data = %q, want CHse", got)
		}
	case CopyModeFull:
		if writeErr == nil {
			t.Fatalf("unsupported FICLONE silently fell back: n %d, cloned %t", n, cloned)
		}
		if got := readAll(t, repo, MainBranch, ino); got != "base" {
			t.Fatalf("failed reflink changed parent to %q", got)
		}
		if got := readAll(t, repo, "child", ino); got != "base" {
			t.Fatalf("failed reflink changed child to %q", got)
		}
	default:
		t.Fatalf("unknown manager copy mode %q", manager.Capability().Mode)
	}
}

func TestNestedForksAndWritesBetweenForks(t *testing.T) {
	repo, err := openTestManager(t).Create("nested")
	if err != nil {
		t.Fatal(err)
	}
	ino, err := repo.CreateRegular("main", rootInode, "state", 0600, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	write(t, repo, "main", ino, "0000", 0)
	if err := repo.Fork("main", "a"); err != nil {
		t.Fatal(err)
	}
	write(t, repo, "main", ino, "M", 0)
	write(t, repo, "a", ino, "A", 1)
	if err := repo.Fork("a", "b"); err != nil {
		t.Fatal(err)
	}
	write(t, repo, "a", ino, "X", 2)
	write(t, repo, "b", ino, "B", 3)
	if err := repo.Fork("main", "c"); err != nil {
		t.Fatal(err)
	}
	write(t, repo, "main", ino, "N", 1)
	write(t, repo, "c", ino, "C", 2)

	for branch, want := range map[string]string{
		"main": "MN00",
		"a":    "0AX0",
		"b":    "0A0B",
		"c":    "M0C0",
	} {
		if got := readAll(t, repo, branch, ino); got != want {
			t.Errorf("%s=%q, want %q", branch, got, want)
		}
	}
}

func TestHardLinksTrackBranchLocalInodeObject(t *testing.T) {
	repo, err := openTestManager(t).Create("links")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	ino, err := repo.CreateRegular("main", rootInode, "first", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	write(t, repo, "main", ino, "shared", 0)
	if err := repo.Link("main", rootInode, "second", ino); err != nil {
		t.Fatal(err)
	}
	if err := repo.Fork("main", "child"); err != nil {
		t.Fatal(err)
	}
	second, err := repo.Lookup("child", rootInode, "second")
	if err != nil {
		t.Fatal(err)
	}
	if second != ino {
		t.Fatalf("hard link inode=%d, want %d", second, ino)
	}
	write(t, repo, "child", second, "C", 0)
	if got := readAll(t, repo, "child", ino); got != "Chared" {
		t.Fatalf("child hard-link view=%q", got)
	}
	if got := readAll(t, repo, "main", ino); got != "shared" {
		t.Fatalf("parent changed through child hard link: %q", got)
	}
}

func TestRepositoryReopensWithoutImport(t *testing.T) {
	root := t.TempDir()
	manager, err := OpenManager(root)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := manager.Create("durable")
	if err != nil {
		t.Fatal(err)
	}
	ino, err := repo.CreateRegular("main", rootInode, "value", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	write(t, repo, "main", ino, "persisted", 0)
	id := repo.Info().ID

	manager, err = OpenManager(root)
	if err != nil {
		t.Fatal(err)
	}
	repo, err = manager.Open("durable")
	if err != nil {
		t.Fatal(err)
	}
	if repo.Info().ID != id {
		t.Fatalf("reopened ID=%q, want %q", repo.Info().ID, id)
	}
	if got := readAll(t, repo, "main", ino); got != "persisted" {
		t.Fatalf("reopened data=%q", got)
	}
	if matches, err := filepath.Glob(filepath.Join(root, "filesystems", "volumes", id, "*.tar")); err != nil || len(matches) != 0 {
		t.Fatalf("unexpected tar persistence: matches=%v err=%v", matches, err)
	}
}

func TestRepositoryOperatesThroughDonatedDirectoryFD(t *testing.T) {
	manager := openTestManager(t)
	repo, err := manager.Create("donated")
	if err != nil {
		t.Fatal(err)
	}
	ino, err := repo.CreateRegular(MainBranch, RootInode, "value", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	write(t, repo, MainBranch, ino, "before", 0)
	dir, err := repo.Directory()
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	donated, err := OpenRepositoryFD(dir, manager.Capability().Mode)
	if err != nil {
		t.Fatal(err)
	}
	defer donated.Close()

	// Remove all pathname knowledge from this instance. Operations must remain
	// relative to the donated repository descriptor.
	donated.path = ""
	if err := donated.Fork(MainBranch, "child"); err != nil {
		t.Fatal(err)
	}
	write(t, donated, "child", ino, "CH", 0)
	if got := readAll(t, donated, MainBranch, ino); got != "before" {
		t.Fatalf("donated-FD parent=%q", got)
	}
	if got := readAll(t, donated, "child", ino); got != "CHfore" {
		t.Fatalf("donated-FD child=%q", got)
	}
}

func TestForkTransactionAbortRestoresSource(t *testing.T) {
	manager, err := OpenManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo, err := manager.Create("transaction")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	ino, err := repo.CreateRegular(MainBranch, RootInode, "data", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.WriteAt(MainBranch, ino, []byte("before"), 0); err != nil {
		t.Fatal(err)
	}
	before, err := repo.Branch(MainBranch)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := repo.BeginFork(MainBranch, "aborted")
	if err != nil {
		t.Fatal(err)
	}
	advanced, err := repo.Branch(MainBranch)
	if err != nil {
		t.Fatal(err)
	}
	if advanced.Generation != before.Generation+1 {
		t.Fatalf("source generation after BeginFork=%d, want %d", advanced.Generation, before.Generation+1)
	}
	if err := tx.Abort(); err != nil {
		t.Fatal(err)
	}
	after, err := repo.Branch(MainBranch)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("source after Abort=%+v, want %+v", after, before)
	}
	if _, err := repo.Branch("aborted"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("aborted child lookup error=%v, want not exist", err)
	}
	if _, _, err := repo.WriteAt(MainBranch, ino, []byte("after!"), 0); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, repo, MainBranch, ino); got != "after!" {
		t.Fatalf("source contents after abort=%q, want after!", got)
	}
}

func TestForkTransactionCommitRetainsBranches(t *testing.T) {
	manager, err := OpenManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo, err := manager.Create("commit")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	tx, err := repo.BeginFork(MainBranch, "child")
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Branch(MainBranch); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Branch("child"); err != nil {
		t.Fatal(err)
	}
	// Commit and Abort are idempotent after completion.
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Abort(); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Branch("child"); err != nil {
		t.Fatal(err)
	}
}

func TestPersistentNamespaceOperationsAcrossForks(t *testing.T) {
	manager, err := OpenManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo, err := manager.Create("namespace")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	dir, err := repo.CreateDirectory(MainBranch, RootInode, "dir", 0750, 12, 34)
	if err != nil {
		t.Fatal(err)
	}
	link, err := repo.CreateSymlink(MainBranch, dir, "link", "../target", 12, 34)
	if err != nil {
		t.Fatal(err)
	}
	file, err := repo.CreateRegular(MainBranch, dir, "file", 0640, 12, 34)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Link(MainBranch, RootInode, "hard", file); err != nil {
		t.Fatal(err)
	}
	if err := repo.Fork(MainBranch, "child"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Rename("child", dir, "file", dir, "renamed", false); err != nil {
		t.Fatal(err)
	}
	if err := repo.Unlink("child", RootInode, "hard"); err != nil {
		t.Fatal(err)
	}
	if err := repo.RemoveDirectory("child", RootInode, "dir"); !errors.Is(err, syscall.ENOTEMPTY) {
		t.Fatalf("removing non-empty directory error=%v, want ENOTEMPTY", err)
	}
	if _, err := repo.Lookup(MainBranch, dir, "file"); err != nil {
		t.Fatalf("parent file disappeared: %v", err)
	}
	if _, err := repo.Lookup(MainBranch, dir, "renamed"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("parent unexpectedly sees child rename: %v", err)
	}
	if got, err := repo.Inode(MainBranch, link); err != nil {
		t.Fatal(err)
	} else if got.Type != "symlink" || got.Target != "../target" {
		t.Fatalf("symlink inode=%+v", got)
	}
	childFile, err := repo.Inode("child", file)
	if err != nil {
		t.Fatal(err)
	}
	childFile.Mode = 0600
	childFile.UID = 99
	childFile.MtimeNsec = 123
	if err := repo.UpdateMetadata("child", childFile); err != nil {
		t.Fatal(err)
	}
	if got, err := repo.Inode("child", file); err != nil {
		t.Fatal(err)
	} else if got.Mode != 0600 || got.UID != 99 || got.MtimeNsec != 123 {
		t.Fatalf("updated child inode=%+v", got)
	}
	if got, err := repo.Inode(MainBranch, file); err != nil {
		t.Fatal(err)
	} else if got.Mode != 0640 || got.UID != 12 {
		t.Fatalf("parent metadata changed=%+v", got)
	}
}

func TestMutationAppendsOneBoundedJournalTransactionAndForkCopiesNoMetadata(t *testing.T) {
	repo, err := openTestManager(t).Create("tree")
	if err != nil {
		t.Fatal(err)
	}
	journalSize := func(layer string) int64 {
		t.Helper()
		info, err := os.Stat(filepath.Join(repo.path, "layers", layer, "journal"))
		if err != nil {
			t.Fatal(err)
		}
		return info.Size()
	}
	before, err := repo.Branch(MainBranch)
	if err != nil {
		t.Fatal(err)
	}
	sealedSize := journalSize(before.Root)
	if err := repo.Fork("main", "child"); err != nil {
		t.Fatal(err)
	}
	if afterFork := journalSize(before.Root); afterFork != sealedSize {
		t.Fatalf("fork changed sealed layer by %d bytes, want 0", afterFork-sealedSize)
	}
	child, err := repo.Branch("child")
	if err != nil {
		t.Fatal(err)
	}
	beforeMutation := journalSize(child.Root)
	if _, err := repo.CreateRegular("child", rootInode, "x", 0644, 0, 0); err != nil {
		t.Fatal(err)
	}
	if delta := journalSize(child.Root) - beforeMutation; delta <= 0 || delta > 4096 {
		t.Fatalf("one mutation appended %d bytes, want one bounded transaction", delta)
	}
	journal, err := os.Open(filepath.Join(repo.path, "layers", child.Root, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	transaction, next, err := readMetadataTransaction(journal, beforeMutation)
	journal.Close()
	if err != nil {
		t.Fatal(err)
	}
	if next != journalSize(child.Root) || len(transaction.Ops) != 2 {
		t.Fatalf("create transaction ended at %d with %d ops, want end %d and 2 ops", next, len(transaction.Ops), journalSize(child.Root))
	}
}

func TestMetadataUsesJournalWithoutPerEntryFiles(t *testing.T) {
	manager := openTestManager(t)
	repo, err := manager.Create("journal-only")
	if err != nil {
		t.Fatal(err)
	}
	_, err = repo.CreateRegular(MainBranch, RootInode, "a-very-long-guest-name", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	branch, err := repo.Branch(MainBranch)
	if err != nil {
		t.Fatal(err)
	}
	root := repo.path
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "layers", branch.Root))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	if got, want := strings.Join(names, ","), "journal,layer.json"; got != want {
		t.Fatalf("metadata layer entries=%q, want %q", got, want)
	}
}

func TestAllocateAndXattrsAreBranchLocalAndDurable(t *testing.T) {
	root := t.TempDir()
	manager, err := OpenManager(root)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := manager.Create("metadata")
	if err != nil {
		t.Fatal(err)
	}
	ino, err := repo.CreateRegular(MainBranch, RootInode, "file", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := repo.Inode(MainBranch, ino)
	if err != nil {
		t.Fatal(err)
	}
	meta.Xattrs = map[string]string{"user.test": "parent"}
	if err := repo.UpdateMetadata(MainBranch, meta); err != nil {
		t.Fatal(err)
	}
	if err := repo.Fork(MainBranch, "child"); err != nil {
		t.Fatal(err)
	}
	if cloned, err := repo.Allocate("child", ino, 0, 8192); err != nil {
		t.Fatal(err)
	} else if !cloned {
		t.Fatal("first child allocation did not privatize the file object")
	}
	child, err := repo.Inode("child", ino)
	if err != nil {
		t.Fatal(err)
	}
	child.Xattrs["user.test"] = "child"
	if err := repo.UpdateMetadata("child", child); err != nil {
		t.Fatal(err)
	}
	if got, err := repo.Inode(MainBranch, ino); err != nil {
		t.Fatal(err)
	} else if got.Size != 0 || got.Xattrs["user.test"] != "parent" {
		t.Fatalf("parent metadata changed after child allocation: %+v", got)
	}
	repo.Close()

	manager, err = OpenManager(root)
	if err != nil {
		t.Fatal(err)
	}
	repo, err = manager.Open("metadata")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := repo.Inode("child", ino); err != nil {
		t.Fatal(err)
	} else if got.Size != 8192 || got.Xattrs["user.test"] != "child" {
		t.Fatalf("reopened child metadata=%+v", got)
	}
}

func TestPrivateRepositoryIsDurableAndNotCatalogued(t *testing.T) {
	root := t.TempDir()
	manager, err := OpenManager(root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "containers", "family", "rootfs-upper")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	repo, err := manager.CreatePrivate(path, "rootfs")
	if err != nil {
		t.Fatal(err)
	}
	id := repo.Info().ID
	if _, err := repo.CreateSpecial(MainBranch, RootInode, ".wh.deleted", "char", 0, 0, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	volumes, err := manager.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(volumes) != 0 {
		t.Fatalf("private repository appeared in named volume catalog: %+v", volumes)
	}
	repo, err = manager.OpenPrivate(path)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if repo.Info().ID != id {
		t.Fatalf("reopened ID = %q, want %q", repo.Info().ID, id)
	}
	ino, err := repo.Lookup(MainBranch, RootInode, ".wh.deleted")
	if err != nil {
		t.Fatal(err)
	}
	meta, err := repo.Inode(MainBranch, ino)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Type != "char" || meta.DevMajor != 0 || meta.DevMinor != 0 {
		t.Fatalf("whiteout metadata = %+v", meta)
	}
	if _, err := manager.CreatePrivate(filepath.Join(root, "..", "escape"), "bad"); err == nil {
		t.Fatal("CreatePrivate accepted path outside data root")
	}
}

func TestScopedBranchID(t *testing.T) {
	if got := ScopedBranchID("", "child"); got != "child" {
		t.Fatalf("unscoped branch = %q, want child", got)
	}
	a := ScopedBranchID("family-a", "child")
	if a != ScopedBranchID("family-a", "child") {
		t.Fatal("scoped branch ID is not deterministic")
	}
	if a == ScopedBranchID("family-b", "child") || a == "child" {
		t.Fatalf("scoped branch IDs collide: %q", a)
	}
}

func TestSnapshotRoundTripIsBranchScoped(t *testing.T) {
	manager := openTestManager(t)
	source, err := manager.Create("source")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	ino, err := source.CreateRegular(MainBranch, RootInode, "value", 0640, 12, 34)
	if err != nil {
		t.Fatal(err)
	}
	write(t, source, MainBranch, ino, "snapshot", 0)
	if err := source.Link(MainBranch, RootInode, "hard", ino); err != nil {
		t.Fatal(err)
	}
	if err := source.Fork(MainBranch, "checkpoint"); err != nil {
		t.Fatal(err)
	}
	write(t, source, MainBranch, ino, "changed!", 0)

	snapshotDir := filepath.Join(t.TempDir(), "snapshot")
	snapshot, err := source.ExportSnapshot("checkpoint", snapshotDir)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.RepositoryID != source.Info().ID || snapshot.BranchID != "checkpoint" {
		t.Fatalf("snapshot metadata = %+v", snapshot)
	}
	importPath := filepath.Join(manager.root, "restored")
	restored, imported, err := manager.ImportPrivateSnapshot(importPath, "restored", snapshotDir)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if imported.RepositoryID != source.Info().ID {
		t.Fatalf("imported source ID = %q, want %q", imported.RepositoryID, source.Info().ID)
	}
	gotIno, err := restored.Lookup(MainBranch, RootInode, "value")
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, restored, MainBranch, gotIno); got != "snapshot" {
		t.Fatalf("restored data = %q, want snapshot", got)
	}
	hard, err := restored.Lookup(MainBranch, RootInode, "hard")
	if err != nil {
		t.Fatal(err)
	}
	if hard != gotIno {
		t.Fatalf("restored hard link inode = %d, want %d", hard, gotIno)
	}
	meta, err := restored.Inode(MainBranch, gotIno)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Mode != 0640 || meta.UID != 12 || meta.GID != 34 || meta.Nlink != 2 {
		t.Fatalf("restored inode metadata = %+v", meta)
	}
	if _, err := restored.Branch("checkpoint"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("import retained source branch: %v", err)
	}
}

func TestUnlinkedInodeRemainsCheckpointable(t *testing.T) {
	manager := openTestManager(t)
	repository, err := manager.Create("orphan")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	ino, err := repository.CreateRegular(MainBranch, RootInode, "open", 0600, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	write(t, repository, MainBranch, ino, "still open", 0)
	if err := repository.Unlink(MainBranch, RootInode, "open"); err != nil {
		t.Fatal(err)
	}
	orphan, err := repository.Inode(MainBranch, ino)
	if err != nil {
		t.Fatalf("unlinked inode disappeared: %v", err)
	}
	if orphan.Nlink != 0 {
		t.Fatalf("unlinked inode nlink=%d, want 0", orphan.Nlink)
	}
	snapshotPath := filepath.Join(t.TempDir(), "snapshot")
	if _, err := repository.ExportSnapshot(MainBranch, snapshotPath); err != nil {
		t.Fatal(err)
	}
	imported, _, err := manager.ImportPrivateSnapshot(filepath.Join(manager.root, "imported"), "imported", snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	defer imported.Close()
	restored, err := imported.Inode(MainBranch, ino)
	if err != nil {
		t.Fatalf("restored unlinked inode is unavailable: %v", err)
	}
	if restored.Nlink != 0 || restored.Object == "" {
		t.Fatalf("restored unlinked inode=%+v", restored)
	}
	data, _, _, err := imported.OpenDataFile(MainBranch, ino, false)
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	got, err := io.ReadAll(data)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "still open" {
		t.Fatalf("restored unlinked data=%q", got)
	}
	if err := imported.ReapOrphan(MainBranch, ino); err != nil {
		t.Fatal(err)
	}
	if _, err := imported.Inode(MainBranch, ino); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reaped inode lookup=%v, want not exist", err)
	}
	if err := imported.ReapOrphan(MainBranch, RootInode); err == nil {
		t.Fatal("ReapOrphan accepted root")
	}
}

func TestRemoveBranchKeepsMain(t *testing.T) {
	manager := openTestManager(t)
	repository, err := manager.Create("data")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if err := repository.Fork(MainBranch, "temporary"); err != nil {
		t.Fatal(err)
	}
	if err := repository.RemoveBranch("temporary"); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Branch("temporary"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed branch lookup = %v, want not exist", err)
	}
	if _, err := repository.Branch(MainBranch); err != nil {
		t.Fatalf("main branch was removed: %v", err)
	}
	if err := repository.RemoveBranch(MainBranch); err == nil {
		t.Fatal("RemoveBranch accepted main")
	}
}

func TestRemoveBranchAllowsMainWithSurvivingPeer(t *testing.T) {
	manager := openTestManager(t)
	repository, err := manager.Create("main-removal")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if err := repository.Fork(MainBranch, "peer"); err != nil {
		t.Fatal(err)
	}
	if err := repository.RemoveBranch(MainBranch); err != nil {
		t.Fatalf("RemoveBranch(main) with peer: %v", err)
	}
	if _, err := repository.Branch(MainBranch); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed main lookup=%v, want not exist", err)
	}
	if _, err := repository.Branch("peer"); err != nil {
		t.Fatalf("surviving peer lookup: %v", err)
	}
}

func TestGarbageCollectBoundsRepeatedBranchChurn(t *testing.T) {
	manager := openTestManager(t)
	repository, err := manager.Create("branch-churn")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	ino, err := repository.CreateRegular(MainBranch, RootInode, "value", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	write(t, repository, MainBranch, ino, "main", 0)

	for round := 0; round < metadataCompactionLayerThreshold*2+3; round++ {
		childID := fmt.Sprintf("temporary-%04d", round)
		if err := repository.Fork(MainBranch, childID); err != nil {
			t.Fatalf("round %d Fork: %v", round, err)
		}
		if _, _, err := repository.WriteAt(childID, ino, []byte("child"), 0); err != nil {
			t.Fatalf("round %d child WriteAt: %v", round, err)
		}
		child, err := repository.Inode(childID, ino)
		if err != nil {
			t.Fatal(err)
		}
		childObject := filepath.Join(repository.path, repository.objectPath(child.Object))
		repository.ForgetBranch(childID)
		collector, err := manager.Open("branch-churn")
		if err != nil {
			t.Fatalf("round %d opening collector: %v", round, err)
		}
		if err := collector.RemoveBranch(childID); err != nil {
			t.Fatalf("round %d RemoveBranch: %v", round, err)
		}
		if err := collector.GarbageCollect(); err != nil {
			t.Fatalf("round %d GarbageCollect: %v", round, err)
		}
		if err := collector.Close(); err != nil {
			t.Fatalf("round %d closing collector: %v", round, err)
		}
		if _, err := os.Stat(childObject); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("round %d child object survived collection: %v", round, err)
		}
		if got := readAll(t, repository, MainBranch, ino); got != "main" {
			t.Fatalf("round %d main data=%q", round, got)
		}
	}

	branches, err := os.ReadDir(filepath.Join(repository.path, "branches"))
	if err != nil {
		t.Fatal(err)
	}
	if len(branches) != 1 || branches[0].Name() != MainBranch+".json" {
		t.Fatalf("branch pointers after churn=%v", branches)
	}
	layers, err := os.ReadDir(filepath.Join(repository.path, "layers"))
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) > metadataCompactionLayerThreshold {
		t.Fatalf("metadata layers grew with deleted history: got %d, threshold %d", len(layers), metadataCompactionLayerThreshold)
	}
	repository.metadataMu.Lock()
	metadataEntries := len(repository.metadata)
	repository.metadataMu.Unlock()
	if metadataEntries > 1 {
		t.Fatalf("process-local metadata retained %d historical branches, want at most main", metadataEntries)
	}
	repository.branchMu.Lock()
	branchCacheEntries := len(repository.branchCache)
	repository.branchMu.Unlock()
	if branchCacheEntries > 1 {
		t.Fatalf("process-local branch cache retained %d historical branches, want at most main", branchCacheEntries)
	}
	repository.journalMu.Lock()
	journalEntries := len(repository.journals)
	repository.journalMu.Unlock()
	if journalEntries > metadataCompactionLayerThreshold {
		t.Fatalf("process-local journals retained %d historical layers", journalEntries)
	}
}

func TestImportSnapshotRejectsSymlinkMetadata(t *testing.T) {
	manager := openTestManager(t)
	source, err := manager.Create("source")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	snapshotPath := filepath.Join(t.TempDir(), "snapshot")
	if _, err := source.ExportSnapshot(MainBranch, snapshotPath); err != nil {
		t.Fatal(err)
	}
	metadata := filepath.Join(snapshotPath, "snapshot.json")
	outside := filepath.Join(t.TempDir(), "outside.json")
	data, err := os.ReadFile(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(metadata); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, metadata); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.ImportPrivateSnapshot(filepath.Join(manager.root, "imported"), "imported", snapshotPath); err == nil {
		t.Fatal("ImportPrivateSnapshot accepted symlinked metadata")
	}
}

func TestImportSnapshotRejectsSymlinkNamespaceMetadata(t *testing.T) {
	manager := openTestManager(t)
	source, err := manager.Create("source")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	snapshotPath := filepath.Join(t.TempDir(), "snapshot")
	if _, err := source.ExportSnapshot(MainBranch, snapshotPath); err != nil {
		t.Fatal(err)
	}
	node := filepath.Join(snapshotPath, "metadata.json")
	outside := filepath.Join(t.TempDir(), "outside-node")
	data, err := os.ReadFile(node)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(node); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, node); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.ImportPrivateSnapshot(filepath.Join(manager.root, "imported"), "imported", snapshotPath); err == nil {
		t.Fatal("ImportPrivateSnapshot accepted a symlinked node")
	}
}

func TestListDirectoryItemsAndBranchUsage(t *testing.T) {
	manager := openTestManager(t)
	repository, err := manager.Create("directory-items")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.CreateDirectory(MainBranch, RootInode, "z-dir", 0755, 0, 0); err != nil {
		t.Fatal(err)
	}
	ino, err := repository.CreateRegular(MainBranch, RootInode, "a-file", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Truncate(MainBranch, ino, 5000); err != nil {
		t.Fatal(err)
	}
	items, err := repository.ListDirectoryItems(MainBranch, RootInode)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Entry.Name != "a-file" || items[0].Inode.Type != "regular" ||
		items[1].Entry.Name != "z-dir" || items[1].Inode.Type != "directory" {
		t.Fatalf("directory items=%+v", items)
	}
	inodes, bytes, err := repository.BranchUsage(MainBranch)
	if err != nil {
		t.Fatal(err)
	}
	wantBytes := uint64(2 * os.Getpagesize())
	if inodes != 3 || bytes != wantBytes {
		t.Fatalf("BranchUsage=(%d, %d), want (3, %d)", inodes, bytes, wantBytes)
	}
}

func TestBranchSetUsageClassifiesFileCoW(t *testing.T) {
	manager := openTestManager(t)
	repository, err := manager.Create("usage-set")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	shared, err := repository.CreateRegular(MainBranch, RootInode, "shared", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := repository.CreateRegular(MainBranch, RootInode, "changed", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, inode := range []uint64{shared, changed} {
		if _, _, err := repository.WriteAt(MainBranch, inode, []byte("base"), 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.Fork(MainBranch, "child"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.WriteAt("child", changed, []byte("child"), 0); err != nil {
		t.Fatal(err)
	}
	usage, err := repository.BranchSetUsage([]string{"child", MainBranch})
	if err != nil {
		t.Fatal(err)
	}
	page := uint64(os.Getpagesize())
	if usage.UniqueBytes != 3*page || len(usage.Branches) != 2 {
		t.Fatalf("usage=%+v, want three unique pages and two branches", usage)
	}
	for _, branch := range usage.Branches {
		if branch.PrivateBytes != page || branch.SharedBytes != page || branch.TotalBytes != 2*page {
			t.Fatalf("branch usage=%+v, want one private and one shared page", branch)
		}
	}
}

// A whiteout is a deletion, and diff says so.
//
// Overlayfs hides a name by putting a 0:0 character device where it was. The
// entry is present, so a diff that only asks "is this path in the layer" sees
// something added or changed and never a deletion — which is how a session that
// had lost its whole /tmp was reported as having a modified character device
// there, and how the person reading it went looking somewhere else.
func TestBranchDiffReadsAWhiteoutAsTheDeletionItIs(t *testing.T) {
	manager := openTestManager(t)
	repository, err := manager.Create("diff-whiteout")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	// A directory the branch will hide, and a file beside it for contrast.
	if _, err := repository.CreateDirectory(MainBranch, RootInode, "gone", 0755, 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CreateRegular(MainBranch, RootInode, "stays", 0644, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := repository.Fork(MainBranch, "child"); err != nil {
		t.Fatal(err)
	}
	if err := repository.RemoveDirectory("child", RootInode, "gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CreateSpecial("child", RootInode, "gone", "char", 0, 0, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	// And one that hides something the branch never had an entry for, which is
	// what deleting a directory the image brought looks like.
	if _, err := repository.CreateSpecial("child", RootInode, "fromtheimage", "char", 0, 0, 0, 0, 0); err != nil {
		t.Fatal(err)
	}

	changes, err := repository.BranchDiff("child")
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]PathChange{}
	for _, change := range changes {
		byPath[change.Path] = change
	}
	if got := byPath["/gone"]; got.Kind != "delete" {
		t.Fatalf("a hidden directory is reported as %+v, not a deletion", got)
	}
	if got := byPath["/gone"]; got.Type != "directory" {
		t.Fatalf("the deletion names %q as what went away, not the directory it was", got.Type)
	}
	// Nothing underneath says what it was, so nothing is claimed about it.
	if got := byPath["/fromtheimage"]; got.Kind != "delete" {
		t.Fatalf("hiding a path the branch never held is reported as %+v", got)
	}
	if got := byPath["/fromtheimage"]; got.Type != "" {
		t.Fatalf("a type of %q was invented for a path nothing under the branch describes", got.Type)
	}
	if _, ok := byPath["/stays"]; ok {
		t.Fatalf("an untouched file appears in the diff: %+v", byPath["/stays"])
	}
}

// A name that comes back is an addition, not a modification of the whiteout.
func TestBranchDiffReadsAReplacedWhiteoutAsThePathReturning(t *testing.T) {
	manager := openTestManager(t)
	repository, err := manager.Create("diff-returns")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.CreateSpecial(MainBranch, RootInode, "back", "char", 0, 0, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := repository.Fork(MainBranch, "child"); err != nil {
		t.Fatal(err)
	}
	if err := repository.Unlink("child", RootInode, "back"); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CreateRegular("child", RootInode, "back", 0644, 0, 0); err != nil {
		t.Fatal(err)
	}
	changes, err := repository.BranchDiff("child")
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range changes {
		if change.Path != "/back" {
			continue
		}
		if change.Kind != "add" || change.Type != "regular" {
			t.Fatalf("a path that came back is reported as %+v", change)
		}
		return
	}
	t.Fatal("the path coming back is not in the diff at all")
}

func TestBranchDiffUsesWritableLayerMetadata(t *testing.T) {
	manager := openTestManager(t)
	repository, err := manager.Create("diff")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	keep, err := repository.CreateRegular(MainBranch, RootInode, "keep", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.WriteAt(MainBranch, keep, []byte("base"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CreateRegular(MainBranch, RootInode, "remove", 0644, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := repository.Fork(MainBranch, "child"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.WriteAt("child", keep, []byte("child"), 0); err != nil {
		t.Fatal(err)
	}
	if err := repository.Unlink("child", RootInode, "remove"); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CreateDirectory("child", RootInode, "added", 0755, 0, 0); err != nil {
		t.Fatal(err)
	}
	changes, err := repository.BranchDiff("child")
	if err != nil {
		t.Fatal(err)
	}
	want := []PathChange{
		{Path: "/", Kind: "modify", Type: "directory"},
		{Path: "/added", Kind: "add", Type: "directory"},
		{Path: "/keep", Kind: "modify", Type: "regular"},
		{Path: "/remove", Kind: "delete", Type: "regular"},
	}
	if !reflect.DeepEqual(changes, want) {
		t.Fatalf("BranchDiff=%+v, want %+v", changes, want)
	}
}

func TestWriteLayerTarPreservesVirtualMetadata(t *testing.T) {
	manager := openTestManager(t)
	repository, err := manager.Create("layer")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	ino, err := repository.CreateRegular(MainBranch, RootInode, "value", 0640, 12, 34)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.WriteAt(MainBranch, ino, []byte("payload"), 0); err != nil {
		t.Fatal(err)
	}
	meta, err := repository.Inode(MainBranch, ino)
	if err != nil {
		t.Fatal(err)
	}
	meta.Xattrs = map[string]string{"user.test": "present"}
	if err := repository.UpdateMetadata(MainBranch, meta); err != nil {
		t.Fatal(err)
	}
	if err := repository.Link(MainBranch, RootInode, "hard", ino); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CreateSpecial(MainBranch, RootInode, "removed", "char", 0, 0, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	nested, err := repository.CreateDirectory(MainBranch, RootInode, "nested", 0755, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CreateSpecial(MainBranch, nested, "removed", "char", 0, 0, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	opaque, err := repository.CreateDirectory(MainBranch, RootInode, "opaque", 0755, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	opaqueMeta, err := repository.Inode(MainBranch, opaque)
	if err != nil {
		t.Fatal(err)
	}
	opaqueMeta.Xattrs = map[string]string{
		"trusted.overlay.opaque": "y",
		"user.test":              "present",
	}
	if err := repository.UpdateMetadata(MainBranch, opaqueMeta); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := repository.WriteLayerTar(MainBranch, &output); err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(&output)
	headers := make(map[string]*tar.Header)
	bodies := make(map[string]string)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		copy := *header
		headers[strings.TrimSuffix(header.Name, "/")] = &copy
		if header.Typeflag == tar.TypeReg && header.Size != 0 {
			data, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			bodies[header.Name] = string(data)
		}
	}
	value := headers["value"]
	hard := headers["hard"]
	if value == nil || hard == nil || value.Uid != 12 || value.Gid != 34 || value.Mode != 0640 {
		t.Fatalf("layer headers value=%+v hard=%+v", value, hard)
	}
	regularName := "value"
	link := hard
	if value.Typeflag == tar.TypeLink {
		regularName = "hard"
		link = value
	}
	if bodies[regularName] != "payload" || link.Typeflag != tar.TypeLink || link.Linkname != regularName {
		t.Fatalf("regular=%q bodies=%v link=%+v", regularName, bodies, link)
	}
	if whiteout := headers[".wh.removed"]; whiteout == nil || whiteout.Typeflag != tar.TypeReg || whiteout.Size != 0 {
		t.Fatalf("whiteout=%+v", whiteout)
	}
	if headers["removed"] != nil {
		t.Fatalf("overlay whiteout exported as a device: %+v", headers["removed"])
	}
	if whiteout := headers["nested/.wh.removed"]; whiteout == nil || whiteout.Typeflag != tar.TypeReg || whiteout.Size != 0 {
		t.Fatalf("nested whiteout=%+v", whiteout)
	}
	if marker := headers["opaque/.wh..wh..opq"]; marker == nil || marker.Typeflag != tar.TypeReg || marker.Size != 0 {
		t.Fatalf("opaque marker=%+v", marker)
	}
	if got := headers["opaque"].PAXRecords["SCHILY.xattr.user.test"]; got != "present" {
		t.Fatalf("opaque directory user xattr=%q, want present", got)
	}
	if _, ok := headers["opaque"].PAXRecords["SCHILY.xattr.trusted.overlay.opaque"]; ok {
		t.Fatalf("opaque implementation xattr leaked into layer: %+v", headers["opaque"].PAXRecords)
	}
}
