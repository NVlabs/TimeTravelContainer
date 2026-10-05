// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcpack

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestLogicalPackIDIsStableAndUnambiguous(t *testing.T) {
	got := IDForKey("image", "ab", "c")
	if got != IDForKey("image", "ab", "c") {
		t.Fatal("logical pack ID is not stable")
	}
	if got == IDForKey("image", "a", "bc") || got == IDForKey("memory-control", "ab", "c") {
		t.Fatal("logical pack ID does not preserve component boundaries")
	}
}

func TestDiscardUncommittedPackWaitsForReader(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "payload"), []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(root, "repository")
	id, err := WriteDirectory(source, repository, IDForKey("test", t.Name()))
	if err != nil {
		t.Fatal(err)
	}
	reader, _, err := Open(repository, id)
	if err != nil {
		t.Fatal(err)
	}
	if busy, err := Discard(repository, id); err != nil || !busy {
		t.Fatalf("Discard with reader = (busy=%t, err=%v), want busy", busy, err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if busy, err := Discard(repository, id); err != nil || busy {
		t.Fatalf("Discard after close = (busy=%t, err=%v)", busy, err)
	}
	if _, err := os.Stat(Path(repository, id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("discarded pack stat error=%v, want not-exist", err)
	}
	if busy, err := Discard(repository, id); err != nil || busy {
		t.Fatalf("idempotent Discard = (busy=%t, err=%v)", busy, err)
	}
}

func TestLegacyPackFormatIsRejected(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(root, "repository")
	id, err := WriteDirectory(source, repository, IDForKey("test", t.Name()))
	if err != nil {
		t.Fatal(err)
	}
	pack, err := os.OpenFile(Path(repository, id), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	header := make([]byte, Alignment)
	if _, err := pack.ReadAt(header, 0); err != nil {
		t.Fatal(err)
	}
	header = bytes.Replace(header, []byte(headerMagic), []byte("TTCPACK1"), 1)
	if _, err := pack.WriteAt(header, 0); err != nil {
		t.Fatal(err)
	}
	if err := pack.Close(); err != nil {
		t.Fatal(err)
	}
	if pack, _, err := Open(repository, id); err == nil {
		pack.Close()
		t.Fatal("legacy pack format was accepted")
	}
}

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
	id, err := WriteDirectory(source, repository, IDForKey("test", t.Name()))
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
	id, err := WriteDirectory(source, repository, IDForKey("test", t.Name()))
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
	id, err := WriteDirectory(source, repository, IDForKey("test", t.Name()))
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

// A prefetch that completes every file of an image records so, and a later
// gofer's prefetch then looks at none of them; an eviction removes the record
// before it makes any file incomplete, and the next prefetch completes the
// image again.
func TestLazyPrefetchRecordsItsCompletion(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(filepath.Join(source, "lib"), 0755); err != nil {
		t.Fatal(err)
	}
	contents := map[string][]byte{
		"large":       bytes.Repeat([]byte("prefetched-image-data"), 10000),
		"lib/small":   []byte("small\n"),
		"lib/another": bytes.Repeat([]byte{7}, 5000),
	}
	for name, data := range contents {
		if err := os.WriteFile(filepath.Join(source, name), data, 0444); err != nil {
			t.Fatal(err)
		}
	}
	repository := filepath.Join(root, "repository")
	id, err := WriteDirectory(source, repository, IDForKey("test", t.Name()))
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
	marker := filepath.Join(filepath.Dir(rootfs), LazyStateDirectory, lazyAllComplete)
	prefetch := func() {
		t.Helper()
		lazy, err := OpenLazyDirectory(rootfs, descriptor)
		if err != nil {
			t.Fatal(err)
		}
		lazy.StartPrefetch()
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			if _, err := os.Stat(marker); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the prefetch did not record its completion")
			}
		}
		if usage := lazy.Usage(); usage.CompleteFiles != usage.TotalFiles || usage.TotalFiles != uint64(len(contents)) {
			t.Errorf("usage once the prefetch completed = %+v", usage)
		}
		if err := lazy.Close(); err != nil {
			t.Fatal(err)
		}
		for name, data := range contents {
			if got, err := os.ReadFile(filepath.Join(rootfs, name)); err != nil || !bytes.Equal(got, data) {
				t.Errorf("%s after the prefetch: length %d, %v", name, len(got), err)
			}
		}
	}
	prefetch()
	// A later gofer's prefetch completes at once, fetching nothing: a file
	// made incomplete behind the record is left to be fetched on demand.
	lazy, err := OpenLazyDirectory(rootfs, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	shard, name := lazyLockName("lib/small")
	if err := os.Remove(filepath.Join(filepath.Dir(rootfs), LazyStateDirectory, shard, name)); err != nil {
		t.Fatal(err)
	}
	lazy.StartPrefetch()
	// Time enough for a prefetch to complete the file.
	time.Sleep(200 * time.Millisecond)
	if err := lazy.Close(); err != nil {
		t.Fatal(err)
	}
	if usage, err := InspectLazyCache(rootfs); err != nil || usage.CompleteFiles != uint64(len(contents)-1) {
		t.Fatalf("usage after a prefetch of a recorded image = %+v, %v; want one file incomplete", usage, err)
	}
	lazy, err = OpenLazyDirectory(rootfs, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if err := lazy.Ensure("lib/small"); err != nil {
		t.Fatal(err)
	}
	if err := lazy.Close(); err != nil {
		t.Fatal(err)
	}
	if _, evicted, err := EvictLazyCache(rootfs, 0); err != nil || evicted.Files != uint64(len(contents)) {
		t.Fatalf("eviction = %+v, %v", evicted, err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the completion record after an eviction: %v", err)
	}
	prefetch()
}

// lazySkeleton makes a skeleton root of a pack of contents, by path, and
// returns the root and its descriptor.
func lazySkeleton(t *testing.T, contents map[string][]byte) (string, LazyDescriptor) {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	for name, data := range contents {
		path := filepath.Join(source, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0444); err != nil {
			t.Fatal(err)
		}
	}
	repository := filepath.Join(root, "repository")
	id, err := WriteDirectory(source, repository, IDForKey("test", t.Name()))
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
	return rootfs, descriptor
}

// lazyRecord returns the path of the completion record of the file name of
// the skeleton root rootfs.
func lazyRecord(rootfs, name string) string {
	shard, file := lazyLockName(name)
	return filepath.Join(filepath.Dir(rootfs), LazyStateDirectory, shard, file)
}

// A gofer remembers the files it found or made complete, as no eviction runs
// while it has the image open: it looks at a file's record once, and at none
// once it knows the image complete.
func TestLazyDirectoryRemembersCompleteFiles(t *testing.T) {
	rootfs, descriptor := lazySkeleton(t, map[string][]byte{"file": []byte("file")})
	lazy, err := OpenLazyDirectory(rootfs, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	// A record removed behind its back stands for one looked at again:
	// Ensure makes it anew whenever it looks.
	record := lazyRecord(rootfs, "file")
	for _, allComplete := range []bool{false, true} {
		if err := lazy.Ensure("file"); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(record); err != nil {
			t.Fatal(err)
		}
		if allComplete {
			if err := os.WriteFile(filepath.Join(filepath.Dir(rootfs), LazyStateDirectory, lazyAllComplete), nil, 0600); err != nil {
				t.Fatal(err)
			}
			lazy.completedMu.Lock()
			lazy.completed = nil
			lazy.completedMu.Unlock()
			lazy.StartPrefetch()
		}
		if err := lazy.Ensure("file"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(record); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the record of a file known complete was looked at again (image known complete: %t): %v", allComplete, err)
		}
		if err := os.WriteFile(record, []byte("complete\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := lazy.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(rootfs, "file")); err != nil || string(got) != "file" {
		t.Errorf("file: %q, %v", got, err)
	}
}

// A fetch makes complete the incomplete files after the one asked for, those
// of its directory, which a guest opens next, reading their data from the
// pack at once; it passes over one whose lock another fetch holds, and stops
// at its bounds.
func TestLazyFetchTakesTheFilesAfterIt(t *testing.T) {
	contents := map[string][]byte{
		"a/1":   []byte("one"),
		"a/2":   bytes.Repeat([]byte("two"), 1000),
		"a/3":   []byte("three"),
		"a/4":   []byte("four"),
		"b/big": bytes.Repeat([]byte{9}, lazyFetchBytes),
		"b/end": []byte("end"),
	}
	rootfs, descriptor := lazySkeleton(t, contents)
	lazy, err := OpenLazyDirectory(rootfs, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	held, err := lazy.lock("a/3", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := lazy.Ensure("a/1"); err != nil {
		t.Fatal(err)
	}
	held.release()
	for name, want := range map[string]bool{"a/1": true, "a/2": true, "a/3": false, "a/4": true, "b/big": false, "b/end": false} {
		record, _ := os.ReadFile(lazyRecord(rootfs, name))
		if complete := string(record) == "complete\n"; complete != want {
			t.Errorf("after a fetch of a/1, %s complete: %t, want %t", name, complete, want)
		}
	}
	for _, name := range []string{"a/3", "b/big", "b/end"} {
		if err := lazy.Ensure(name); err != nil {
			t.Fatal(err)
		}
	}
	if err := lazy.Close(); err != nil {
		t.Fatal(err)
	}
	for name, data := range contents {
		if got, err := os.ReadFile(filepath.Join(rootfs, name)); err != nil || !bytes.Equal(got, data) {
			t.Errorf("%s: length %d, %v", name, len(got), err)
		}
		if info, err := os.Stat(filepath.Join(rootfs, name)); err != nil || info.Mode().Perm() != 0444 {
			t.Errorf("%s: %v, %v", name, info.Mode(), err)
		}
	}
	if usage, err := InspectLazyCache(rootfs); err != nil || usage.CompleteFiles != uint64(len(contents)) {
		t.Errorf("usage = %+v, %v", usage, err)
	}
}

// A pack keeps small files back to back, with no header for each entry, so a
// directory of many small files packs into little more than its size; a pack
// of the padded format before is still read, eagerly and on demand.
func TestPackFormats(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(filepath.Join(source, "d"), 0755); err != nil {
		t.Fatal(err)
	}
	files := make(map[string][]byte)
	var total int64
	for i := 0; i < 200; i++ {
		files[filepath.Join("d", fmt.Sprintf("f%03d", i))] = bytes.Repeat([]byte{byte(i)}, 100+7*i)
	}
	files["large"] = bytes.Repeat([]byte("large-"), 400000)
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(source, name), data, 0644); err != nil {
			t.Fatal(err)
		}
		total += int64(len(data))
	}
	if err := os.Symlink("large", filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(root, "repository")
	sizes := make(map[int]int64)
	for _, version := range []int{formatVersion, paddedFormatVersion} {
		id, err := writeDirectory(source, repository, IDForKey("test", t.Name(), fmt.Sprint(version)), version)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(Path(repository, id))
		if err != nil {
			t.Fatal(err)
		}
		sizes[version] = info.Size()
		destination := filepath.Join(root, fmt.Sprintf("eager-%d", version))
		if err := Materialize(repository, id, destination); err != nil {
			t.Fatalf("materializing a format %d pack: %v", version, err)
		}
		lazyRoot := filepath.Join(root, fmt.Sprintf("lazy-%d", version), "rootfs")
		if err := MaterializeSkeleton(repository, id, lazyRoot, nil); err != nil {
			t.Fatal(err)
		}
		if err := WriteLazyDescriptor(lazyRoot, repository, id, ""); err != nil {
			t.Fatal(err)
		}
		descriptor, err := ReadLazyDescriptor(lazyRoot)
		if err != nil {
			t.Fatal(err)
		}
		lazy, err := OpenLazyDirectory(lazyRoot, descriptor)
		if err != nil {
			t.Fatal(err)
		}
		for name, want := range files {
			if err := lazy.Ensure(filepath.ToSlash(name)); err != nil {
				t.Fatalf("reading %s on demand from a format %d pack: %v", name, version, err)
			}
			for _, dir := range []string{destination, lazyRoot} {
				if got, err := os.ReadFile(filepath.Join(dir, name)); err != nil || !bytes.Equal(got, want) {
					t.Fatalf("%s from a format %d pack in %s: %d bytes, %v; want %d bytes", name, version, dir, len(got), err, len(want))
				}
			}
		}
		if err := lazy.Close(); err != nil {
			t.Fatal(err)
		}
		if link, err := os.Readlink(filepath.Join(destination, "link")); err != nil || link != "large" {
			t.Fatalf("link from a format %d pack: %q, %v", version, link, err)
		}
	}
	// The data, its index, and the pages of the header, the trailer and the
	// large file's alignment.
	if limit := total + 64<<10; sizes[formatVersion] > limit {
		t.Errorf("pack of %d bytes of data is %d bytes, over %d", total, sizes[formatVersion], limit)
	}
	t.Logf("%d bytes of data pack into %d bytes, and took %d padded", total, sizes[formatVersion], sizes[paddedFormatVersion])
}
