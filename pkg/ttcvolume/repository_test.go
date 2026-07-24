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
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func openTestManager(t *testing.T) *Manager {
	t.Helper()
	manager, err := OpenManager(t.TempDir(), true)
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
	manager, err := OpenManager(root, true)
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

	manager, err = OpenManager(root, true)
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
	if matches, err := filepath.Glob(filepath.Join(root, "volumes-v2", id, "*.tar")); err != nil || len(matches) != 0 {
		t.Fatalf("unexpected tar persistence: matches=%v err=%v", matches, err)
	}
}

func TestTreeMutationIsPathCopyAndForkWritesNoNodes(t *testing.T) {
	repo, err := openTestManager(t).Create("tree")
	if err != nil {
		t.Fatal(err)
	}
	countNodes := func() int {
		t.Helper()
		entries, err := os.ReadDir(filepath.Join(repo.path, "nodes"))
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	beforeFork := countNodes()
	if err := repo.Fork("main", "child"); err != nil {
		t.Fatal(err)
	}
	if afterFork := countNodes(); afterFork != beforeFork {
		t.Fatalf("fork wrote %d metadata nodes, want 0", afterFork-beforeFork)
	}
	if _, err := repo.CreateRegular("child", rootInode, "x", 0644, 0, 0); err != nil {
		t.Fatal(err)
	}
	if delta := countNodes() - beforeFork; delta <= 0 || delta > 100 {
		t.Fatalf("one mutation wrote %d nodes, want bounded path-copy", delta)
	}
}
