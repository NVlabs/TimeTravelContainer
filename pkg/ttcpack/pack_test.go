// Copyright 2026 The gVisor Authors.
// Licensed under the Apache License, Version 2.0 (the "License");

package ttcpack

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPackRoundTripAndRangeIndex(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(filepath.Join(source, "a", "b"), 0750); err != nil {
		t.Fatal(err)
	}
	contents := bytes.Repeat([]byte("ttc-range-data-"), 150000)
	if err := os.WriteFile(filepath.Join(source, "a", "b", "large"), contents, 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(source, "a", "b", "large"), filepath.Join(source, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a/b/large", filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "empty"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(source, "a", "b", "large"), 0440); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(source, "a", "b"), 0550); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(source, "a", "b"), 0750)
	})
	repository := filepath.Join(root, "repository")
	id, err := WriteDirectory(source, repository)
	if err != nil {
		t.Fatal(err)
	}
	file, index, err := Open(repository, id)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	extents := 0
	for _, entry := range index.Entries {
		if entry.Kind == "file" && entry.Path == "a/b/large" {
			extents++
			if entry.RecordOffset%Alignment != 0 || entry.Length > MaxDataExtent {
				t.Fatalf("invalid extent: %+v", entry)
			}
		}
	}
	if extents < 2 {
		t.Fatalf("large file has %d extents, want multiple", extents)
	}
	destination := filepath.Join(root, "destination")
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(destination, "a", "b"), 0750)
	})
	if err := Materialize(repository, id, destination); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(destination, "a", "b", "large"))
	if err != nil || !bytes.Equal(got, contents) {
		t.Fatalf("round trip mismatch: err=%v len=%d", err, len(got))
	}
	if info, err := os.Stat(filepath.Join(destination, "a", "b")); err != nil || info.Mode().Perm() != 0550 {
		t.Fatalf("restrictive directory mode = %v, %v; want 0550", info, err)
	}
	if info, err := os.Stat(filepath.Join(destination, "a", "b", "large")); err != nil || info.Mode().Perm() != 0440 {
		t.Fatalf("restrictive file mode = %v, %v; want 0440", info, err)
	}
	first, _ := os.Stat(filepath.Join(destination, "a", "b", "large"))
	second, _ := os.Stat(filepath.Join(destination, "hardlink"))
	if !os.SameFile(first, second) {
		t.Fatal("hardlink identity was not preserved")
	}
	if info, err := os.Stat(filepath.Join(destination, "empty")); err != nil || info.Size() != 0 {
		t.Fatalf("empty file round trip: info=%v err=%v", info, err)
	}
}

