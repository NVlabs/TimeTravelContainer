// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcsnapshot

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestHostTreeImportMaterializeRoundTrip(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "source")
	if err := os.MkdirAll(filepath.Join(source, "a", "b"), 0750); err != nil {
		t.Fatal(err)
	}
	contents := bytes.Repeat([]byte("whole-file-cow\x00"), 10000)
	first := filepath.Join(source, "a", "file")
	if err := os.WriteFile(first, contents, 06750); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(first, filepath.Join(source, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a/file", filepath.Join(source, "symlink")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(source, "pipe"), 0640); err != nil {
		t.Fatal(err)
	}
	xattr := []byte{0, 1, 2, 0xff}
	if err := unix.Setxattr(first, "user.ttc-test", xattr, 0); err != nil {
		t.Fatal(err)
	}

	graph, err := Open(filepath.Join(directory, "graph"))
	if err != nil {
		t.Fatal(err)
	}
	defer graph.Close()
	data, err := OpenLocalDataStore(filepath.Join(directory, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	origin, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	root, err := ImportDirectory(context.Background(), graph, data, origin, "image/rootfs", source)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.Verify(); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(directory, "destination")
	if err := MaterializeFilesystem(context.Background(), graph, data, root, destination); err != nil {
		t.Fatal(err)
	}
	resolvedDestination := filepath.Join(directory, "resolved-destination")
	resolved := 0
	if err := MaterializeFilesystemFrom(context.Background(), graph, func(_ context.Context, id ObjectID, length int64) (io.ReadCloser, error) {
		resolved++
		return data.Open(id, length)
	}, root, resolvedDestination); err != nil {
		t.Fatal(err)
	}
	if resolved != 1 {
		t.Fatalf("resolver opened %d bulk objects, want 1 hardlink-shared file", resolved)
	}
	if got, err := os.ReadFile(filepath.Join(resolvedDestination, "hardlink")); err != nil || !bytes.Equal(got, contents) {
		t.Fatalf("resolver-backed materialization=%q err=%v", got, err)
	}
	got, err := os.ReadFile(filepath.Join(destination, "a", "file"))
	if err != nil || !bytes.Equal(got, contents) {
		t.Fatalf("materialized file length=%d err=%v", len(got), err)
	}
	firstInfo, err := os.Stat(filepath.Join(destination, "a", "file"))
	if err != nil {
		t.Fatal(err)
	}
	linkedInfo, err := os.Stat(filepath.Join(destination, "hardlink"))
	if err != nil {
		t.Fatal(err)
	}
	firstStat := firstInfo.Sys().(*syscall.Stat_t)
	linkedStat := linkedInfo.Sys().(*syscall.Stat_t)
	if firstStat.Dev != linkedStat.Dev || firstStat.Ino != linkedStat.Ino {
		t.Fatal("hardlink identity was not preserved")
	}
	if target, err := os.Readlink(filepath.Join(destination, "symlink")); err != nil || target != "a/file" {
		t.Fatalf("symlink target=%q err=%v", target, err)
	}
	pipeInfo, err := os.Lstat(filepath.Join(destination, "pipe"))
	if err != nil || pipeInfo.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("FIFO mode=%v err=%v", pipeInfo, err)
	}
	value := make([]byte, len(xattr))
	if n, err := unix.Getxattr(filepath.Join(destination, "a", "file"), "user.ttc-test", value); err != nil || n != len(value) || !bytes.Equal(value, xattr) {
		t.Fatalf("xattr=%v n=%d err=%v", value, n, err)
	}
}

func TestHostTreeImportBatchesLargeDirectory(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 600; index++ {
		name := filepath.Join(source, fmt.Sprintf("file-%04d", index))
		if err := os.WriteFile(name, []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	graph, err := Open(filepath.Join(directory, "graph"))
	if err != nil {
		t.Fatal(err)
	}
	defer graph.Close()
	data, _ := OpenLocalDataStore(filepath.Join(directory, "objects"))
	origin, _ := graph.AllocateWorking()
	root, err := ImportDirectory(context.Background(), graph, data, origin, "large", source)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := graph.FilesystemDirectory(root, 1)
	if err != nil || len(entries) != 600 {
		t.Fatalf("large directory entries=%d err=%v", len(entries), err)
	}
}
