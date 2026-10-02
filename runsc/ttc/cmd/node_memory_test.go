// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

// testMemoryFile returns a memory file with size bytes allocated.
func testMemoryFile(t *testing.T, size int64) *os.File {
	t.Helper()
	fd, err := unix.MemfdCreate("ttc-test-memory", unix.MFD_CLOEXEC)
	if err != nil {
		t.Skipf("memfd_create: %v", err)
	}
	file := os.NewFile(uintptr(fd), "ttc-test-memory")
	t.Cleanup(func() { file.Close() })
	if err := unix.Fallocate(fd, 0, 0, size); err != nil {
		t.Skipf("fallocate on a memory file: %v", err)
	}
	return file
}

func TestAddMemoryFilesCountsEachFileOnce(t *testing.T) {
	const size = 8 << 20
	file := testMemoryFile(t, size)
	dup, err := unix.Dup(int(file.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(dup)
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		t.Fatal(err)
	}
	key := memoryFileKey{dev: uint64(stat.Dev), ino: stat.Ino}

	files := make(map[memoryFileKey]uint64)
	kinds := addMemoryFiles(os.Getpid(), nil, files)
	if files[key] != size {
		t.Fatalf("memory file counted as %d bytes, want %d", files[key], size)
	}
	if !kinds[strconv.Itoa(int(file.Fd()))] || !kinds[strconv.Itoa(dup)] {
		t.Fatalf("descriptors %d and %d are not known as memory files: %v", file.Fd(), dup, kinds)
	}
	if kinds["0"] || kinds["1"] || kinds["2"] {
		t.Fatalf("standard descriptors are known as memory files: %v", kinds)
	}

	// The next measurement, which knows the descriptors, sees the file grow.
	if err := unix.Fallocate(int(file.Fd()), 0, 0, 2*size); err != nil {
		t.Fatal(err)
	}
	files = make(map[memoryFileKey]uint64)
	addMemoryFiles(os.Getpid(), kinds, files)
	if files[key] != 2*size {
		t.Fatalf("grown memory file counted as %d bytes, want %d", files[key], 2*size)
	}
}

// startTestRuntimeProcess starts a process whose argv names name, which holds
// file open until the test ends.
func startTestRuntimeProcess(t *testing.T, name string, file *os.File) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "read line; exit 0", name)
	cmd.ExtraFiles = []*os.File{file}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stdin.Close()
		if err := cmd.Wait(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	})
}

func TestNodeMemoryCountsMemoryFilesOnce(t *testing.T) {
	const size = 16 << 20
	root := t.TempDir()
	file := testMemoryFile(t, size)
	// Two processes of one runtime hold the same memory file, as a Sentry and
	// the systrap stubs it forks do.
	startTestRuntimeProcess(t, filepath.Join(root, "runtime-a"), file)
	startTestRuntimeProcess(t, filepath.Join(root, "runtime-a-stub"), file)

	held, err := runtimeMemoryBytes(root, []string{"runtime-a", "runtime-b"})
	if err != nil {
		t.Fatal(err)
	}
	if held["runtime-a"] < size || held["runtime-a"] >= 2*size {
		t.Errorf("runtime-a holds %d bytes, want its memory file's %d once and its processes' own", held["runtime-a"], size)
	}
	if held["runtime-b"] != 0 {
		t.Errorf("runtime-b, which has no process, holds %d bytes", held["runtime-b"])
	}

	total, err := currentNodeMemoryBytes(root)
	if err != nil {
		t.Fatal(err)
	}
	self, err := processResidentMemoryBytes(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	// What the node holds beyond the daemon, here the test, is the memory
	// file, once, and its processes' anonymous memory, a little.
	if total < self+size-(4<<20) || total >= self+2*size {
		t.Errorf("node holds %d bytes beside the daemon's %d, want the memory file's %d once", total-self, self, size)
	}
}
