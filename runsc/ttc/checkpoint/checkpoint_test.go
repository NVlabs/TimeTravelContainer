// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package checkpoint

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gvisor.dev/gvisor/runsc/ttc/bundle"
)

func testManifest() Manifest {
	return Manifest{
		SourceBranch: "main",
		Container: bundle.ContainerInfo{
			ID:       "old",
			Name:     "old",
			Digest:   "sha256:test",
			Platform: "linux/amd64",
		},
	}
}

// unpack unpacks the archive at source as Unpack does.
func unpack(source, destination string, leave ...string) (map[string]Section, error) {
	archive, err := os.Open(source)
	if err != nil {
		return nil, err
	}
	defer archive.Close()
	return Unpack(archive, destination, leave...)
}

func TestPackUnpack(t *testing.T) {
	staging := t.TempDir()
	if err := os.MkdirAll(filepath.Join(staging, "state"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "state", "checkpoint.img"), []byte("state"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("checkpoint.img", filepath.Join(staging, "state", "latest")); err != nil {
		t.Fatal(err)
	}
	if err := WriteManifest(staging, testManifest()); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "snapshot.ttc")
	if err := Pack(staging, archive); err != nil {
		t.Fatal(err)
	}
	extracted := filepath.Join(t.TempDir(), "extracted")
	if _, err := unpack(archive, extracted); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(extracted, "state", "checkpoint.img"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "state" {
		t.Fatalf("checkpoint state = %q, want state", data)
	}
	target, err := os.Readlink(filepath.Join(extracted, "state", "latest"))
	if err != nil {
		t.Fatal(err)
	}
	if target != "checkpoint.img" {
		t.Fatalf("symlink target = %q, want checkpoint.img", target)
	}
	manifest, err := ReadManifest(extracted)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Container.Digest != "sha256:test" {
		t.Fatalf("manifest = %+v", manifest)
	}
	if err := Pack(staging, archive); err == nil {
		t.Fatal("Pack overwrote an existing checkpoint")
	}
}

// A member streamed into an archive comes first, its size recorded once it is
// written, and the staging tree's entries follow it: Unpack extracts both.
func TestArchiveStreamsAMemberFirst(t *testing.T) {
	staging := t.TempDir()
	if err := os.MkdirAll(filepath.Join(staging, "state"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "state", "metadata.state"), []byte("metadata"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := WriteManifest(staging, testManifest()); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "snapshot.ttc")
	archive, err := CreateArchive(filepath.Dir(destination))
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Abort()
	// Not a whole number of blocks, and written in pieces.
	memory := bytes.Repeat([]byte("page"), 3000)
	if err := archive.Stream("state/memory.stream", 0600, func(w *os.File) error {
		for rest := memory; len(rest) != 0; {
			n := min(len(rest), 1000)
			if _, err := w.Write(rest[:n]); err != nil {
				return err
			}
			rest = rest[n:]
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := archive.Stream("state/memory.stream", 0600, func(*os.File) error { return nil }); err == nil {
		t.Fatal("a member was streamed twice")
	}
	if err := archive.Finish(staging); err != nil {
		t.Fatal(err)
	}
	contents, err := io.ReadAll(archive.Contents())
	if err != nil {
		t.Fatal(err)
	}
	if err := archive.Publish(destination); err != nil {
		t.Fatal(err)
	}
	if published, err := os.ReadFile(destination); err != nil || !bytes.Equal(published, contents) {
		t.Fatalf("the published archive differs from its contents (%v)", err)
	}
	file, err := os.Open(destination)
	if err != nil {
		t.Fatal(err)
	}
	first, err := tar.NewReader(file).Next()
	file.Close()
	if err != nil || first.Name != "state/memory.stream" || first.Size != int64(len(memory)) {
		t.Fatalf("the archive begins with %+v (%v), want the streamed member", first, err)
	}
	extracted := filepath.Join(t.TempDir(), "extracted")
	if _, err := unpack(destination, extracted); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]byte{"state/memory.stream": memory, "state/metadata.state": []byte("metadata")} {
		got, err := os.ReadFile(filepath.Join(extracted, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s holds %d bytes (%v), want %d", name, len(got), err, len(want))
		}
	}
}

// Unpack leaves a member it is asked to in the archive, wherever it is, and
// says where its content is.
func TestUnpackLeavesAMemberInTheArchive(t *testing.T) {
	memory := bytes.Repeat([]byte("page"), 3000)
	staging := t.TempDir()
	if err := os.MkdirAll(filepath.Join(staging, "state"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := WriteManifest(staging, testManifest()); err != nil {
		t.Fatal(err)
	}
	// Packed after the manifest, as a staged file.
	if err := os.WriteFile(filepath.Join(staging, "state", "memory.stream"), memory, 0600); err != nil {
		t.Fatal(err)
	}
	packed := filepath.Join(t.TempDir(), "packed.ttc")
	if err := Pack(staging, packed); err != nil {
		t.Fatal(err)
	}
	// Streamed first.
	if err := os.Remove(filepath.Join(staging, "state", "memory.stream")); err != nil {
		t.Fatal(err)
	}
	streamed := filepath.Join(t.TempDir(), "streamed.ttc")
	archive, err := CreateArchive(filepath.Dir(streamed))
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Abort()
	if err := archive.Stream("state/memory.stream", 0600, func(w *os.File) error {
		_, err := w.Write(memory)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := archive.Finish(staging); err != nil {
		t.Fatal(err)
	}
	if err := archive.Publish(streamed); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{packed, streamed} {
		file, err := os.Open(source)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		extracted := filepath.Join(t.TempDir(), "extracted")
		sections, err := Unpack(file, extracted, "state/memory.stream")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(filepath.Join(extracted, "state", "memory.stream")); !os.IsNotExist(err) {
			t.Fatalf("%s: the member left in the archive was extracted (%v)", source, err)
		}
		if _, err := ReadManifest(extracted); err != nil {
			t.Fatalf("%s: %v", source, err)
		}
		section, ok := sections["state/memory.stream"]
		if !ok {
			t.Fatalf("%s: Unpack returned %v, without the member", source, sections)
		}
		got := make([]byte, section.Size)
		if _, err := file.ReadAt(got, section.Offset); err != nil || !bytes.Equal(got, memory) {
			t.Fatalf("%s: the section at %d holds %d bytes (%v), not the member's %d", source, section.Offset, len(got), err, len(memory))
		}
	}
}

// An archive read rather than published is removed, unsynced, by Abort.
func TestArchiveReadAndDiscarded(t *testing.T) {
	staging := t.TempDir()
	if err := WriteManifest(staging, testManifest()); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	archive, err := CreateArchive(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := archive.Finish(staging); err != nil {
		t.Fatal(err)
	}
	header, err := tar.NewReader(archive.Contents()).Next()
	if err != nil || header.Name != "manifest.json" {
		t.Fatalf("the archive begins with %+v (%v), want the manifest", header, err)
	}
	archive.Abort()
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("the discarded archive's directory holds %v (%v)", entries, err)
	}
}

// An archive written into its caller's file stays there once finished, and
// Unpack reads it wherever the file's offset is; one that is not finished is
// emptied by Abort.
func TestWriteArchiveIntoACallersFile(t *testing.T) {
	staging := t.TempDir()
	if err := WriteManifest(staging, testManifest()); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(filepath.Join(t.TempDir(), "client.ttc"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	unfinished := WriteArchive(file)
	if err := unfinished.Stream("state/memory.stream", 0600, func(w *os.File) error {
		_, err := w.Write([]byte("partial"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	unfinished.Abort()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatalf("an unfinished archive left %d bytes in its caller's file", info.Size())
	}
	archive := WriteArchive(file)
	memory := bytes.Repeat([]byte("page"), 3000)
	if err := archive.Stream("state/memory.stream", 0600, func(w *os.File) error {
		_, err := w.Write(memory)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := archive.Finish(staging); err != nil {
		t.Fatal(err)
	}
	if err := archive.Publish(filepath.Join(t.TempDir(), "elsewhere.ttc")); err == nil {
		t.Fatal("an archive in its caller's file was published")
	}
	archive.Abort()
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	extracted := filepath.Join(t.TempDir(), "extracted")
	sections, err := Unpack(file, extracted, "state/memory.stream")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadManifest(extracted); err != nil {
		t.Fatal(err)
	}
	section := sections["state/memory.stream"]
	got := make([]byte, section.Size)
	if _, err := file.ReadAt(got, section.Offset); err != nil || !bytes.Equal(got, memory) {
		t.Fatalf("the section at %d holds %d bytes (%v), not the member's %d", section.Offset, len(got), err, len(memory))
	}
}

// A streamed member's header records a size beyond 8 GiB in its one block.
func TestStreamedHeaderRecordsLargeSizes(t *testing.T) {
	const size = 10 << 30
	header, err := streamedHeader("state/memory.stream", 0600, size)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := tar.NewReader(bytes.NewReader(header)).Next()
	if err != nil || parsed.Name != "state/memory.stream" || parsed.Size != size {
		t.Fatalf("the header reads as %+v (%v), want a member of %d bytes", parsed, err, int64(size))
	}
}

func TestUnpackRejectsFileBeneathSymlink(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "bad.ttc")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	writer := tar.NewWriter(file)
	entries := []tar.Header{
		{Name: ManifestName, Typeflag: tar.TypeReg, Size: int64(len(`{"format_version":1}`)), Mode: 0600},
		{Name: "mount", Typeflag: tar.TypeSymlink, Linkname: "../../escape", Mode: 0777},
		{Name: "mount/file", Typeflag: tar.TypeReg, Size: 1, Mode: 0600},
	}
	for index := range entries {
		if err := writer.WriteHeader(&entries[index]); err != nil {
			t.Fatal(err)
		}
		switch entries[index].Name {
		case ManifestName:
			if _, err := writer.Write([]byte(`{"format_version":1}`)); err != nil {
				t.Fatal(err)
			}
		case "mount/file":
			if _, err := writer.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "out")
	if _, err := unpack(archivePath, destination); err == nil {
		t.Fatal("Unpack accepted a file beneath a symlink")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(destination), "escape", "file")); !os.IsNotExist(err) {
		t.Fatalf("symlink traversal target exists: %v", err)
	}
}

func TestUnpackRejectsTraversal(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "bad.ttc")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	writer := tar.NewWriter(file)
	if err := writer.WriteHeader(&tar.Header{Name: "../escape", Typeflag: tar.TypeReg, Size: 1, Mode: 0600}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "out")
	if _, err := unpack(archivePath, destination); err == nil {
		t.Fatal("Unpack accepted traversal")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(destination), "escape")); !os.IsNotExist(err) {
		t.Fatalf("traversal target exists: %v", err)
	}
}

func TestReadManifestRejectsRepositorySymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "repositories"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "repositories", "0")); err != nil {
		t.Fatal(err)
	}
	manifest := testManifest()
	manifest.Repositories = []RepositorySnapshot{{Source: "rootfs", Path: "repositories/0"}}
	if err := WriteManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadManifest(root); err == nil {
		t.Fatal("ReadManifest accepted a repository snapshot symlink")
	}
}

func TestReadManifestRejectsSymlinkedSnapshotParent(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "repositories")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(outside, "0"), 0700); err != nil {
		t.Fatal(err)
	}
	manifest := testManifest()
	manifest.Repositories = []RepositorySnapshot{{Source: "rootfs", Path: "repositories/0"}}
	if err := WriteManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadManifest(root); err == nil {
		t.Fatal("ReadManifest followed a symlinked repository parent")
	}
}

// A checkpoint of the format before this one held a gVisor checkpoint that
// only the build which wrote it could restore; reading one says so.
func TestReadManifestRefusesAKernelCheckpoint(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ManifestName), []byte(`{"format_version":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadManifest(root); err == nil || !strings.Contains(err.Error(), "earlier TTC build") {
		t.Fatalf("ReadManifest of a format 1 checkpoint: %v", err)
	}
}
