// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcvolume

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/ttcsnapshot"
)

func TestRepositoryCloseReleasesDataStore(t *testing.T) {
	manager := openTestManager(t)
	repository, err := manager.Create("close-data-store")
	if err != nil {
		t.Fatal(err)
	}
	dataLock := filepath.Join(repository.path, "data", ".gc.lock")
	var lockStat syscall.Stat_t
	if err := syscall.Stat(dataLock, &lockStat); err != nil {
		t.Fatal(err)
	}
	// Identify open descriptors by inode rather than /proc/self/fd, which not
	// every host has.
	countOpen := func() int {
		count := 0
		for fd := 0; fd < 4096; fd++ {
			var stat syscall.Stat_t
			if syscall.Fstat(fd, &stat) == nil && stat.Dev == lockStat.Dev && stat.Ino == lockStat.Ino {
				count++
			}
		}
		return count
	}
	if got := countOpen(); got != 1 {
		t.Fatalf("open data-store locks=%d, want 1", got)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	if got := countOpen(); got != 0 {
		t.Fatalf("open data-store locks after Repository.Close=%d, want 0", got)
	}
	if err := repository.Close(); err != nil {
		t.Fatalf("second Repository.Close: %v", err)
	}
}

func openTestManager(t *testing.T) *Manager {
	t.Helper()
	manager, err := OpenManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func graphJournalPath(t *testing.T, repositoryPath string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(repositoryPath, "graph", "graph-*.journal"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("graph journals=%v, want exactly one", matches)
	}
	return matches[0]
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

func TestConcurrentMetadataReadsStayOnLiveGraphGeneration(t *testing.T) {
	manager := openTestManager(t)
	repository, err := manager.Create("concurrent-reads")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	const files = 48
	inodes := make([]uint64, 0, files)
	for i := 0; i < files; i++ {
		name := fmt.Sprintf("file-%03d", i)
		ino, err := repository.CreateRegular(MainBranch, RootInode, name, 0644, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := repository.WriteAt(MainBranch, ino, []byte(name), 0); err != nil {
			t.Fatal(err)
		}
		inodes = append(inodes, ino)
	}
	if err := repository.SyncMetadata(); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	errorsCh := make(chan error, 16)
	var readers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		readers.Add(1)
		go func(seed int) {
			defer readers.Done()
			<-start
			for iteration := 0; iteration < 500; iteration++ {
				index := (iteration + seed) % files
				name := fmt.Sprintf("file-%03d", index)
				ino, err := repository.Lookup(MainBranch, RootInode, name)
				if err == nil {
					_, err = repository.Inode(MainBranch, ino)
				}
				if err == nil {
					err = repository.EnsureDataRange(MainBranch, ino, 0, 1)
				}
				if err != nil {
					select {
					case errorsCh <- err:
					default:
					}
					return
				}
			}
		}(worker)
	}
	close(start)
	for iteration := 0; iteration < 40; iteration++ {
		updates := make([]Inode, 0, files)
		for _, ino := range inodes {
			inode, err := repository.Inode(MainBranch, ino)
			if err != nil {
				t.Fatal(err)
			}
			inode.AtimeNsec++
			updates = append(updates, inode)
		}
		if err := repository.UpdateMetadataBatch(MainBranch, updates); err != nil {
			t.Fatal(err)
		}
		if err := repository.SyncMetadata(); err != nil {
			t.Fatal(err)
		}
	}
	readers.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Fatalf("concurrent metadata read crossed a reclaimed graph generation: %v", err)
	}
}

// A batch of more updates than a journal transaction holds, as a file
// system's flush of every inode it changed before a live fork, is published.
func TestUpdateMetadataBatchLargerThanTransaction(t *testing.T) {
	manager := openTestManager(t)
	repository, err := manager.Create("large-batch")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	// 1200 inodes with 8 KiB of extended attributes are some 10 MB.
	const files = 1200
	value := strings.Repeat("x", 8<<10)
	updates := make([]Inode, 0, files)
	for i := 0; i < files; i++ {
		ino, err := repository.CreateRegular(MainBranch, RootInode, fmt.Sprintf("file-%04d", i), 0644, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		inode, err := repository.Inode(MainBranch, ino)
		if err != nil {
			t.Fatal(err)
		}
		inode.Xattrs = map[string]string{"user.value": value}
		inode.MtimeNsec = int64(i + 1)
		updates = append(updates, inode)
	}
	if err := repository.UpdateMetadataBatch(MainBranch, updates); err != nil {
		t.Fatalf("UpdateMetadataBatch: %v", err)
	}
	if err := repository.SyncMetadata(); err != nil {
		t.Fatal(err)
	}
	for _, updated := range []Inode{updates[0], updates[files/2], updates[files-1]} {
		inode, err := repository.Inode(MainBranch, updated.Number)
		if err != nil {
			t.Fatal(err)
		}
		if inode.MtimeNsec != updated.MtimeNsec || inode.Xattrs["user.value"] != value {
			t.Errorf("inode %d: mtime %d, %d bytes of xattr; want mtime %d, %d bytes",
				updated.Number, inode.MtimeNsec, len(inode.Xattrs["user.value"]), updated.MtimeNsec, len(value))
		}
	}
}

func TestOpenSealedDataFile(t *testing.T) {
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
	write(t, repo, "main", ino, "base", 0)
	sealed := func(branch string) (string, bool) {
		t.Helper()
		file, inode, sealed, err := repo.OpenSealedDataFile(branch, ino)
		if err != nil {
			t.Fatalf("OpenSealedDataFile(%q): %v", branch, err)
		}
		defer file.Close()
		buf := make([]byte, 4)
		if _, err := file.ReadAt(buf, 0); err != nil {
			t.Fatalf("reading %q's object: %v", branch, err)
		}
		if _, err := file.WriteAt([]byte("x"), 0); err == nil {
			t.Fatalf("%q's object opened writable", branch)
		}
		return inode.Object + ":" + string(buf), sealed
	}
	if _, ok := sealed("main"); ok {
		t.Fatal("a branch's own object is sealed before a fork")
	}
	before, _ := sealed("main")
	if err := repo.Fork("main", "child"); err != nil {
		t.Fatal(err)
	}
	for _, branch := range []string{"main", "child"} {
		got, ok := sealed(branch)
		if !ok || got != before {
			t.Fatalf("%q after the fork opens %q, sealed %t; want the sealed %q", branch, got, ok, before)
		}
	}
	write(t, repo, "main", ino, "PA", 0)
	if got, ok := sealed("main"); ok || got == before {
		t.Fatalf("main after its write opens %q, sealed %t; want its own copy", got, ok)
	}
	if got, ok := sealed("child"); !ok || got != before {
		t.Fatalf("child after main's write opens %q, sealed %t; want the sealed %q", got, ok, before)
	}
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
	journal := graphJournalPath(t, repo.path)
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(journal, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte{0, 0, 0}); err != nil {
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

func TestFileCloneAutomaticallyFallsBack(t *testing.T) {
	manager := openTestManager(t)
	repo, err := manager.Create("actual-reflink")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	ino, err := repo.CreateRegular(MainBranch, RootInode, "value", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	write(t, repo, MainBranch, ino, "base", 0)
	if err := repo.Fork(MainBranch, "child"); err != nil {
		t.Fatal(err)
	}

	// The configured capability is only an optimization hint. Correctness must
	// transparently fall back to a whole-file copy.
	repo.copyMode = CopyModeReflink
	n, cloned, writeErr := repo.WriteAt("child", ino, []byte("CH"), 0)
	if writeErr != nil || n != 2 || !cloned {
		t.Fatalf("branch write = n %d, cloned %t, err %v; want 2, true, nil", n, cloned, writeErr)
	}
	if got := readAll(t, repo, MainBranch, ino); got != "base" {
		t.Fatalf("child write changed parent to %q", got)
	}
	if got := readAll(t, repo, "child", ino); got != "CHse" {
		t.Fatalf("child data = %q, want CHse", got)
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

func TestRepositoryReopensAfterDurabilityFenceWithoutImport(t *testing.T) {
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
	// A second Repository instance cannot observe this process's bounded live
	// metadata batch until an ordinary filesystem durability boundary publishes
	// it to the immutable graph.
	if err := repo.SyncMetadata(); err != nil {
		t.Fatal(err)
	}
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

func TestMetadataBatchDropsDeletedTransientSubtree(t *testing.T) {
	root := t.TempDir()
	manager, err := OpenManager(root)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := manager.Create("transient")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := repo.CreateDirectory("main", rootInode, "temporary", 0755, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	ino, err := repo.CreateRegular("main", dir, "payload", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	write(t, repo, "main", ino, "discarded", 0)
	if err := repo.Unlink("main", dir, "payload"); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReapOrphan("main", ino); err != nil {
		t.Fatal(err)
	}
	if err := repo.RemoveDirectory("main", rootInode, "temporary"); err != nil {
		t.Fatal(err)
	}
	if err := repo.SyncMetadata(); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Lookup("main", rootInode, "temporary"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted transient directory lookup: %v", err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	repo, err = manager.Open("transient")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if _, err := repo.Lookup("main", rootInode, "temporary"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replayed transient directory lookup: %v", err)
	}
}

func TestExchange(t *testing.T) {
	manager, err := OpenManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo, err := manager.Create("exchange")
	if err != nil {
		t.Fatal(err)
	}
	a, err := repo.CreateDirectory(MainBranch, RootInode, "a", 0755, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := repo.CreateDirectory(MainBranch, RootInode, "b", 0755, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	file, err := repo.CreateRegular(MainBranch, a, "x", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := repo.CreateDirectory(MainBranch, b, "y", 0755, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	inner, err := repo.CreateRegular(MainBranch, dir, "inner", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Exchange(MainBranch, a, "x", b, "missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("exchange with a missing file: %v, want not found", err)
	}
	if err := repo.Exchange(MainBranch, a, "x", b, "y"); err != nil {
		t.Fatal(err)
	}
	check := func(repo *Repository, branch, when string) {
		t.Helper()
		for _, want := range []struct {
			parent uint64
			name   string
			ino    uint64
		}{{a, "x", dir}, {b, "y", file}, {dir, "inner", inner}} {
			if got, err := repo.Lookup(branch, want.parent, want.name); err != nil || got != want.ino {
				t.Fatalf("%s: lookup of %s = %d, %v, want %d", when, want.name, got, err, want.ino)
			}
		}
		// The directory's ".." moved from b to a.
		for _, want := range []struct {
			ino   uint64
			nlink uint32
		}{{a, 3}, {b, 2}} {
			inode, err := repo.Inode(branch, want.ino)
			if err != nil || inode.Nlink != want.nlink {
				t.Fatalf("%s: inode %d has %d links, %v, want %d", when, want.ino, inode.Nlink, err, want.nlink)
			}
		}
	}
	check(repo, MainBranch, "after the exchange")
	if err := repo.Fork(MainBranch, "child"); err != nil {
		t.Fatal(err)
	}
	check(repo, "child", "in a fork")
	if err := repo.SyncMetadata(); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	repo, err = manager.Open("exchange")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	check(repo, MainBranch, "reopened")
}

func TestNewDirectoryLookupsAgreeWithTheGraph(t *testing.T) {
	manager, err := OpenManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo, err := manager.Create("lookups")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := repo.CreateDirectory(MainBranch, RootInode, "dir", 0755, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	// More files than a metadata batch holds, so that some are in the graph.
	const files = metadataBatchOperations + 44
	inodes := make(map[string]uint64)
	for i := 0; i < files; i++ {
		name := fmt.Sprintf("f%d", i)
		ino, err := repo.CreateRegular(MainBranch, dir, name, 0644, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		inodes[name] = ino
	}
	if _, err := repo.CreateRegular(MainBranch, dir, "f0", 0644, 0, 0); !errors.Is(err, os.ErrExist) {
		t.Fatalf("creating an existing name: %v", err)
	}
	if err := repo.Unlink(MainBranch, dir, "f1"); err != nil {
		t.Fatal(err)
	}
	delete(inodes, "f1")
	if err := repo.Rename(MainBranch, dir, "f2", RootInode, "moved", false); err != nil {
		t.Fatal(err)
	}
	delete(inodes, "f2")
	check := func(repo *Repository, branch, when string) {
		t.Helper()
		for name, want := range inodes {
			if got, err := repo.Lookup(branch, dir, name); err != nil || got != want {
				t.Fatalf("%s: lookup of %s = %d, %v, want %d", when, name, got, err, want)
			}
		}
		for _, name := range []string{"f1", "f2", "missing"} {
			if _, err := repo.Lookup(branch, dir, name); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s: lookup of %s: %v, want not found", when, name, err)
			}
		}
		entries, err := repo.ListDirectory(branch, dir)
		if err != nil || len(entries) != len(inodes) {
			t.Fatalf("%s: %d entries, %v, want %d", when, len(entries), err, len(inodes))
		}
	}
	check(repo, MainBranch, "before a sync")
	if err := repo.Fork(MainBranch, "child"); err != nil {
		t.Fatal(err)
	}
	check(repo, "child", "in a fork")
	if err := repo.SyncMetadata(); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	repo, err = manager.Open("lookups")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	check(repo, MainBranch, "reopened")
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
	if after.Root != before.Root || after.WorkingSnapshot == before.WorkingSnapshot || after.Generation != before.Generation+1 {
		t.Fatalf("source after Abort=%+v, want same root and a fresh working origin", after)
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

func TestClusteredRepositoryUsesExternallyAllocatedOrigins(t *testing.T) {
	manager, err := OpenManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo, err := manager.CreateWithOrigin("clustered", 1001)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	main, err := repo.Branch(MainBranch)
	if err != nil {
		t.Fatal(err)
	}
	if main.WorkingSnapshot != 1001 {
		t.Fatalf("initial working origin=%d, want 1001", main.WorkingSnapshot)
	}
	if !repo.Info().Clustered {
		t.Fatal("externally allocated repository was not marked clustered")
	}
	tx, err := repo.BeginForkWithOrigins(MainBranch, "child", 1002, 1003)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	main, err = repo.Branch(MainBranch)
	if err != nil {
		t.Fatal(err)
	}
	child, err := repo.Branch("child")
	if err != nil {
		t.Fatal(err)
	}
	if main.WorkingSnapshot != 1002 || child.WorkingSnapshot != 1003 {
		t.Fatalf("fork working origins=(%d,%d), want (1002,1003)", main.WorkingSnapshot, child.WorkingSnapshot)
	}
	sealed, ok := repo.graph.Snapshot(1001)
	if !ok || sealed.State != ttcsnapshot.SnapshotSealedVolatile {
		t.Fatalf("sealed clustered origin=(%+v,%t)", sealed, ok)
	}
}

func TestClusteredRepositoryOpenPreservesSealedBranchUntilActivation(t *testing.T) {
	manager, err := OpenManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := manager.CreateWithOrigin("cold-clustered", 1001)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.withLock(func() error {
		return repository.graph.Seal(1001, ttcsnapshot.SnapshotSealedVolatile)
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}

	repository, err = manager.Open("cold-clustered")
	if err != nil {
		t.Fatal(err)
	}
	branch, err := repository.Branch(MainBranch)
	if err != nil {
		t.Fatal(err)
	}
	if branch.WorkingSnapshot != 1001 {
		t.Fatalf("read-only reopen changed sealed origin to %d", branch.WorkingSnapshot)
	}
	if needs, err := repository.BranchNeedsWorkingOrigin(MainBranch); err != nil || !needs {
		t.Fatalf("BranchNeedsWorkingOrigin()=(%t,%v), want (true,nil)", needs, err)
	}
	adopted, err := repository.InstallBranchWorkingOrigin(MainBranch, 1002)
	if err != nil || !adopted {
		t.Fatalf("InstallBranchWorkingOrigin()=(%t,%v), want (true,nil)", adopted, err)
	}
	branch, err = repository.Branch(MainBranch)
	if err != nil {
		t.Fatal(err)
	}
	if branch.WorkingSnapshot != 1002 {
		t.Fatalf("activated working origin=%d, want 1002", branch.WorkingSnapshot)
	}
	working, ok := repository.graph.Snapshot(1002)
	if !ok || working.State != ttcsnapshot.SnapshotWorking {
		t.Fatalf("activated snapshot=(%+v,%t), want working", working, ok)
	}
	if needs, err := repository.BranchNeedsWorkingOrigin(MainBranch); err != nil || needs {
		t.Fatalf("BranchNeedsWorkingOrigin() after activation=(%t,%v), want (false,nil)", needs, err)
	}
	if err := repository.Close(); err != nil {
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

func TestMutationAndForkAppendBoundedGraphTransactions(t *testing.T) {
	repo, err := openTestManager(t).Create("tree")
	if err != nil {
		t.Fatal(err)
	}
	journalSize := func() int64 {
		t.Helper()
		info, err := os.Stat(graphJournalPath(t, repo.path))
		if err != nil {
			t.Fatal(err)
		}
		return info.Size()
	}
	sealedSize := journalSize()
	if err := repo.Fork("main", "child"); err != nil {
		t.Fatal(err)
	}
	if delta := journalSize() - sealedSize; delta <= 0 || delta > 16<<10 {
		t.Fatalf("fork appended %d graph bytes, want one bounded root transaction", delta)
	}
	beforeMutation := journalSize()
	if _, err := repo.CreateRegular("child", rootInode, "x", 0644, 0, 0); err != nil {
		t.Fatal(err)
	}
	if delta := journalSize() - beforeMutation; delta <= 0 || delta > 8<<10 {
		t.Fatalf("one mutation appended %d graph bytes, want one compact logical filesystem transaction", delta)
	}
}

func TestRepeatedMetadataUpdatesKeepGraphBoundedAndRecoverable(t *testing.T) {
	manager := openTestManager(t)
	repo, err := manager.Create("metadata-churn")
	if err != nil {
		t.Fatal(err)
	}
	ino, err := repo.CreateRegular(MainBranch, RootInode, "value", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	baseline := repo.graph.Stats().Objects
	for iteration := 0; iteration < 1000; iteration++ {
		inode, err := repo.Inode(MainBranch, ino)
		if err != nil {
			t.Fatal(err)
		}
		inode.Mode = uint32(0600 | iteration&077)
		inode.MtimeNsec = int64(iteration + 1)
		if err := repo.UpdateMetadata(MainBranch, inode); err != nil {
			t.Fatalf("metadata update %d: %v", iteration, err)
		}
		if stats := repo.graph.Stats(); stats.Objects > baseline+4 || stats.Reclaiming != 0 {
			t.Fatalf("metadata update %d left an unbounded graph: baseline=%d stats=%+v", iteration, baseline, stats)
		}
	}
	if err := repo.SyncMetadata(); err != nil {
		t.Fatal(err)
	}
	root := repo.path
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}

	repo, err = manager.Open("metadata-churn")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	got, err := repo.Inode(MainBranch, ino)
	if err != nil {
		t.Fatal(err)
	}
	if got.MtimeNsec != 1000 {
		t.Fatalf("recovered mtime=%d, want 1000", got.MtimeNsec)
	}
	if stats := repo.graph.Stats(); stats.Objects > baseline+4 || stats.Reclaiming != 0 {
		t.Fatalf("reopened graph at %q is unbounded: baseline=%d stats=%+v", root, baseline, stats)
	}
}

func TestFsyncMetadataChurnCompactsGraphJournal(t *testing.T) {
	manager := openTestManager(t)
	repo, err := manager.Create("fsync-metadata-churn")
	if err != nil {
		t.Fatal(err)
	}
	ino, err := repo.CreateRegular(MainBranch, RootInode, "database", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Use one legal-size xattr value to make the regression inexpensive. The
	// important shape is a durability boundary after repeatedly updating the
	// same inode: the metadata batch contains one key and therefore never
	// reaches its distinct-operation flush threshold.
	value := strings.Repeat("x", 60<<10)
	for iteration := 0; iteration < 160; iteration++ {
		inode, err := repo.Inode(MainBranch, ino)
		if err != nil {
			t.Fatal(err)
		}
		inode.MtimeNsec = int64(iteration + 1)
		inode.Xattrs = map[string]string{"user.ttc-journal-test": value}
		if err := repo.UpdateMetadata(MainBranch, inode); err != nil {
			t.Fatalf("metadata update %d: %v", iteration, err)
		}
		if err := repo.SyncMetadata(); err != nil {
			t.Fatalf("metadata sync %d: %v", iteration, err)
		}
	}
	if got := repo.graph.JournalBytes(); got >= reclaimJournalBytes {
		t.Fatalf("graph journal remained unbounded at %d bytes, threshold %d", got, reclaimJournalBytes)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	repo, err = manager.Open("fsync-metadata-churn")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if got, err := repo.Inode(MainBranch, ino); err != nil {
		t.Fatal(err)
	} else if got.MtimeNsec != 160 || got.Xattrs["user.ttc-journal-test"] != value {
		t.Fatalf("recovered inode metadata = %+v", got)
	}
}

func TestMetadataUsesGraphWithoutPerEntryFiles(t *testing.T) {
	manager := openTestManager(t)
	repo, err := manager.Create("journal-only")
	if err != nil {
		t.Fatal(err)
	}
	_, err = repo.CreateRegular(MainBranch, RootInode, "a-very-long-guest-name", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	root := repo.path
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "graph"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	if len(names) != 4 || names[0] != "graph-0000000000000001.checkpoint" || names[1] != "graph-0000000000000001.journal" || names[2] != "graph.head" || names[3] != "graph.lock" {
		t.Fatalf("snapshot graph entries=%q, want one selected checkpoint/journal generation", strings.Join(names, ","))
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

func TestNewScopedBranchIDAllowsVisibleNameReuse(t *testing.T) {
	first, err := NewScopedBranchID("family", "child")
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewScopedBranchID("family", "child")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("new branch incarnations reused physical ID %q", first)
	}
	if !strings.HasPrefix(first, "b-") || !strings.HasPrefix(second, "b-") {
		t.Fatalf("new scoped branch IDs = %q, %q, want b- prefixes", first, second)
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

func TestPathBackedRepositoryExportsBulkObjects(t *testing.T) {
	root := t.TempDir()
	manager, err := OpenManager(root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := manager.Create("path-export")
	if err != nil {
		t.Fatal(err)
	}
	ino, err := repository.CreateRegular(MainBranch, RootInode, "value", 0600, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	write(t, repository, MainBranch, ino, "manager-side export", 0)
	path := repository.path
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}

	// Daemon publication opens repositories by their trusted absolute path,
	// rather than through Manager.Open. This must still expose a path-backed
	// LocalDataStore to ExportSnapshot.
	repository, err = OpenRepository(path, CopyModeFull)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	snapshot := filepath.Join(t.TempDir(), "snapshot")
	if _, err := repository.ExportSnapshot(MainBranch, snapshot); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(snapshot, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("snapshot objects=%v, want one", entries)
	}
}

func TestExportSnapshotNeverFallsBackToLegacyObjectPath(t *testing.T) {
	manager := openTestManager(t)
	repository, err := manager.Create("no-legacy-export")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	ino, err := repository.CreateRegular(MainBranch, RootInode, "value", 0600, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	write(t, repository, MainBranch, ino, "authoritative graph object", 0)
	if err := repository.SyncMetadata(); err != nil {
		t.Fatal(err)
	}
	inode, err := repository.Inode(MainBranch, ino)
	if err != nil {
		t.Fatal(err)
	}
	authoritative := filepath.Join(repository.path, repository.objectPath(inode.Object))
	if err := os.Remove(authoritative); err != nil {
		t.Fatal(err)
	}
	legacyDir := filepath.Join(repository.path, "objects")
	if err := os.MkdirAll(legacyDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, inode.Object), []byte("legacy decoy"), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := repository.ExportSnapshot(MainBranch, filepath.Join(t.TempDir(), "snapshot")); err == nil {
		t.Fatal("ExportSnapshot used a legacy flat object instead of rejecting missing graph data")
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
	if err := repository.RemoveBranch(MainBranch); !errors.Is(err, ErrFinalBranch) {
		t.Fatalf("RemoveBranch(main)=%v, want ErrFinalBranch", err)
	}
}

func TestRemoveBranchRejectsFinalScopedBranch(t *testing.T) {
	manager := openTestManager(t)
	repository, err := manager.Create("data")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if err := repository.Fork(MainBranch, "scoped"); err != nil {
		t.Fatal(err)
	}
	if err := repository.RemoveBranch(MainBranch); err != nil {
		t.Fatal(err)
	}
	if err := repository.RemoveBranch("scoped"); !errors.Is(err, ErrFinalBranch) {
		t.Fatalf("RemoveBranch(scoped)=%v, want ErrFinalBranch", err)
	}
	if _, err := repository.Branch("scoped"); err != nil {
		t.Fatalf("final scoped branch was removed: %v", err)
	}
}

func TestGarbageCollectReportsInterruptedFinalBranchRemoval(t *testing.T) {
	manager := openTestManager(t)
	repository, err := manager.Create("data")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if err := os.Remove(filepath.Join(repository.path, "branches", MainBranch+".json")); err != nil {
		t.Fatal(err)
	}
	if err := repository.GarbageCollect(); !errors.Is(err, ErrEmptyRepository) {
		t.Fatalf("GarbageCollect()=%v, want ErrEmptyRepository", err)
	}
}

func TestManagerRemovePrivateFinalRepository(t *testing.T) {
	manager := openTestManager(t)
	parent := filepath.Join(manager.root, "families")
	if err := os.MkdirAll(parent, 0700); err != nil {
		t.Fatal(err)
	}
	repository, err := manager.CreatePrivate(filepath.Join(parent, "rootfs"), "rootfs")
	if err != nil {
		t.Fatal(err)
	}
	path := repository.path
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	if err := manager.RemovePrivate(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed private repository stat=%v, want not exist", err)
	}
	if err := manager.RemovePrivate(manager.root); err == nil {
		t.Fatal("RemovePrivate accepted manager root")
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

func TestOpenRecoversCommittedGraphBeforeBranchPointer(t *testing.T) {
	manager := openTestManager(t)
	repository, err := manager.Create("recover-root-pointer")
	if err != nil {
		t.Fatal(err)
	}
	branch, err := repository.Branch(MainBranch)
	if err != nil {
		t.Fatal(err)
	}
	var root ttcsnapshot.ObjectID
	err = repository.withLock(func() error {
		var applyErr error
		root, applyErr = repository.graph.ApplyFilesystem(
			ttcsnapshot.SnapshotID(branch.WorkingSnapshot),
			graphRootName(MainBranch),
			[]ttcsnapshot.FilesystemOperation{{PutInode: &ttcsnapshot.InodeRecord{
				Number: 99, Type: "directory", Mode: 0755, Nlink: 2,
			}}},
		)
		return applyErr
	})
	if err != nil {
		t.Fatal(err)
	}
	// Model a crash before commitMetadata publishes the branch pointer.
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	repository, err = manager.Open("recover-root-pointer")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	recovered, err := repository.Branch(MainBranch)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Root != root.String() {
		t.Fatalf("recovered root=%q, want %q", recovered.Root, root)
	}
	if inode, err := repository.Inode(MainBranch, 99); err != nil || inode.Type != "directory" {
		t.Fatalf("recovered inode=%+v, err=%v", inode, err)
	}
}

func TestReopenUsesDirectGraphLookupWithoutWholeTreeDecode(t *testing.T) {
	manager := openTestManager(t)
	repository, err := manager.Create("direct-lookup")
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 100; index++ {
		if _, err := repository.CreateRegular(MainBranch, RootInode, fmt.Sprintf("file-%03d", index), 0644, 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	repository, err = manager.Open("direct-lookup")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	ino, err := repository.Lookup(MainBranch, RootInode, "file-050")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Inode(MainBranch, ino); err != nil {
		t.Fatal(err)
	}
	repository.metadataMu.Lock()
	state := repository.metadata[MainBranch]
	repository.metadataMu.Unlock()
	state.mu.RLock()
	if state.complete || len(state.entries) != 1 || len(state.inodes) != 1 || len(state.directories) != 0 {
		t.Fatalf("lookup eagerly decoded metadata: complete=%t entries=%d inodes=%d directories=%d", state.complete, len(state.entries), len(state.inodes), len(state.directories))
	}
	state.mu.RUnlock()
	entries, err := repository.ListDirectory(MainBranch, RootInode)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 100 {
		t.Fatalf("directory entries=%d, want 100", len(entries))
	}
	state.mu.RLock()
	defer state.mu.RUnlock()
	if state.complete || len(state.inodes) != 1 || len(state.directories[RootInode]) != 100 {
		t.Fatalf("readdir decoded unrelated inodes: complete=%t inodes=%d entries=%d", state.complete, len(state.inodes), len(state.directories[RootInode]))
	}
}

func TestOpenRecoversInterruptedForkGraphTransaction(t *testing.T) {
	manager := openTestManager(t)
	repository, err := manager.Create("recover-fork")
	if err != nil {
		t.Fatal(err)
	}
	branch, err := repository.Branch(MainBranch)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.withLock(func() error {
		_, err := repository.graph.SealAndFork(
			ttcsnapshot.SnapshotID(branch.WorkingSnapshot),
			ttcsnapshot.SnapshotSealedLocal,
			map[string]string{graphRootName("unpublished"): graphRootName(MainBranch)},
			2,
		)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Model a crash before either branch pointer is published.
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	repository, err = manager.Open("recover-fork")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	recovered, err := repository.Branch(MainBranch)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.WorkingSnapshot == branch.WorkingSnapshot {
		t.Fatalf("recovery reused sealed working origin %d", branch.WorkingSnapshot)
	}
	working, ok := repository.graph.Snapshot(ttcsnapshot.SnapshotID(recovered.WorkingSnapshot))
	if !ok || working.State != ttcsnapshot.SnapshotWorking {
		t.Fatalf("recovered working snapshot=%+v, found=%t", working, ok)
	}
	if _, ok := repository.graph.Root(graphRootName("unpublished")); ok {
		t.Fatal("unpublished child graph root survived recovery")
	}
	if _, err := repository.Branch("unpublished"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unpublished branch lookup=%v, want not exist", err)
	}
}

func TestOpenReleasesInterruptedConstructionPins(t *testing.T) {
	manager := openTestManager(t)
	repository, err := manager.Create("recover-construction")
	if err != nil {
		t.Fatal(err)
	}
	branch, err := repository.Branch(MainBranch)
	if err != nil {
		t.Fatal(err)
	}
	var id ttcsnapshot.ObjectID
	err = repository.withLock(func() error {
		var createErr error
		id, createErr = repository.graph.CreateObject(
			ttcsnapshot.SnapshotID(branch.WorkingSnapshot),
			"construct/interrupted",
			ttcsnapshot.ObjectKindFile,
			nil,
			nil,
		)
		return createErr
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	repository, err = manager.Open("recover-construction")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, ok := repository.graph.Inspect().Pins["construct/interrupted"]; ok {
		t.Fatal("interrupted construction pin survived recovery")
	}
	// Releasing the pin collects the object it alone held.
	if object, ok := repository.graph.Object(id); ok {
		t.Fatalf("interrupted object survived its pin's release: %+v", object)
	}
}

func TestOriginHandoffUsesRepositoryWriterFence(t *testing.T) {
	manager := openTestManager(t)
	repository, err := manager.CreateWithOrigin("handoff-fence", 100)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	ino, err := repository.CreateRegular(MainBranch, RootInode, "payload", 0600, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.WriteAt(MainBranch, ino, []byte("payload"), 0); err != nil {
		t.Fatal(err)
	}
	if err := repository.ForkWithOrigins(MainBranch, "peer", 101, 102); err != nil {
		t.Fatal(err)
	}
	shared, err := ttcsnapshot.OpenSharedPackStore(filepath.Join(t.TempDir(), "shared"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.PublishOriginHandoff(context.Background(), shared, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.FreezeOriginHandoff(100); err != nil {
		t.Fatal(err)
	}
	if err := repository.CompleteOriginHandoff(100); err != nil {
		t.Fatal(err)
	}
	if snapshot, ok := repository.graph.Snapshot(100); !ok || snapshot.State != ttcsnapshot.SnapshotShared {
		t.Fatalf("published snapshot=(%+v,%t), want shared", snapshot, ok)
	}
}

// A frozen handoff is listed until it completes, so that a restarted daemon
// finds it to complete.
func TestFrozenOriginHandoffsAreListedUntilComplete(t *testing.T) {
	manager := openTestManager(t)
	repository, err := manager.CreateWithOrigin("frozen-list", 100)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	ino, err := repository.CreateRegular(MainBranch, RootInode, "payload", 0600, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.WriteAt(MainBranch, ino, []byte("payload"), 0); err != nil {
		t.Fatal(err)
	}
	if err := repository.ForkWithOrigins(MainBranch, "peer", 101, 102); err != nil {
		t.Fatal(err)
	}
	shared, err := ttcsnapshot.OpenSharedPackStore(filepath.Join(t.TempDir(), "shared"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.PublishOriginHandoff(context.Background(), shared, 100); err != nil {
		t.Fatal(err)
	}
	if frozen := repository.FrozenOriginHandoffs(); len(frozen) != 0 || repository.OriginHandoffFrozen(100) {
		t.Fatalf("frozen handoffs before the freeze = %v", frozen)
	}
	if _, err := repository.FreezeOriginHandoff(100); err != nil {
		t.Fatal(err)
	}
	if frozen := repository.FrozenOriginHandoffs(); len(frozen) != 1 || frozen[0] != 100 || !repository.OriginHandoffFrozen(100) {
		t.Fatalf("frozen handoffs = %v, want [100]", frozen)
	}
	if err := repository.CompleteOriginHandoff(100); err != nil {
		t.Fatal(err)
	}
	if frozen := repository.FrozenOriginHandoffs(); len(frozen) != 0 || repository.OriginHandoffFrozen(100) {
		t.Fatalf("frozen handoffs after completion = %v", frozen)
	}
}

// A fork that a frozen handoff refuses must leave the source as it was. It
// used to publish the source's new file object, keep only a construction pin
// on it and fail; the next process to open the repository took the pin for a
// crashed transaction's and released it, and the source could never fork
// again: "inode references unavailable working file".
func TestForkRefusedByAFrozenHandoffLeavesTheSourceForkable(t *testing.T) {
	root := t.TempDir()
	manager, err := OpenManager(root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := manager.CreateWithOrigin("frozen-fork", 100)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	ino, err := repository.CreateRegular(MainBranch, RootInode, "base", 0600, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.WriteAt(MainBranch, ino, []byte("base"), 0); err != nil {
		t.Fatal(err)
	}
	if err := repository.ForkWithOrigins(MainBranch, "ck", 101, 102); err != nil {
		t.Fatal(err)
	}
	shared, err := ttcsnapshot.OpenSharedPackStore(filepath.Join(t.TempDir(), "shared"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.PublishOriginHandoff(context.Background(), shared, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.FreezeOriginHandoff(100); err != nil {
		t.Fatal(err)
	}
	created, err := repository.CreateRegular(MainBranch, RootInode, "newfile", 0600, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.WriteAt(MainBranch, created, []byte("hello"), 0); err != nil {
		t.Fatal(err)
	}
	if err := repository.ForkWithOrigins(MainBranch, "a1", 103, 104); !errors.Is(err, ttcsnapshot.ErrSnapshotLedgerFrozen) {
		t.Fatalf("fork during a frozen handoff: %v, want ErrSnapshotLedgerFrozen", err)
	}
	// ttcd opens the repository to clean up after the refused fork.
	other, err := OpenManager(root)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := other.Open("frozen-fork")
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if err := repository.CompleteOriginHandoff(100); err != nil {
		t.Fatal(err)
	}
	if err := repository.ForkWithOrigins(MainBranch, "a2", 105, 106); err != nil {
		t.Fatalf("fork after the handoff completed: %v", err)
	}
	for _, branch := range []string{MainBranch, "a2"} {
		data := make([]byte, 5)
		if n, err := repository.ReadAt(branch, created, data, 0); err != nil || string(data[:n]) != "hello" {
			t.Fatalf("%s reads %q, %v; want hello", branch, data[:n], err)
		}
	}
}

// frozenHandoffRepository returns a repository whose main branch shares
// origin 100 with a fork "ck", with origin 100 frozen for an authority
// handoff, as a sibling's publication freezes it.
func frozenHandoffRepository(t *testing.T, root string) *Repository {
	t.Helper()
	manager, err := OpenManager(root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := manager.CreateWithOrigin("frozen", 100)
	if err != nil {
		t.Fatal(err)
	}
	ino, err := repository.CreateRegular(MainBranch, RootInode, "base", 0600, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.WriteAt(MainBranch, ino, []byte("base"), 0); err != nil {
		t.Fatal(err)
	}
	if err := repository.ForkWithOrigins(MainBranch, "ck", 101, 102); err != nil {
		t.Fatal(err)
	}
	shared, err := ttcsnapshot.OpenSharedPackStore(filepath.Join(t.TempDir(), "shared"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.PublishOriginHandoff(context.Background(), shared, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.FreezeOriginHandoff(100); err != nil {
		t.Fatal(err)
	}
	return repository
}

// A live branch goes on creating files while a handoff has a snapshot it
// shares frozen: the batch that fills up meanwhile waits for the handoff to
// end. Failing the file creation that filled it made the Sentry's overlay
// panic.
func TestMetadataBatchWaitsOutAFrozenHandoff(t *testing.T) {
	root := t.TempDir()
	repository := frozenHandoffRepository(t, root)
	defer repository.Close()
	const files = 2*metadataBatchOperations + 10
	for i := 0; i < files; i++ {
		if _, err := repository.CreateRegular(MainBranch, RootInode, fmt.Sprintf("f%d", i), 0600, 0, 0); err != nil {
			t.Fatalf("creating file %d during the handoff: %v", i, err)
		}
	}
	if err := repository.CompleteOriginHandoff(100); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CreateRegular(MainBranch, RootInode, "after", 0600, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := repository.SyncMetadata(); err != nil {
		t.Fatal(err)
	}
	other, err := OpenManager(root)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := other.Open("frozen")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	entries, err := reopened.ListDirectory(MainBranch, RootInode)
	if err != nil {
		t.Fatal(err)
	}
	// base, the files, and after.
	if len(entries) != files+2 {
		t.Fatalf("main has %d entries, want %d", len(entries), files+2)
	}
}

// Removing a branch whose roots reach a snapshot a handoff has frozen removes
// it, and the repository still opens: the roots wait for an open after the
// handoff. The removal used to fail with the branch's pointer gone, and every
// open after it failed while the handoff lasted, the cleanup that ends a
// canceled one among them.
func TestBranchRemovedDuringAFrozenHandoff(t *testing.T) {
	root := t.TempDir()
	repository := frozenHandoffRepository(t, root)
	defer repository.Close()
	if err := repository.RemoveBranch("ck"); err != nil {
		t.Fatalf("removing a branch during the handoff: %v", err)
	}
	if _, err := repository.Branch("ck"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed branch: %v, want not found", err)
	}
	open := func() {
		t.Helper()
		other, err := OpenManager(root)
		if err != nil {
			t.Fatal(err)
		}
		reopened, err := other.Open("frozen")
		if err != nil {
			t.Fatalf("opening during the handoff: %v", err)
		}
		if err := reopened.Close(); err != nil {
			t.Fatal(err)
		}
	}
	open()
	if err := repository.CancelOriginHandoff(100); err != nil {
		t.Fatal(err)
	}
	open()
	if err := repository.graph.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, ok := repository.graph.Root(graphRootName("ck")); ok {
		t.Fatal("the removed branch's root remains after the handoff")
	}
}

// A publication hands off whole origins, and the shared ledger takes in every
// edge of every object of each, so it publishes too what an object only
// another branch still reaches depends on, before the origin holding it.
func TestPrepareBranchPublicationTakesInWholeOrigins(t *testing.T) {
	manager := openTestManager(t)
	repository, err := manager.CreateWithOrigin("whole", 100)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	write := func(ino uint64, data string) {
		t.Helper()
		if _, _, err := repository.WriteAt(MainBranch, ino, []byte(data), 0); err != nil {
			t.Fatal(err)
		}
	}
	fork := func(child string, working, childWorking uint64) {
		t.Helper()
		if err := repository.ForkWithOrigins(MainBranch, child, working, childWorking); err != nil {
			t.Fatal(err)
		}
	}
	file, err := repository.CreateRegular(MainBranch, RootInode, "f", 0600, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	write(file, "first")
	fork("keep", 101, 102)
	// Origin 101 holds the file's second version ...
	write(file, "other")
	fork("before", 103, 104)
	// ... which origin 103, that another file changed, still names ...
	if _, err := repository.CreateRegular(MainBranch, RootInode, "g", 0600, 0, 0); err != nil {
		t.Fatal(err)
	}
	fork("sibling", 105, 106)
	// ... and main, which changed the file again, no longer reaches.
	write(file, "third")
	fork("publish", 107, 108)
	origins, err := repository.PrepareBranchPublication("publish")
	if err != nil {
		t.Fatal(err)
	}
	position := make(map[ttcsnapshot.SnapshotID]int, len(origins))
	for index, origin := range origins {
		position[origin] = index
	}
	for _, origin := range []ttcsnapshot.SnapshotID{101, 103, 105} {
		if _, ok := position[origin]; !ok {
			t.Fatalf("publication origins %v omit %d", origins, origin)
		}
	}
	if position[101] > position[103] || position[103] > position[105] {
		t.Fatalf("publication origins %v hand an origin off before one it depends on", origins)
	}
}

// A branch's metadata published during a handoff of an origin it shares
// waits for the handoff, as an observation does, rather than failing.
func TestPublishMetadataDuringOriginHandoff(t *testing.T) {
	manager := openTestManager(t)
	repository, err := manager.CreateWithOrigin("publish-handoff", 100)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.CreateRegular(MainBranch, RootInode, "payload", 0600, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := repository.ForkWithOrigins(MainBranch, "peer", 101, 102); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.PrepareBranchPublication("peer"); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.FreezeOriginHandoff(100); err != nil {
		t.Fatal(err)
	}
	// Main's tail, which the handoff of the origin it shares refuses.
	if _, err := repository.CreateRegular(MainBranch, RootInode, "later", 0600, 0, 0); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- repository.PublishMetadata() }()
	select {
	case err := <-done:
		t.Fatalf("PublishMetadata did not wait for the handoff: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := repository.CompleteOriginHandoff(100); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PublishMetadata remained blocked after the handoff completed")
	}
}

func TestBranchDiffDuringOriginHandoff(t *testing.T) {
	manager := openTestManager(t)
	repository, err := manager.CreateWithOrigin("diff-handoff", 100)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	ino, err := repository.CreateRegular(MainBranch, RootInode, "payload", 0600, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.WriteAt(MainBranch, ino, []byte("base"), 0); err != nil {
		t.Fatal(err)
	}
	if err := repository.ForkWithOrigins(MainBranch, "peer", 101, 102); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.WriteAt("peer", ino, []byte("peer"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.PrepareBranchPublication("peer"); err != nil {
		t.Fatal(err)
	}
	root, ok := repository.graph.Root(graphRootName("peer"))
	if !ok {
		t.Fatal("peer graph root is absent")
	}
	if _, err := repository.FreezeOriginHandoff(root.Snapshot); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := repository.BranchDiff("peer")
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("BranchDiff did not wait for handoff completion: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := repository.CompleteOriginHandoff(root.Snapshot); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("BranchDiff remained blocked after handoff completion")
	}
}

func TestOriginDependenciesIncludeNewerCrossOriginChildren(t *testing.T) {
	manager := openTestManager(t)
	repository, err := manager.CreateWithOrigin("origin-dependencies", 100)
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	var child ttcsnapshot.ObjectID
	if err := repository.withLock(func() error {
		if err := repository.graph.AdoptWorking(101); err != nil {
			return err
		}
		var err error
		child, err = repository.graph.CreateObject(101, "construct/dependency-child", ttcsnapshot.ObjectKindFile, nil, nil)
		if err != nil {
			return err
		}
		_, err = repository.graph.CreateObject(100, "construct/dependency-parent", ttcsnapshot.ObjectKindInode, nil, []ttcsnapshot.Edge{{Slot: 0, Child: child}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	dependencies, err := repository.OriginDependencies(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(dependencies) != 1 || dependencies[0] != 101 {
		t.Fatalf("OriginDependencies(100)=%v, want [101]", dependencies)
	}
	if dependencies, err := repository.OriginDependencies(101); err != nil || len(dependencies) != 0 {
		t.Fatalf("OriginDependencies(101)=%v, err=%v, want empty", dependencies, err)
	}
	prepared, err := repository.PrepareRetirementPublication()
	if err != nil {
		t.Fatal(err)
	}
	// The dependency goes first, though no branch reaches the object that
	// depends on it: the origin is handed off whole.
	if len(prepared) != 2 || prepared[0] != 101 || prepared[1] != 100 {
		t.Fatalf("PrepareRetirementPublication()=%v, want [101 100]", prepared)
	}
	for _, origin := range prepared {
		snapshot, ok := repository.graph.Snapshot(origin)
		if !ok || snapshot.State != ttcsnapshot.SnapshotSealedVolatile {
			t.Fatalf("snapshot %d=(%+v,%t), want sealed-volatile", origin, snapshot, ok)
		}
	}
}

func TestMaterializedRemoteMarkerDoesNotPoisonLaterSync(t *testing.T) {
	manager := openTestManager(t)
	repository, err := manager.Create("materialized-marker-sync")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	local := "0000000000000001-02-0000000000000010"
	source := "0000000000000002-02-0000000000000020"
	if err := repository.markRemoteObjectPending(local, source); err != nil {
		t.Fatal(err)
	}
	if err := repository.clearRemoteObjectPending(local); err != nil {
		t.Fatal(err)
	}
	if err := repository.SyncMetadata(); err != nil {
		t.Fatalf("SyncMetadata after materialization: %v", err)
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

	for round := 0; round < 131; round++ {
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
	if err := repository.graph.Reload(); err != nil {
		t.Fatal(err)
	}
	if stats := repository.graph.Stats(); stats.Roots != 1 || stats.Reclaiming != 0 || stats.Snapshots > 2 {
		t.Fatalf("graph after churn=%+v, want the parentless main root, at most two live origins, and no reclaiming objects", stats)
	}
	// Collection checkpoints the graph only once its journal has outgrown the
	// last checkpoint, as other durability boundaries do.
	if info, err := os.Stat(graphJournalPath(t, repository.path)); err != nil || info.Size() >= reclaimJournalBytes {
		t.Fatalf("graph journal after churn: info=%v err=%v, want under %d bytes", info, err, reclaimJournalBytes)
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
}

func TestGarbageCollectSkipsLiveDonatedRepositoryMetadataTail(t *testing.T) {
	manager := openTestManager(t)
	repository, err := manager.Create("live-gc-fence")
	if err != nil {
		t.Fatal(err)
	}
	directory, err := repository.Directory()
	if err != nil {
		t.Fatal(err)
	}
	path := repository.path
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	runtime, err := OpenRepositoryFD(directory, CopyModeFull)
	if err != nil {
		directory.Close()
		t.Fatal(err)
	}
	directory.Close()

	ino, err := runtime.CreateRegular(MainBranch, RootInode, "pending", 0644, 0, 0)
	if err != nil {
		runtime.Close()
		t.Fatal(err)
	}
	if _, _, err := runtime.WriteAt(MainBranch, ino, []byte("still-live"), 0); err != nil {
		runtime.Close()
		t.Fatal(err)
	}
	inode, err := runtime.Inode(MainBranch, ino)
	if err != nil {
		runtime.Close()
		t.Fatal(err)
	}
	objectID, err := ttcsnapshot.ParseObjectID(inode.Object)
	if err != nil {
		runtime.Close()
		t.Fatal(err)
	}
	if _, ok := runtime.graph.Object(objectID); ok {
		runtime.Close()
		t.Fatal("test object was published before the metadata-tail GC check")
	}
	collector, err := manager.Open("live-gc-fence")
	if err != nil {
		runtime.Close()
		t.Fatal(err)
	}
	objectPath := filepath.Join(path, collector.objectPath(inode.Object))
	if err := collector.GarbageCollect(); err != nil {
		collector.Close()
		runtime.Close()
		t.Fatal(err)
	}
	if _, err := os.Stat(objectPath); err != nil {
		collector.Close()
		runtime.Close()
		t.Fatalf("GC removed a live Sentry metadata-tail object: %v", err)
	}
	if got := readAll(t, runtime, MainBranch, ino); got != "still-live" {
		collector.Close()
		runtime.Close()
		t.Fatalf("data after skipped GC=%q", got)
	}
	if err := collector.Close(); err != nil {
		runtime.Close()
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	collector, err = manager.Open("live-gc-fence")
	if err != nil {
		t.Fatal(err)
	}
	defer collector.Close()
	if err := collector.GarbageCollect(); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, collector, MainBranch, ino); got != "still-live" {
		t.Fatalf("published data after runtime close=%q", got)
	}
}

func TestActiveGarbageCollectBoundsDonatedBranchChurn(t *testing.T) {
	manager := openTestManager(t)
	repository, err := manager.Create("active-branch-churn")
	if err != nil {
		t.Fatal(err)
	}
	ino, err := repository.CreateRegular(MainBranch, RootInode, "value", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	write(t, repository, MainBranch, ino, "main", 0)
	directory, err := repository.Directory()
	if err != nil {
		t.Fatal(err)
	}
	path := repository.path
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	runtime, err := OpenRepositoryFD(directory, CopyModeFull)
	directory.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	for round := 0; round < 160; round++ {
		childID := fmt.Sprintf("temporary-%04d", round)
		if err := runtime.Fork(MainBranch, childID); err != nil {
			t.Fatalf("round %d Fork: %v", round, err)
		}
		if _, _, err := runtime.WriteAt(childID, ino, []byte("child"), 0); err != nil {
			t.Fatalf("round %d child WriteAt: %v", round, err)
		}
		child, err := runtime.Inode(childID, ino)
		if err != nil {
			t.Fatal(err)
		}
		childObject := filepath.Join(path, runtime.objectPath(child.Object))
		if err := runtime.RemoveBranchAndCollect(childID); err != nil {
			t.Fatalf("round %d RemoveBranchAndCollect: %v", round, err)
		}
		if _, err := os.Stat(childObject); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("round %d child object survived active collection: %v", round, err)
		}
		if got := readAll(t, runtime, MainBranch, ino); got != "main" {
			t.Fatalf("round %d main data=%q", round, got)
		}
	}

	if stats := runtime.graph.Stats(); stats.Roots != 1 || stats.Reclaiming != 0 || stats.Snapshots > 2 {
		t.Fatalf("active graph after churn=%+v, want the parentless main root, at most two live origins, and no reclaiming objects", stats)
	}
	runtime.metadataMu.Lock()
	metadataEntries := len(runtime.metadata)
	runtime.metadataMu.Unlock()
	if metadataEntries > 1 {
		t.Fatalf("active runtime retained %d historical metadata entries", metadataEntries)
	}
}

func TestActiveGarbageCollectDefersForPeerRuntimeTail(t *testing.T) {
	manager := openTestManager(t)
	repository, err := manager.Create("active-peer-fence")
	if err != nil {
		t.Fatal(err)
	}
	directoryA, err := repository.Directory()
	if err != nil {
		t.Fatal(err)
	}
	directoryB, err := repository.Directory()
	if err != nil {
		t.Fatal(err)
	}
	path := repository.path
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	runtimeA, err := OpenRepositoryFD(directoryA, CopyModeFull)
	directoryA.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer runtimeA.Close()
	runtimeB, err := OpenRepositoryFD(directoryB, CopyModeFull)
	directoryB.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer runtimeB.Close()

	ino, err := runtimeA.CreateRegular(MainBranch, RootInode, "pending", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := runtimeA.WriteAt(MainBranch, ino, []byte("peer-tail"), 0); err != nil {
		t.Fatal(err)
	}
	inode, err := runtimeA.Inode(MainBranch, ino)
	if err != nil {
		t.Fatal(err)
	}
	objectID, err := ttcsnapshot.ParseObjectID(inode.Object)
	if err != nil {
		t.Fatal(err)
	}
	objectPath := filepath.Join(path, "data", "files", fmt.Sprintf("%016x", objectID.Snapshot), fmt.Sprintf("%02x", byte(objectID.Ordinal)), inode.Object+".data")
	collected, err := runtimeB.GarbageCollectActive()
	if err != nil {
		t.Fatal(err)
	}
	if collected {
		t.Fatal("active collection succeeded while a peer runtime held an unpublished metadata tail")
	}
	if _, err := os.Stat(objectPath); err != nil {
		t.Fatalf("deferred active collection removed peer-tail data: %v", err)
	}
	if got := readAll(t, runtimeA, MainBranch, ino); got != "peer-tail" {
		t.Fatalf("peer-tail data=%q", got)
	}
}

func TestMetadataDurabilityFenceReclaimsObsoleteBulkByID(t *testing.T) {
	manager := openTestManager(t)
	repository, err := manager.Create("bounded-bulk-churn")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()

	countBulk := func() int {
		count := 0
		if err := filepath.WalkDir(filepath.Join(repository.path, "data", "files"), func(_ string, entry fs.DirEntry, walkErr error) error {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}
			if walkErr != nil {
				return walkErr
			}
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".data") {
				count++
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return count
	}

	for iteration := 0; iteration < reclaimBatchObjects+100; iteration++ {
		ino, err := repository.CreateRegular(MainBranch, RootInode, "temporary", 0600, 0, 0)
		if err != nil {
			t.Fatalf("iteration %d create: %v", iteration, err)
		}
		if err := repository.Unlink(MainBranch, RootInode, "temporary"); err != nil {
			t.Fatalf("iteration %d unlink: %v", iteration, err)
		}
		if err := repository.ReapOrphan(MainBranch, ino); err != nil {
			t.Fatalf("iteration %d reap: %v", iteration, err)
		}
	}
	if got := countBulk(); got >= reclaimBatchObjects {
		t.Fatalf("automatic durability fence retained %d obsolete objects, want fewer than %d", got, reclaimBatchObjects)
	}
	if err := repository.SyncMetadata(); err != nil {
		t.Fatal(err)
	}
	if got := countBulk(); got != 0 {
		t.Fatalf("explicit durability fence retained %d obsolete objects", got)
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
	wantBytes := uint64(2 * hostarch.PageSize)
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
	page := uint64(hostarch.PageSize)
	if usage.UniqueBytes != 3*page || len(usage.Branches) != 2 {
		t.Fatalf("usage=%+v, want three unique pages and two branches", usage)
	}
	for _, branch := range usage.Branches {
		if branch.PrivateBytes != page || branch.SharedBytes != page || branch.TotalBytes != 2*page {
			t.Fatalf("branch usage=%+v, want one private and one shared page", branch)
		}
	}
}

func TestObservationPinRetainsReplacedRoot(t *testing.T) {
	manager := openTestManager(t)
	repository, err := manager.Create("observed-root")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.CreateRegular(MainBranch, RootInode, "before", 0644, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := repository.SyncMetadata(); err != nil {
		t.Fatal(err)
	}
	roots, pins, err := repository.pinGraphRoots([]string{graphRootName(MainBranch)})
	if err != nil {
		t.Fatal(err)
	}
	oldRoot := roots[graphRootName(MainBranch)]
	if _, err := repository.CreateRegular(MainBranch, RootInode, "after", 0644, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := repository.SyncMetadata(); err != nil {
		t.Fatal(err)
	}
	if err := repository.withLock(func() error {
		_, err := repository.graph.Collect(4096)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.graph.FilesystemRecords(oldRoot); err != nil {
		t.Fatalf("pinned replaced root is not traversable: %v", err)
	}
	if err := repository.releaseGraphObservation(pins); err != nil {
		t.Fatal(err)
	}
	if err := repository.withLock(func() error {
		_, err := repository.graph.Collect(4096)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := repository.graph.Object(oldRoot); ok {
		t.Fatal("released observation root remained reachable after collection")
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

// A live Sentry and ttcd observe a repository through different handles. The
// Sentry keeps a bounded metadata tail in memory, so manager-side inspection
// must not accidentally report the preceding graph root until the next fsync,
// fork, or lifecycle transition. PublishMetadata is the inexpensive
// visibility boundary used before observations such as `ttc diff`.
func TestPublishMetadataMakesLiveTailVisibleToAnotherHandle(t *testing.T) {
	manager := openTestManager(t)
	writer, err := manager.Create("publish-metadata")
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := writer.Fork(MainBranch, "child"); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.CreateRegular("child", RootInode, "visible", 0644, 0, 0); err != nil {
		t.Fatal(err)
	}

	observer, err := manager.Open("publish-metadata")
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	changes, err := observer.BranchDiff("child")
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range changes {
		if change.Path == "/visible" {
			t.Fatalf("unpublished process-local metadata became visible: %+v", changes)
		}
	}

	if err := writer.PublishMetadata(); err != nil {
		t.Fatal(err)
	}
	changes, err = observer.BranchDiff("child")
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range changes {
		if change.Path == "/visible" && change.Kind == "add" && change.Type == "regular" {
			return
		}
	}
	t.Fatalf("published metadata is missing from second handle's diff: %+v", changes)
}

// A handle that made a branch's changes keeps their pointer summary, and
// before a fence their batch, while another process, ttcd, removes the branch
// as an rm does. The handle's next fence must not put either back: the
// summary named a branch without graph roots, which no process could open
// the repository with again, and the batch alone became the branch's root.
func TestBranchRemovedByAnotherHandleStaysRemoved(t *testing.T) {
	for _, published := range []bool{true, false} {
		t.Run(fmt.Sprintf("published=%t", published), func(t *testing.T) {
			manager := openTestManager(t)
			sentry, err := manager.Create("removed-elsewhere")
			if err != nil {
				t.Fatal(err)
			}
			defer sentry.Close()
			if err := sentry.Fork(MainBranch, "child"); err != nil {
				t.Fatal(err)
			}
			if _, err := sentry.CreateRegular("child", RootInode, "file", 0644, 0, 0); err != nil {
				t.Fatal(err)
			}
			if published {
				if err := sentry.PublishMetadata(); err != nil {
					t.Fatal(err)
				}
			}
			daemon, err := manager.Open("removed-elsewhere")
			if err != nil {
				t.Fatal(err)
			}
			defer daemon.Close()
			if err := daemon.RemoveBranch("child"); err != nil {
				t.Fatal(err)
			}
			if err := sentry.SyncMetadata(); err != nil {
				t.Fatalf("fence after another handle removed the branch: %v", err)
			}
			reopened, err := manager.Open("removed-elsewhere")
			if err != nil {
				t.Fatalf("reopening after another handle removed the branch: %v", err)
			}
			defer reopened.Close()
			if _, err := reopened.Branch("child"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("removed branch came back: %v", err)
			}
		})
	}
}

// ttcd forks a branch whose Kernel is gone to publish it, which seals the
// origin the Sentry's handle last wrote the branch at. The Sentry's summary
// must not take the branch back to that origin, and a batch the Sentry still
// had for it must not refuse every later fence of the repository.
func TestBranchForkedByAnotherHandleKeepsItsPointer(t *testing.T) {
	for _, published := range []bool{true, false} {
		t.Run(fmt.Sprintf("published=%t", published), func(t *testing.T) {
			manager := openTestManager(t)
			sentry, err := manager.Create("forked-elsewhere")
			if err != nil {
				t.Fatal(err)
			}
			defer sentry.Close()
			if err := sentry.Fork(MainBranch, "child"); err != nil {
				t.Fatal(err)
			}
			if _, err := sentry.CreateRegular("child", RootInode, "file", 0644, 0, 0); err != nil {
				t.Fatal(err)
			}
			if published {
				if err := sentry.PublishMetadata(); err != nil {
					t.Fatal(err)
				}
			}
			daemon, err := manager.Open("forked-elsewhere")
			if err != nil {
				t.Fatal(err)
			}
			defer daemon.Close()
			if err := daemon.Fork("child", "publication"); err != nil {
				t.Fatal(err)
			}
			forked, err := daemon.Branch("child")
			if err != nil {
				t.Fatal(err)
			}
			if err := sentry.SyncMetadata(); err != nil {
				t.Fatalf("fence after another handle forked the branch: %v", err)
			}
			got, err := daemon.Branch("child")
			if err != nil {
				t.Fatal(err)
			}
			if got.Generation != forked.Generation || got.WorkingSnapshot != forked.WorkingSnapshot {
				t.Fatalf("pointer went back to before another handle's fork: generation %d at origin %d, want %d at %d", got.Generation, got.WorkingSnapshot, forked.Generation, forked.WorkingSnapshot)
			}
			reopened, err := manager.Open("forked-elsewhere")
			if err != nil {
				t.Fatalf("reopening after another handle forked the branch: %v", err)
			}
			defer reopened.Close()
		})
	}
}

// A handle's cache of a branch pointer may vouch, by the file's stat, for a
// record that another handle replaced: Linux's filesystems give a freed inode
// number out again at once and stamp modifications by a coarse clock, so that
// a pointer another handle rewrote twice within a tick stats as the file this
// handle read. A fence must not write such a record back over the other
// handle's fork, as it did on CI's Linux runners.
func TestFenceReadsAPointerItsCacheVouchesFor(t *testing.T) {
	manager := openTestManager(t)
	sentry, err := manager.Create("cache-vouches")
	if err != nil {
		t.Fatal(err)
	}
	defer sentry.Close()
	if err := sentry.Fork(MainBranch, "child"); err != nil {
		t.Fatal(err)
	}
	if _, err := sentry.CreateRegular("child", RootInode, "file", 0644, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := sentry.PublishMetadata(); err != nil {
		t.Fatal(err)
	}
	stale, err := sentry.Branch("child")
	if err != nil {
		t.Fatal(err)
	}
	daemon, err := manager.Open("cache-vouches")
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	if err := daemon.Fork("child", "publication"); err != nil {
		t.Fatal(err)
	}
	forked, err := daemon.Branch("child")
	if err != nil {
		t.Fatal(err)
	}
	// The sentry's cache holds its own record, as if the daemon's pointer
	// file were the one it read.
	var stat unix.Stat_t
	if err := unix.Fstatat(int(sentry.dir.Fd()), filepath.Join("branches", "child.json"), &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		t.Fatal(err)
	}
	sentry.branchMu.Lock()
	sentry.branchCache["child"] = cachedBranch{
		branch: stale, device: uint64(stat.Dev), inode: stat.Ino,
		size: stat.Size, mtimeNsec: stat.Mtim.Sec*1e9 + stat.Mtim.Nsec,
	}
	sentry.branchMu.Unlock()
	if err := sentry.SyncMetadata(); err != nil {
		t.Fatal(err)
	}
	got, err := daemon.Branch("child")
	if err != nil {
		t.Fatal(err)
	}
	if got.Generation != forked.Generation || got.WorkingSnapshot != forked.WorkingSnapshot {
		t.Fatalf("the fence wrote back the pointer of before the daemon's fork: generation %d at origin %d, want %d at %d", got.Generation, got.WorkingSnapshot, forked.Generation, forked.WorkingSnapshot)
	}
}

// Nor does a fork trust such a record (see
// TestFenceReadsAPointerItsCacheVouchesFor): a fork of a branch that another
// handle forked must seal the branch's working origin, not the one that the
// other handle sealed.
func TestForkReadsAPointerItsCacheVouchesFor(t *testing.T) {
	manager := openTestManager(t)
	sentry, err := manager.Create("cache-forks")
	if err != nil {
		t.Fatal(err)
	}
	defer sentry.Close()
	if err := sentry.Fork(MainBranch, "child"); err != nil {
		t.Fatal(err)
	}
	if err := sentry.PublishMetadata(); err != nil {
		t.Fatal(err)
	}
	stale, err := sentry.Branch("child")
	if err != nil {
		t.Fatal(err)
	}
	daemon, err := manager.Open("cache-forks")
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	if err := daemon.Fork("child", "publication"); err != nil {
		t.Fatal(err)
	}
	forked, err := daemon.Branch("child")
	if err != nil {
		t.Fatal(err)
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(int(sentry.dir.Fd()), filepath.Join("branches", "child.json"), &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		t.Fatal(err)
	}
	sentry.branchMu.Lock()
	sentry.branchCache["child"] = cachedBranch{
		branch: stale, device: uint64(stat.Dev), inode: stat.Ino,
		size: stat.Size, mtimeNsec: stat.Mtim.Sec*1e9 + stat.Mtim.Nsec,
	}
	sentry.branchMu.Unlock()
	if err := sentry.Fork("child", "grandchild"); err != nil {
		t.Fatalf("forking a branch that another handle forked: %v", err)
	}
	got, err := daemon.Branch("child")
	if err != nil {
		t.Fatal(err)
	}
	if got.Generation != forked.Generation+1 || got.WorkingSnapshot == forked.WorkingSnapshot {
		t.Fatalf("the fork did not follow the other handle's: generation %d at origin %d, after %d at %d", got.Generation, got.WorkingSnapshot, forked.Generation, forked.WorkingSnapshot)
	}
	reopened, err := manager.Open("cache-forks")
	if err != nil {
		t.Fatalf("reopening after the forks: %v", err)
	}
	defer reopened.Close()
	for _, branch := range []string{"child", "publication", "grandchild"} {
		if _, err := reopened.Branch(branch); err != nil {
			t.Errorf("branch %s: %v", branch, err)
		}
	}
}

func TestDiffBaseIsAnAuthoritativeGraphRoot(t *testing.T) {
	manager := openTestManager(t)
	repository, err := manager.Create("diff-root-lifetime")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	ino, err := repository.CreateRegular(MainBranch, RootInode, "value", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	write(t, repository, MainBranch, ino, "base", 0)
	if err := repository.Fork(MainBranch, "child"); err != nil {
		t.Fatal(err)
	}
	write(t, repository, MainBranch, ino, "main", 0)
	write(t, repository, "child", ino, "child", 0)
	if err := repository.GarbageCollect(); err != nil {
		t.Fatal(err)
	}
	changes, err := repository.BranchDiff("child")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, change := range changes {
		if change.Path == "/value" && change.Kind == "modify" {
			found = true
		}
	}
	if !found {
		t.Fatalf("diff after both current roots advanced=%+v, want /value modification", changes)
	}
	branch, err := repository.Branch("child")
	if err != nil {
		t.Fatal(err)
	}
	base, ok := repository.graph.Root(graphBaseRootName("child"))
	if !ok || branch.BaseRoot != base.String() {
		t.Fatalf("branch base=%q graph base=%v found=%t", branch.BaseRoot, base, ok)
	}
}

func TestForkPreservesSourceDiffParent(t *testing.T) {
	manager := openTestManager(t)
	repository, err := manager.Create("diff-source-parent")
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	ino, err := repository.CreateRegular(MainBranch, RootInode, "value", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	write(t, repository, MainBranch, ino, "base", 0)
	if err := repository.Fork(MainBranch, "child"); err != nil {
		t.Fatal(err)
	}
	write(t, repository, "child", ino, "changed", 0)
	before, err := repository.BranchDiff("child")
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 || before[0].Path != "/value" || before[0].Kind != "modify" {
		t.Fatalf("diff before child fork=%+v, want value modification", before)
	}
	if err := repository.Fork("child", "grandchild"); err != nil {
		t.Fatal(err)
	}
	after, err := repository.BranchDiff("child")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("fork changed source diff from %+v to %+v", before, after)
	}
	grandchild, err := repository.BranchDiff("grandchild")
	if err != nil {
		t.Fatal(err)
	}
	if len(grandchild) != 0 {
		t.Fatalf("new child diff=%+v, want empty fork-point delta", grandchild)
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

// A runtime deleting a branch marks it discarding first: its teardown then
// reaps no orphans one by one, as the branch goes whole. The mark ends with
// the removal, whether or not that succeeds, so a new branch of the same name
// reaps its orphans again.
func TestDiscardedBranchGoesWhole(t *testing.T) {
	manager := openTestManager(t)
	repository, err := manager.Create("discard")
	if err != nil {
		t.Fatal(err)
	}
	directory, err := repository.Directory()
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	runtime, err := OpenRepositoryFD(directory, CopyModeFull)
	directory.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	orphan := func() uint64 {
		t.Helper()
		if err := runtime.Fork(MainBranch, "doomed"); err != nil {
			t.Fatal(err)
		}
		ino, err := runtime.CreateRegular("doomed", RootInode, "open-and-unlinked", 0644, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		write(t, runtime, "doomed", ino, "orphaned", 0)
		if err := runtime.Unlink("doomed", RootInode, "open-and-unlinked"); err != nil {
			t.Fatal(err)
		}
		return ino
	}
	ino := orphan()
	runtime.DiscardBranch("doomed")
	if err := runtime.ReapOrphan("doomed", ino); err != nil {
		t.Fatal(err)
	}
	if inode, err := runtime.Inode("doomed", ino); err != nil || inode.Nlink != 0 {
		t.Fatalf("a discarding branch's orphan: %+v, %v; want it left for the removal", inode, err)
	}
	if err := runtime.RemoveBranchAndCollect("doomed"); err != nil {
		t.Fatal(err)
	}
	if runtime.Discarding("doomed") {
		t.Fatal("the removed branch is still discarding")
	}
	if _, err := runtime.Branch("doomed"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed branch: %v", err)
	}
	ino = orphan()
	if err := runtime.ReapOrphan("doomed", ino); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Inode("doomed", ino); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the reused name's orphan was not reaped: %v", err)
	}

	// A path-backed repository cannot collect as a runtime does: the removal
	// fails, and the mark ends all the same.
	manager2 := openTestManager(t)
	path, err := manager2.Create("discard-path")
	if err != nil {
		t.Fatal(err)
	}
	defer path.Close()
	if err := path.Fork(MainBranch, "doomed"); err != nil {
		t.Fatal(err)
	}
	path.DiscardBranch("doomed")
	if err := path.RemoveBranchAndCollect("doomed"); err == nil {
		t.Fatal("a path-backed repository collected as a runtime")
	}
	if path.Discarding("doomed") {
		t.Fatal("a failed removal left the branch discarding")
	}
}

func TestRandomForkHardlinkOrphanAndNameReuse(t *testing.T) {
	type branchModel struct {
		entries map[string]uint64
		data    map[uint64]string
	}
	cloneModel := func(source *branchModel) *branchModel {
		clone := &branchModel{entries: make(map[string]uint64, len(source.entries)), data: make(map[uint64]string, len(source.data))}
		for name, ino := range source.entries {
			clone.entries[name] = ino
		}
		for ino, contents := range source.data {
			clone.data[ino] = contents
		}
		return clone
	}
	sortedKeys := func(values map[string]uint64) []string {
		result := make([]string, 0, len(values))
		for value := range values {
			result = append(result, value)
		}
		sort.Strings(result)
		return result
	}
	sortedBranches := func(values map[string]*branchModel) []string {
		result := make([]string, 0, len(values))
		for value := range values {
			result = append(result, value)
		}
		sort.Strings(result)
		return result
	}
	linkCount := func(model *branchModel, ino uint64) uint32 {
		var count uint32
		for _, linked := range model.entries {
			if linked == ino {
				count++
			}
		}
		return count
	}

	root := t.TempDir()
	manager, err := OpenManager(root)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := manager.Create("random-semantics")
	if err != nil {
		t.Fatal(err)
	}
	models := map[string]*branchModel{MainBranch: {entries: make(map[string]uint64), data: make(map[uint64]string)}}
	var random uint64 = 0x92346871da05bc4f
	nextRandom := func(limit int) int {
		random ^= random << 13
		random ^= random >> 7
		random ^= random << 17
		return int(random % uint64(limit))
	}
	validate := func(step int) {
		for branch, model := range models {
			for name, wantIno := range model.entries {
				gotIno, err := repository.Lookup(branch, RootInode, name)
				if err != nil || gotIno != wantIno {
					t.Fatalf("step %d %s/%s lookup=(%d,%v), want %d", step, branch, name, gotIno, err, wantIno)
				}
			}
			for ino, wantData := range model.data {
				inode, err := repository.Inode(branch, ino)
				if err != nil {
					t.Fatalf("step %d %s inode %d: %v", step, branch, ino, err)
				}
				if wantLinks := linkCount(model, ino); inode.Nlink != wantLinks {
					t.Fatalf("step %d %s inode %d links=%d, want %d", step, branch, ino, inode.Nlink, wantLinks)
				}
				if got := readAll(t, repository, branch, ino); got != wantData {
					t.Fatalf("step %d %s inode %d data=%q, want %q", step, branch, ino, got, wantData)
				}
			}
		}
		if err := repository.graph.Verify(); err != nil {
			t.Fatalf("step %d graph: %v", step, err)
		}
	}

	for step := 0; step < 300; step++ {
		branches := sortedBranches(models)
		branch := branches[nextRandom(len(branches))]
		model := models[branch]
		switch nextRandom(8) {
		case 0:
			name := fmt.Sprintf("file-%02d", nextRandom(24))
			if _, exists := model.entries[name]; !exists {
				ino, err := repository.CreateRegular(branch, RootInode, name, 0644, 0, 0)
				if err != nil {
					t.Fatalf("step %d create: %v", step, err)
				}
				model.entries[name] = ino
				model.data[ino] = ""
			}
		case 1:
			if len(model.data) != 0 {
				var inodes []uint64
				for ino := range model.data {
					inodes = append(inodes, ino)
				}
				sort.Slice(inodes, func(i, j int) bool { return inodes[i] < inodes[j] })
				ino := inodes[nextRandom(len(inodes))]
				value := fmt.Sprintf("%08x", step)
				write(t, repository, branch, ino, value, 0)
				model.data[ino] = value
			}
		case 2:
			entries := sortedKeys(model.entries)
			name := fmt.Sprintf("link-%02d", nextRandom(24))
			if len(entries) != 0 {
				if _, exists := model.entries[name]; !exists {
					ino := model.entries[entries[nextRandom(len(entries))]]
					if err := repository.Link(branch, RootInode, name, ino); err != nil {
						t.Fatalf("step %d link: %v", step, err)
					}
					model.entries[name] = ino
				}
			}
		case 3:
			entries := sortedKeys(model.entries)
			if len(entries) != 0 {
				name := entries[nextRandom(len(entries))]
				if err := repository.Unlink(branch, RootInode, name); err != nil {
					t.Fatalf("step %d unlink: %v", step, err)
				}
				delete(model.entries, name)
			}
		case 4:
			var orphans []uint64
			for ino := range model.data {
				if linkCount(model, ino) == 0 {
					orphans = append(orphans, ino)
				}
			}
			if len(orphans) != 0 {
				sort.Slice(orphans, func(i, j int) bool { return orphans[i] < orphans[j] })
				ino := orphans[nextRandom(len(orphans))]
				if err := repository.ReapOrphan(branch, ino); err != nil {
					t.Fatalf("step %d reap: %v", step, err)
				}
				delete(model.data, ino)
			}
		case 5:
			child := fmt.Sprintf("branch-%02d", nextRandom(16))
			if _, exists := models[child]; !exists {
				if err := repository.Fork(branch, child); err != nil {
					t.Fatalf("step %d fork: %v", step, err)
				}
				models[child] = cloneModel(model)
			}
		case 6:
			if len(models) > 1 {
				if err := repository.RemoveBranch(branch); err != nil {
					t.Fatalf("step %d remove branch: %v", step, err)
				}
				delete(models, branch)
				if err := repository.GarbageCollect(); err != nil {
					t.Fatalf("step %d collect: %v", step, err)
				}
			}
		case 7:
			entries := sortedKeys(model.entries)
			newName := fmt.Sprintf("renamed-%02d", nextRandom(24))
			if len(entries) != 0 {
				if _, exists := model.entries[newName]; !exists {
					oldName := entries[nextRandom(len(entries))]
					ino := model.entries[oldName]
					if err := repository.Rename(branch, RootInode, oldName, RootInode, newName, true); err != nil {
						t.Fatalf("step %d rename: %v", step, err)
					}
					delete(model.entries, oldName)
					model.entries[newName] = ino
				}
			}
		}
		validate(step)
		if step%61 == 60 {
			if err := repository.Close(); err != nil {
				t.Fatal(err)
			}
			repository, err = manager.Open("random-semantics")
			if err != nil {
				t.Fatalf("step %d reopen: %v", step, err)
			}
			validate(step)
		}
	}
	if err := repository.GarbageCollect(); err != nil {
		t.Fatal(err)
	}
	validate(300)
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
}

// What a graph references of an origin is read again once the graph changes,
// and only then.
func TestReadOriginReferencesFollowsTheGraph(t *testing.T) {
	path := t.TempDir()
	dir := filepath.Join(path, "graph")
	change := func(f func(*ttcsnapshot.Store)) {
		t.Helper()
		graph, err := ttcsnapshot.OpenExternallyCoordinated(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := graph.BeginExternalTransaction(); err != nil {
			t.Fatal(err)
		}
		f(graph)
		if err := graph.EndExternalTransaction(); err != nil {
			t.Fatal(err)
		}
		if err := graph.Close(); err != nil {
			t.Fatal(err)
		}
	}
	var origin ttcsnapshot.SnapshotID
	change(func(graph *ttcsnapshot.Store) {
		var err error
		if origin, err = graph.AllocateWorking(); err != nil {
			t.Fatal(err)
		}
		if _, err := graph.CreateObject(origin, "keep", ttcsnapshot.ObjectKindMetadata, nil, nil); err != nil {
			t.Fatal(err)
		}
	})
	for i := 0; i < 2; i++ {
		references, _, _, err := ReadOriginReferences(path, origin)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := references["pin/keep"]; !ok || len(references) != 1 {
			t.Fatalf("read %d: references %v", i, references)
		}
	}
	change(func(graph *ttcsnapshot.Store) {
		if err := graph.DeletePin("keep"); err != nil {
			t.Fatal(err)
		}
	})
	references, _, _, err := ReadOriginReferences(path, origin)
	if err != nil {
		t.Fatal(err)
	}
	if len(references) != 0 {
		t.Fatalf("after the pin went, references %v", references)
	}
}
