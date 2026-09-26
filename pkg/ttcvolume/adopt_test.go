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
	"bytes"
	"errors"
	"os"
	"testing"

	"gvisor.dev/gvisor/pkg/ttcsnapshot"
)

// largeFile creates name in branch holding a file just over the size a fork
// may leave to one branch without a copy, and returns its inode and contents.
func largeFile(t *testing.T, repo *Repository, branch, name string) (uint64, []byte) {
	t.Helper()
	ino, err := repo.CreateRegular(branch, RootInode, name, 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	contents := bytes.Repeat([]byte("0123456789abcdef"), (adoptFileMinimumBytes+4096)/16)
	write(t, repo, branch, ino, string(contents), 0)
	return ino, contents
}

func objectOf(t *testing.T, repo *Repository, branch string, ino uint64) string {
	t.Helper()
	inode, err := repo.Inode(branch, ino)
	if err != nil {
		t.Fatal(err)
	}
	return inode.Object
}

// sameHostFile reports whether two objects are names for one host file.
func sameHostFile(t *testing.T, repo *Repository, a, b string) bool {
	t.Helper()
	stat := func(object string) os.FileInfo {
		file, err := repo.openObject(object, os.O_RDONLY)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			t.Fatal(err)
		}
		return info
	}
	return os.SameFile(stat(a), stat(b))
}

func objectExists(t *testing.T, repo *Repository, object string) bool {
	t.Helper()
	id, err := ttcsnapshot.ParseObjectID(object)
	if err != nil {
		t.Fatal(err)
	}
	file, err := repo.data.Open(id, -1)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	return true
}

// A file that a fork shared, and that the fork's removal left to one branch,
// is taken over by that branch's first write instead of copied: the new
// object is a second name for the same host file, and the sealed name goes
// once the metadata that stopped using it is durable.
func TestAFileLeftToOneBranchIsTakenOverNotCopied(t *testing.T) {
	manager := openTestManager(t)
	repo, err := manager.Create("adopt")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	ino, contents := largeFile(t, repo, MainBranch, "big")
	if err := repo.Fork(MainBranch, "child"); err != nil {
		t.Fatal(err)
	}
	if err := repo.RemoveBranch("child"); err != nil {
		t.Fatal(err)
	}
	sealed := objectOf(t, repo, MainBranch, ino)
	if cloned := write(t, repo, MainBranch, ino, "X", 0); !cloned {
		t.Fatal("the first write after the fork kept the sealed object")
	}
	taken := objectOf(t, repo, MainBranch, ino)
	if taken == sealed {
		t.Fatal("the branch still writes the sealed object")
	}
	if !sameHostFile(t, repo, sealed, taken) {
		t.Fatal("the file was copied although nothing else could read it")
	}
	want := append([]byte("X"), contents[1:]...)
	if got := readAll(t, repo, MainBranch, ino); got != string(want) {
		t.Fatalf("read %d bytes after the takeover that differ from what was written", len(got))
	}
	if cloned := write(t, repo, MainBranch, ino, "Y", 1); cloned {
		t.Fatal("a second write in the same generation privatized the file again")
	}
	if err := repo.SyncMetadata(); err != nil {
		t.Fatal(err)
	}
	if objectExists(t, repo, sealed) {
		t.Fatal("the sealed name survived the fence that made its release durable")
	}
	want[1] = 'Y'
	if got := readAll(t, repo, MainBranch, ino); got != string(want) {
		t.Fatal("the file changed when its sealed name was reclaimed")
	}
}

// While the fork that shares a file exists, a write copies it, and the fork
// keeps reading what it had.
func TestAFileStillSharedIsCopied(t *testing.T) {
	manager := openTestManager(t)
	repo, err := manager.Create("adopt-shared")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	ino, contents := largeFile(t, repo, MainBranch, "big")
	if err := repo.Fork(MainBranch, "child"); err != nil {
		t.Fatal(err)
	}
	sealed := objectOf(t, repo, MainBranch, ino)
	write(t, repo, MainBranch, ino, "X", 0)
	if sameHostFile(t, repo, sealed, objectOf(t, repo, MainBranch, ino)) {
		t.Fatal("a file the fork still reads was taken over")
	}
	if got := readAll(t, repo, "child", ino); got != string(contents) {
		t.Fatal("the fork saw its source's write")
	}
}

