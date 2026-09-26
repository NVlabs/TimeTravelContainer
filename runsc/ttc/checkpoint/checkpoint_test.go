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

package checkpoint

import (
	"archive/tar"
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
	if err := Unpack(archive, extracted); err != nil {
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
	if err := Unpack(archivePath, destination); err == nil {
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
	if err := Unpack(archivePath, destination); err == nil {
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
