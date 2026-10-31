// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package fs_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/fdbudget"
	"gvisor.dev/gvisor/pkg/lisafs"
	"gvisor.dev/gvisor/pkg/lisafs/testsuite"
	"gvisor.dev/gvisor/runsc/gofer/fs"
)

func init() {
	if err := fs.OpenProcSelfFD("/proc/self/fd"); err != nil {
		panic(err)
	}
}

// openFDs counts this process's open descriptors.
func openFDs(t *testing.T) int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("reading /proc/self/fd: %v", err)
	}
	return len(entries)
}

// held is a file that the client holds, as a Sentry holds one a guest has
// open: its control FD and a read-only open FD.
type held struct {
	name    string
	control lisafs.ClientFD
	open    lisafs.ClientFD
	ino     uint64
}

func contents(i int) []byte {
	return []byte(fmt.Sprintf("file %d of the bind mount", i))
}

// evictAll has the budget close every descriptor that nothing is using.
func evictAll() {
	for fdbudget.Retry(unix.EMFILE) {
	}
}

func readAndCompare(ctx context.Context, t *testing.T, fd lisafs.ClientFD, want []byte) {
	t.Helper()
	buf := make([]byte, len(want)+16)
	n, err := fd.Read(ctx, buf, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(buf[:n], want) {
		t.Fatalf("read %q, want %q", buf[:n], want)
	}
}

// A client holds many more of a mount's files than the gofer may have
// descriptors, as a guest may hold a bind mount's files open, and every
// operation on them works: the gofer closes those that are idle and opens
// them again when they are used, from their paths, and refuses a path that
// no longer leads to the file it had.
func testHoldingMoreFilesThanDescriptors(ctx context.Context, t *testing.T, root lisafs.ClientFD, mountPath string) {
	const files = 600
	for i := 0; i < files; i++ {
		if err := os.WriteFile(filepath.Join(mountPath, fmt.Sprintf("f%d", i)), contents(i), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// A limit well below what holding every file would take, and the budget
	// that goes with it.
	var saved unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &saved); err != nil {
		t.Fatal(err)
	}
	limit := uint64(openFDs(t) + 200)
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &unix.Rlimit{Cur: limit, Max: saved.Max}); err != nil {
		t.Fatal(err)
	}
	defer unix.Setrlimit(unix.RLIMIT_NOFILE, &saved)
	fdbudget.Init()
	evictedBefore, reopenedBefore := fdbudget.Stats()

	hold := make([]held, files)
	for i := range hold {
		name := fmt.Sprintf("f%d", i)
		ino, err := root.Walk(ctx, name)
		if err != nil {
			t.Fatalf("walking to %s: %v", name, err)
		}
		control := root.Client().NewFD(ino.ControlFD)
		openFD, hostFD, err := control.OpenAt(ctx, unix.O_RDONLY)
		if err != nil {
			t.Fatalf("opening %s: %v", name, err)
		}
		// The Sentry's side of donated descriptors is not this test's.
		if hostFD >= 0 {
			unix.Close(hostFD)
		}
		hold[i] = held{name: name, control: control, open: root.Client().NewFD(openFD), ino: ino.Stat.Ino}
	}
	if n := uint64(openFDs(t)); n >= limit {
		t.Fatalf("%d descriptors open holding %d files, at a limit of %d", n, files, limit)
	}

	// Everything still works, the closed descriptors opened again.
	for i, h := range hold {
		readAndCompare(ctx, t, h.open, contents(i))
		var stat lisafs.Statx
		if err := h.control.StatTo(ctx, &stat); err != nil {
			t.Fatalf("stat of %s: %v", h.name, err)
		}
		if stat.Ino != h.ino {
			t.Fatalf("stat of %s is of inode %d, want %d", h.name, stat.Ino, h.ino)
		}
	}
	evicted, reopened := fdbudget.Stats()
	if evicted == evictedBefore || reopened == reopenedBefore {
		t.Errorf("the budget closed %d descriptors and saw %d opened again; want both", evicted-evictedBefore, reopened-reopenedBefore)
	}

	// A file the client removes while it holds it stays readable: its
	// descriptor is opened again before its path goes.
	evictAll()
	if err := root.UnlinkAt(ctx, hold[0].name, 0); err != nil {
		t.Fatalf("unlinking %s: %v", hold[0].name, err)
	}
	evictAll()
	readAndCompare(ctx, t, hold[0].open, contents(0))
	var stat lisafs.Statx
	if err := hold[0].control.StatTo(ctx, &stat); err != nil || stat.Nlink != 0 {
		t.Errorf("stat of the removed %s: nlink %d, %v; want 0, nil", hold[0].name, stat.Nlink, err)
	}

	// A file renamed through the gofer is opened again at its new path.
	evictAll()
	if err := root.RenameAt(ctx, hold[1].name, root.ID(), "renamed", 0); err != nil {
		t.Fatalf("renaming %s: %v", hold[1].name, err)
	}
	evictAll()
	readAndCompare(ctx, t, hold[1].open, contents(1))

	// So is one renamed over another that is held, which keeps its file.
	evictAll()
	if err := root.RenameAt(ctx, hold[2].name, root.ID(), hold[3].name, 0); err != nil {
		t.Fatalf("renaming %s over %s: %v", hold[2].name, hold[3].name, err)
	}
	evictAll()
	readAndCompare(ctx, t, hold[2].open, contents(2))
	readAndCompare(ctx, t, hold[3].open, contents(3))

	// A file that something else replaces while its descriptor is closed is
	// not the file the client holds: the gofer refuses it rather than serve
	// another.
	evictAll()
	other := filepath.Join(mountPath, "other")
	if err := os.WriteFile(other, []byte("another file"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(other, filepath.Join(mountPath, hold[4].name)); err != nil {
		t.Fatal(err)
	}
	if err := hold[4].control.StatTo(ctx, &stat); !linuxerr.Equals(linuxerr.ESTALE, err) {
		t.Errorf("stat of %s, which was replaced: %v, want ESTALE", hold[4].name, err)
	}

	for _, h := range hold {
		h.open.Close(ctx, false /* flush */)
		h.control.Close(ctx, false /* flush */)
	}
}

// A directory on the way to a file whose descriptor is closed is walked again
// without following a symbolic link that now stands in its place.
func testReopeningFollowsNoSymlink(ctx context.Context, t *testing.T, root lisafs.ClientFD, mountPath string) {
	if err := os.MkdirAll(filepath.Join(mountPath, "a/b"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mountPath, "a/b/leaf"), contents(0), 0644); err != nil {
		t.Fatal(err)
	}
	status, inodes, err := root.WalkMultiple(ctx, []string{"a", "b", "leaf"})
	if err != nil || status != lisafs.WalkSuccess || len(inodes) != 3 {
		t.Fatalf("walking to a/b/leaf: %v, %v, %d inodes", err, status, len(inodes))
	}
	var fds []lisafs.ClientFD
	for _, ino := range inodes {
		fds = append(fds, root.Client().NewFD(ino.ControlFD))
	}
	defer func() {
		for _, fd := range fds {
			fd.Close(ctx, false /* flush */)
		}
	}()
	leaf := fds[2]

	evictAll()
	var stat lisafs.Statx
	if err := leaf.StatTo(ctx, &stat); err != nil || stat.Ino != inodes[2].Stat.Ino {
		t.Fatalf("stat of a/b/leaf, opened again: inode %d, %v; want %d", stat.Ino, err, inodes[2].Stat.Ino)
	}

	// The same file, reached through a symbolic link where a was.
	if err := os.Rename(filepath.Join(mountPath, "a"), filepath.Join(mountPath, "elsewhere")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere", filepath.Join(mountPath, "a")); err != nil {
		t.Fatal(err)
	}
	evictAll()
	if err := leaf.StatTo(ctx, &stat); !linuxerr.Equals(linuxerr.ESTALE, err) {
		t.Errorf("stat of a/b/leaf through a symbolic link: %v, want ESTALE", err)
	}
}

func TestBudget(t *testing.T) {
	for _, test := range []struct {
		name string
		fn   func(context.Context, *testing.T, lisafs.ClientFD, string)
	}{
		{"HoldingMoreFilesThanDescriptors", testHoldingMoreFilesThanDescriptors},
		{"ReopeningFollowsNoSymlink", testReopeningFollowsNoSymlink},
	} {
		mountPath, err := os.MkdirTemp(os.Getenv("TEST_TMPDIR"), "")
		if err != nil {
			t.Fatal(err)
		}
		testsuite.RunTest(t, budgetTester{}, test.name, func(ctx context.Context, t *testing.T, _ testsuite.Tester, root lisafs.ClientFD) {
			test.fn(ctx, t, root, mountPath)
		}, mountPath)
		os.RemoveAll(mountPath)
	}
}

// budgetTester implements testsuite.Tester.
type budgetTester struct{}

// NewConnImpl implements testsuite.Tester.NewConnImpl.
func (budgetTester) NewConnImpl(t *testing.T) lisafs.ConnectionImpl {
	return fs.NewConnectionImpl(&fs.Config{})
}

// LinkSupported implements testsuite.Tester.LinkSupported.
func (budgetTester) LinkSupported() bool { return true }

// SetUserGroupIDSupported implements testsuite.Tester.SetUserGroupIDSupported.
func (budgetTester) SetUserGroupIDSupported() bool { return true }

// BindSupported implements testsuite.Tester.BindSupported.
func (budgetTester) BindSupported() bool { return false }