// Below the size where a copy costs less than the journal sync a takeover
// needs, the file is copied.
func TestASmallFileLeftToOneBranchIsCopied(t *testing.T) {
	manager := openTestManager(t)
	repo, err := manager.Create("adopt-small")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	ino, err := repo.CreateRegular(MainBranch, RootInode, "small", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	write(t, repo, MainBranch, ino, "small file", 0)
	if err := repo.Fork(MainBranch, "child"); err != nil {
		t.Fatal(err)
	}
	if err := repo.RemoveBranch("child"); err != nil {
		t.Fatal(err)
	}
	sealed := objectOf(t, repo, MainBranch, ino)
	write(t, repo, MainBranch, ino, "S", 0)
	if sameHostFile(t, repo, sealed, objectOf(t, repo, MainBranch, ino)) {
		t.Fatal("a small file was taken over")
	}
}

// A fork that copied the file for its own write still holds the original
// through its diff base, the fork point, so the source copies too. Once the
// fork is gone, the source takes over its other files; each branch keeps its
// own bytes throughout.
func TestAForkHoldsWhatItCopiedThroughItsDiffBase(t *testing.T) {
	manager := openTestManager(t)
	repo, err := manager.Create("adopt-sibling")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	first, contents := largeFile(t, repo, MainBranch, "first")
	second, _ := largeFile(t, repo, MainBranch, "second")
	if err := repo.Fork(MainBranch, "child"); err != nil {
		t.Fatal(err)
	}
	sealed := objectOf(t, repo, MainBranch, first)
	write(t, repo, "child", first, "C", 0)
	write(t, repo, MainBranch, first, "M", 0)
	if sameHostFile(t, repo, sealed, objectOf(t, repo, MainBranch, first)) {
		t.Fatal("the source took over a file the fork's diff base holds")
	}
	if got := readAll(t, repo, "child", first); got != "C"+string(contents[1:]) {
		t.Fatal("the fork saw its source's write")
	}
	if got := readAll(t, repo, MainBranch, first); got != "M"+string(contents[1:]) {
		t.Fatal("the source lost its write")
	}
	if err := repo.RemoveBranch("child"); err != nil {
		t.Fatal(err)
	}
	sealed = objectOf(t, repo, MainBranch, second)
	write(t, repo, MainBranch, second, "M", 0)
	if !sameHostFile(t, repo, sealed, objectOf(t, repo, MainBranch, second)) {
		t.Fatal("the source copied a file the removed fork had shared")
	}
}

// A fork keeps its fork point as its diff base, and that snapshot must not
// change, so a fork copies a file it inherited even after its source is gone.
func TestAForkCopiesWhatItsDiffBaseHolds(t *testing.T) {
	manager := openTestManager(t)
	repo, err := manager.Create("adopt-base")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	ino, _ := largeFile(t, repo, MainBranch, "big")
	if err := repo.Fork(MainBranch, "child"); err != nil {
		t.Fatal(err)
	}
	if err := repo.RemoveBranch(MainBranch); err != nil {
		t.Fatal(err)
	}
	sealed := objectOf(t, repo, "child", ino)
	write(t, repo, "child", ino, "X", 0)
	if sameHostFile(t, repo, sealed, objectOf(t, repo, "child", ino)) {
		t.Fatal("a fork took over a file its diff base still holds")
	}
}

// An observation pins the root it reads, and what it reads must not change
// under it.
func TestAnObservedFileIsCopied(t *testing.T) {
	manager := openTestManager(t)
	repo, err := manager.Create("adopt-observed")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	ino, _ := largeFile(t, repo, MainBranch, "big")
	if err := repo.Fork(MainBranch, "child"); err != nil {
		t.Fatal(err)
	}
	if err := repo.RemoveBranch("child"); err != nil {
		t.Fatal(err)
	}
	_, pins, err := repo.pinGraphRoots([]string{graphRootName(MainBranch)})
	if err != nil {
		t.Fatal(err)
	}
	sealed := objectOf(t, repo, MainBranch, ino)
	write(t, repo, MainBranch, ino, "X", 0)
	if sameHostFile(t, repo, sealed, objectOf(t, repo, MainBranch, ino)) {
		t.Fatal("a file under an observation pin was taken over")
	}
	if err := repo.releaseGraphObservation(pins); err != nil {
		t.Fatal(err)
	}
}

// The Sentry opens files for writing through OpenDataFileWithCoW and drops
// its descriptors and mappings of the old object when told the file moved. A
// takeover moves the file to a new object, so it must say so.
func TestATakeoverIsReportedToTheOpener(t *testing.T) {
	manager := openTestManager(t)
	repo, err := manager.Create("adopt-open")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	ino, _ := largeFile(t, repo, MainBranch, "big")
	if err := repo.Fork(MainBranch, "child"); err != nil {
		t.Fatal(err)
	}
	if err := repo.RemoveBranch("child"); err != nil {
		t.Fatal(err)
	}
	sealed := objectOf(t, repo, MainBranch, ino)
	notified := false
	file, inode, cloned, err := repo.OpenDataFileWithCoW(MainBranch, ino, true, func() { notified = true })
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	if !cloned || !notified {
		t.Fatalf("cloned=%v notified=%v, want both", cloned, notified)
	}
	if inode.Object == sealed || !sameHostFile(t, repo, sealed, inode.Object) {
		t.Fatal("the opener was not given the taken-over object")
	}
}

// A crash after a takeover recovers the file through its new name, and the
// sealed name is garbage.
func TestATakenOverFileSurvivesACrash(t *testing.T) {
	root := t.TempDir()
	manager, err := OpenManager(root)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := manager.Create("adopt-crash")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	ino, contents := largeFile(t, repo, MainBranch, "big")
	if err := repo.Fork(MainBranch, "child"); err != nil {
		t.Fatal(err)
	}
	if err := repo.RemoveBranch("child"); err != nil {
		t.Fatal(err)
	}
	sealed := objectOf(t, repo, MainBranch, ino)
	write(t, repo, MainBranch, ino, "X", 0)
	if !sameHostFile(t, repo, sealed, objectOf(t, repo, MainBranch, ino)) {
		t.Fatal("setup: the file was copied")
	}
	crashed := t.TempDir()
	copyTree(t, root, crashed)
	recoveredManager, err := OpenManager(crashed)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := recoveredManager.Open("adopt-crash")
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	want := append([]byte("X"), contents[1:]...)
	if got := readAll(t, recovered, MainBranch, ino); got != string(want) {
		t.Fatal("the recovered file differs from what was written")
	}
	if err := recovered.GarbageCollect(); err != nil {
		t.Fatal(err)
	}
	if objectExists(t, recovered, sealed) {
		t.Fatal("the sealed name survived recovery and collection")
	}
	if got := readAll(t, recovered, MainBranch, ino); got != string(want) {
		t.Fatal("collection changed the recovered file")
	}
}