func TestMaterializeMissingFillsAncestorObjectsWithoutReplacingMetadata(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	newest := filepath.Join(root, "newest")
	ancestor := filepath.Join(root, "ancestor")
	for _, dir := range []string{newest, ancestor} {
		if err := os.MkdirAll(filepath.Join(dir, "objects"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(newest, "metadata.json"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newest, "objects", "changed"), []byte("new object"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ancestor, "metadata.json"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ancestor, "objects", "unchanged"), []byte("old object"), 0600); err != nil {
		t.Fatal(err)
	}
	newestID, err := WriteDirectory(newest, repository)
	if err != nil {
		t.Fatal(err)
	}
	ancestorID, err := WriteDirectory(ancestor, repository)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "destination")
	if err := Materialize(repository, newestID, destination); err != nil {
		t.Fatal(err)
	}
	if err := MaterializeMissing(repository, ancestorID, destination); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(destination, "metadata.json")); err != nil || string(got) != "new" {
		t.Fatalf("metadata = %q, %v; want newest", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(destination, "objects", "unchanged")); err != nil || string(got) != "old object" {
		t.Fatalf("ancestor object = %q, %v", got, err)
	}
}

func TestDirectoryReadsNewestLogicalFileByRange(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	ancestor := filepath.Join(root, "ancestor")
	newest := filepath.Join(root, "newest")
	for _, dir := range []string{ancestor, newest} {
		if err := os.MkdirAll(filepath.Join(dir, "objects"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	old := bytes.Repeat([]byte{0x41}, int(MaxDataExtent)+16384)
	changed := bytes.Repeat([]byte{0x42}, 65536)
	if err := os.WriteFile(filepath.Join(ancestor, "objects", "shared"), old, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ancestor, "objects", "changed"), old, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newest, "objects", "changed"), changed, 0600); err != nil {
		t.Fatal(err)
	}
	newestID, err := WriteDirectory(newest, repository)
	if err != nil {
		t.Fatal(err)
	}
	ancestorID, err := WriteDirectory(ancestor, repository)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := OpenDirectory(repository, []ID{newestID, ancestorID})
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	buffer := make([]byte, 16384)
	if n, err := directory.ReadAt("objects/shared", buffer, MaxDataExtent-4096); err != nil || n != len(buffer) || !bytes.Equal(buffer, old[MaxDataExtent-4096:MaxDataExtent-4096+int64(len(buffer))]) {
		t.Fatalf("cross-extent read = %d, %v", n, err)
	}
	if n, err := directory.ReadAt("objects/changed", buffer[:4096], 0); err != nil || n != 4096 || !bytes.Equal(buffer[:4096], changed[:4096]) {
		t.Fatalf("newest read = %d, %v", n, err)
	}
}

func TestMaterializeMetadataOmitsObjectData(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(filepath.Join(source, "objects"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "metadata.json"), []byte("metadata"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "objects", "object"), bytes.Repeat([]byte{1}, 1<<20), 0600); err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(root, "repository")
	id, err := WriteDirectory(source, repository)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "destination")
	if err := MaterializeMetadata(repository, id, destination, "objects"); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(destination, "metadata.json")); err != nil || string(got) != "metadata" {
		t.Fatalf("metadata = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(destination, "objects", "object")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("object data unexpectedly materialized: %v", err)
	}
}

func TestLazyDirectoryPopulatesWholeFileAfterCrashAndConcurrency(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0755); err != nil {
		t.Fatal(err)
	}
	contents := bytes.Repeat([]byte("whole-file-lazy-data"), 100000)
	file := filepath.Join(source, "large")
	if err := os.WriteFile(file, contents, 0440); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(file, filepath.Join(source, "alias")); err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(root, "repository")
	id, err := WriteDirectory(source, repository)
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(root, "cache")
	rootfs := filepath.Join(cacheDir, "rootfs")
	if err := MaterializeSkeleton(repository, id, rootfs, nil); err != nil {
		t.Fatal(err)
	}
	if err := WriteLazyDescriptor(rootfs, repository, id, ""); err != nil {
		t.Fatal(err)
	}
	// Emulate a gofer crash after a partial write but before its completion
	// record. The retry must truncate and reconstruct the immutable file.
	if err := os.WriteFile(filepath.Join(rootfs, "large"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	descriptor, err := ReadLazyDescriptor(rootfs)
	if err != nil {
		t.Fatal(err)
	}
	lazy, err := OpenLazyDirectory(rootfs, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	packFD, rootFD, stateFD, prefix := lazy.InheritedFDs()
	packFD, err = unix.Dup(packFD)
	if err != nil {
		t.Fatal(err)
	}
	rootFD, err = unix.Dup(rootFD)
	if err != nil {
		t.Fatal(err)
	}
	stateFD, err = unix.Dup(stateFD)
	if err != nil {
		t.Fatal(err)
	}
	if err := lazy.Close(); err != nil {
		t.Fatal(err)
	}
	lazy, err = OpenLazyDirectoryFromFDs(packFD, rootFD, stateFD, prefix)
	if err != nil {
		t.Fatalf("reopening inherited lazy image descriptors: %v", err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(alias bool) {
			defer wg.Done()
			path := "large"
			if alias {
				path = "alias"
			}
			errs <- lazy.Ensure(path)
		}(i%2 != 0)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := lazy.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"large", "alias"} {
		got, err := os.ReadFile(filepath.Join(rootfs, name))
		if err != nil || !bytes.Equal(got, contents) {
			t.Fatalf("%s contents length=%d err=%v", name, len(got), err)
		}
	}
	left, _ := os.Stat(filepath.Join(rootfs, "large"))
	right, _ := os.Stat(filepath.Join(rootfs, "alias"))
	if !os.SameFile(left, right) {
		t.Fatal("lazy population broke hardlink identity")
	}
}
