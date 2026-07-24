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

package cmd

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCopyCommandArgs(t *testing.T) {
	opts, source, destination, err := parseCopyCommandArgs([]string{"-aL", "demo@child:/src/.", "/tmp/dest"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.archive || !opts.followLink || source != "demo@child:/src/." || destination != "/tmp/dest" {
		t.Fatalf("parseCopyCommandArgs = (%+v, %q, %q), want both options and original operands", opts, source, destination)
	}
	if _, _, _, err := parseCopyCommandArgs([]string{"--bogus", "a", "b"}); err == nil {
		t.Fatal("unknown option unexpectedly accepted")
	}
}

func TestSplitCopyContainerEndpoint(t *testing.T) {
	for _, test := range []struct {
		value     string
		container bool
		target    selector
		path      string
	}{
		{"demo@child:/src/.", true, selector{container: "demo", branch: "child"}, "/src/."},
		{"demo:relative", true, selector{container: "demo", branch: defaultBranchID}, "/relative"},
		{"demo:/.", true, selector{container: "demo", branch: defaultBranchID}, "/"},
		{"./local:name", false, selector{}, ""},
		{"/tmp/local", false, selector{}, ""},
	} {
		got, container, err := splitCopyContainerEndpoint(test.value)
		if err != nil {
			t.Fatalf("splitCopyContainerEndpoint(%q): %v", test.value, err)
		}
		if container != test.container || got != (copyContainerEndpoint{target: test.target, path: test.path}) {
			t.Errorf("splitCopyContainerEndpoint(%q) = (%+v, %t), want (%+v, %t)", test.value, got, container, copyContainerEndpoint{target: test.target, path: test.path}, test.container)
		}
	}
}

func TestNormalizeCopyCommandArgsResolvesHostPath(t *testing.T) {
	args, err := normalizeCopyCommandArgs([]string{"cp", "-a", "local/.", "demo:/dest"})
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs("local")
	if err != nil {
		t.Fatal(err)
	}
	if got := args[len(args)-2]; got != want+string(filepath.Separator)+"." {
		t.Fatalf("normalized source=%q, want %q", got, want+string(filepath.Separator)+".")
	}
	if args[len(args)-1] != "demo:/dest" {
		t.Fatalf("container destination changed to %q", args[len(args)-1])
	}
}

func TestCopyArchiveRoundTrip(t *testing.T) {
	sourceParent := t.TempDir()
	source := filepath.Join(sourceParent, "tree")
	if err := os.Mkdir(source, 0751); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(source, "nested"), 0750); err != nil {
		t.Fatal(err)
	}
	regular := filepath.Join(source, "nested", "file")
	if err := os.WriteFile(regular, []byte("payload\x00with binary"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(regular, filepath.Join(source, "hard")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nested/file", filepath.Join(source, "symbolic")); err != nil {
		t.Fatal(err)
	}

	var archive bytes.Buffer
	if err := writeCopyArchive(source, false, &archive); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	if err := extractCopyArchive(&archive, destination, false, os.Geteuid(), os.Getegid(), false); err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(destination, "tree")
	data, err := os.ReadFile(filepath.Join(copied, "nested", "file"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "payload\x00with binary" {
		t.Errorf("copied data=%q", data)
	}
	link, err := os.Readlink(filepath.Join(copied, "symbolic"))
	if err != nil || link != "nested/file" {
		t.Errorf("symlink=(%q, %v), want nested/file", link, err)
	}
	first, err := os.Stat(filepath.Join(copied, "nested", "file"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := os.Stat(filepath.Join(copied, "hard"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(first, second) {
		t.Error("hard link was materialized as a distinct file")
	}
}

func TestCopyDirectoryContents(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(source, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "child"), []byte("contents"), 0644); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := writeCopyArchive(source+string(filepath.Separator)+".", false, &archive); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "new-destination")
	if err := extractCopyArchive(&archive, destination, false, os.Geteuid(), os.Getegid(), false); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(destination, "child")); err != nil || string(data) != "contents" {
		t.Fatalf("copied child=(%q, %v)", data, err)
	}
	if _, err := os.Stat(filepath.Join(destination, "source")); !os.IsNotExist(err) {
		t.Fatalf("source directory was nested despite trailing '/.': %v", err)
	}
}

func TestExtractCopyArchiveRejectsTraversal(t *testing.T) {
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{Name: "../escape", Typeflag: tar.TypeReg, Mode: 0644, Size: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	destination := filepath.Join(parent, "destination")
	if err := os.Mkdir(destination, 0755); err != nil {
		t.Fatal(err)
	}
	if err := extractCopyArchive(&archive, destination, false, os.Geteuid(), os.Getegid(), true); err == nil {
		t.Fatal("path traversal archive unexpectedly extracted")
	}
	if _, err := os.Stat(filepath.Join(parent, "escape")); !os.IsNotExist(err) {
		t.Fatalf("escape path exists after rejected archive: %v", err)
	}
}

func TestExtractCopyArchiveRejectsSymlinkAncestor(t *testing.T) {
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	headers := []*tar.Header{
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../outside", Mode: 0777},
		{Name: "link/payload", Typeflag: tar.TypeReg, Mode: 0644, Size: 1},
	}
	for _, header := range headers {
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size != 0 {
			if _, err := writer.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	destination := filepath.Join(parent, "destination")
	outside := filepath.Join(parent, "outside")
	if err := os.Mkdir(destination, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outside, 0755); err != nil {
		t.Fatal(err)
	}
	if err := extractCopyArchive(&archive, destination, false, os.Geteuid(), os.Getegid(), true); err == nil {
		t.Fatal("symlink-ancestor archive unexpectedly extracted")
	}
	if _, err := os.Stat(filepath.Join(outside, "payload")); !os.IsNotExist(err) {
		t.Fatalf("outside payload exists after rejected archive: %v", err)
	}
}

func TestRunCopyStreamsBothDirections(t *testing.T) {
	t.Run("container to stdout", func(t *testing.T) {
		backend := &fakeBackend{copyExportData: []byte("archive")}
		var stdout bytes.Buffer
		if err := runCopy([]string{"-L", "demo@child:/source", "-"}, &stdout, io.Discard, backend); err != nil {
			t.Fatal(err)
		}
		if stdout.String() != "archive" || backend.copyExportTarget != (selector{container: "demo", branch: "child"}) ||
			backend.copyExportPath != "/source" || !backend.copyExportFollow {
			t.Fatalf("export result=%q target=%+v path=%q follow=%t", stdout.String(), backend.copyExportTarget, backend.copyExportPath, backend.copyExportFollow)
		}
	})

	t.Run("stdin archive to container", func(t *testing.T) {
		backend := &fakeBackend{copyStdin: strings.NewReader("archive")}
		if err := runCopy([]string{"-a", "-", "demo@child:/destination"}, io.Discard, io.Discard, backend); err != nil {
			t.Fatal(err)
		}
		if string(backend.copyImportData) != "archive" || backend.copyImportTarget != (selector{container: "demo", branch: "child"}) ||
			backend.copyImportPath != "/destination" || !backend.copyImportArchive || !backend.copyImportRaw {
			t.Fatalf("import data=%q target=%+v path=%q archive=%t raw=%t", backend.copyImportData, backend.copyImportTarget, backend.copyImportPath, backend.copyImportArchive, backend.copyImportRaw)
		}
	})
}
