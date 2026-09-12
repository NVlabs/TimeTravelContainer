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
	if usage := lazy.Usage(); usage.CompleteFiles != 0 || usage.TotalFiles != 1 || usage.CompleteBytes != 0 || usage.TotalBytes != uint64(len(contents)) {
		t.Fatalf("initial lazy cache usage = %+v", usage)
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
	usage, err := InspectLazyCache(rootfs)
	if err != nil {
		t.Fatal(err)
	}
	if usage.CompleteFiles != 1 || usage.TotalFiles != 1 || usage.CompleteBytes != uint64(len(contents)) || usage.TotalBytes != uint64(len(contents)) {
		t.Fatalf("populated lazy cache usage = %+v", usage)
	}
}

func TestLazyDirectoryCapacityEvictionFencesActiveGofer(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0755); err != nil {
		t.Fatal(err)
	}
	contents := bytes.Repeat([]byte("capacity-managed-image"), 10000)
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
	rootfs := filepath.Join(root, "cache", "rootfs")
	if err := MaterializeSkeleton(repository, id, rootfs, nil); err != nil {
		t.Fatal(err)
	}
	if err := WriteLazyDescriptor(rootfs, repository, id, ""); err != nil {
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
	if err := lazy.Ensure("alias"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := EvictLazyCache(rootfs, 0); !errors.Is(err, ErrLazyCacheBusy) {
		t.Fatalf("active eviction error = %v, want %v", err, ErrLazyCacheBusy)
	}
	if got, err := os.ReadFile(filepath.Join(rootfs, "large")); err != nil || !bytes.Equal(got, contents) {
		t.Fatalf("active cache changed: len=%d err=%v", len(got), err)
	}
	if err := lazy.Close(); err != nil {
		t.Fatal(err)
	}

	usage, evicted, err := EvictLazyCache(rootfs, 0)
	if err != nil {
		t.Fatal(err)
	}
	if usage.CompleteFiles != 0 || usage.CompleteBytes != 0 || evicted.Files != 1 || evicted.Bytes != uint64(len(contents)) {
		t.Fatalf("eviction = usage %+v, result %+v", usage, evicted)
	}
	for _, name := range []string{"large", "alias"} {
		if info, err := os.Stat(filepath.Join(rootfs, name)); err != nil || info.Size() != 0 {
			t.Fatalf("evicted %s = %+v, %v", name, info, err)
		}
	}

	lazy, err = OpenLazyDirectory(rootfs, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if size, mode, ok := lazy.Metadata("large"); !ok || size != int64(len(contents)) || mode&07777 != 0440 {
		t.Fatalf("evicted metadata = (%d, %#o, %t)", size, mode, ok)
	}
	if err := lazy.Ensure("large"); err != nil {
		t.Fatal(err)
	}
	if err := lazy.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(rootfs, "alias")); err != nil || !bytes.Equal(got, contents) {
		t.Fatalf("refetched alias: len=%d err=%v", len(got), err)
	}
}
