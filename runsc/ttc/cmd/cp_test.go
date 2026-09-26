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
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/pkg/ttcvolume"
	"gvisor.dev/gvisor/runsc/specutils"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
)

func TestDistributedCopyRoute(t *testing.T) {
	upload, ok, err := parseDistributedCopyRoute([]string{"cp", "-aL", "/host/source", "demo@main:/guest/destination"})
	if err != nil || !ok {
		t.Fatalf("parse upload = (%+v, %t, %v)", upload, ok, err)
	}
	if upload.sourceContainer || !upload.destContainer || !upload.opts.archive || !upload.opts.followLink {
		t.Fatalf("upload route = %+v", upload)
	}
	download, ok, err := parseDistributedCopyRoute([]string{"cp", "demo@main:/guest/source", "/host/destination"})
	if err != nil || !ok || !download.sourceContainer || download.destContainer {
		t.Fatalf("parse download = (%+v, %t, %v)", download, ok, err)
	}
	if got := distributedCopyArgs(upload.opts, "-", upload.destination); strings.Join(got, "\x00") != "cp\x00--archive\x00--follow-link\x00-\x00demo@main:/guest/destination" {
		t.Fatalf("rewritten upload = %q", got)
	}
}

func TestDaemonStdinFrameReader(t *testing.T) {
	payload := bytes.Repeat([]byte("distributed-copy"), 8192)
	reader := bufio.NewReader(&daemonStdinFrameReader{raw: bytes.NewReader(payload)})
	var got bytes.Buffer
	for {
		typ, frame, err := readDaemonFrame(reader)
		if err != nil {
			t.Fatal(err)
		}
		switch typ {
		case daemonFrameStdin:
			got.Write(frame)
		case daemonFrameEOF:
			if got.String() != string(payload) {
				t.Fatalf("framed payload size=%d, want %d", got.Len(), len(payload))
			}
			return
		default:
			t.Fatalf("unexpected frame %q", typ)
		}
	}
}

func TestDistributedCopyUploadConsumesFlowControl(t *testing.T) {
	flow := newDaemonStdinFlow()
	payload := bytes.Repeat([]byte("large-distributed-copy"), daemonStdinWindowBytes/(len("large-distributed-copy"))*3)
	framed := bufio.NewReader(&daemonStdinFrameReader{raw: bytes.NewReader(payload), flow: flow})
	var response bytes.Buffer
	var responseMu sync.Mutex
	var received bytes.Buffer
	done := make(chan error, 1)
	go func() {
		for {
			typ, frame, err := readDaemonFrame(framed)
			if err != nil {
				done <- err
				return
			}
			switch typ {
			case daemonFrameStdin:
				received.Write(frame)
				if err := flow.acknowledge(len(frame)); err != nil {
					done <- err
					return
				}
			case daemonFrameEOF:
				done <- nil
				return
			default:
				done <- fmt.Errorf("unexpected frame %q", typ)
				return
			}
		}
	}()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(received.Bytes(), payload) {
		t.Fatalf("received %d bytes, want %d", received.Len(), len(payload))
	}
	// Keep imports and the framing writer exercised together; the network
	// response filter forwards non-acknowledgement frames unchanged.
	if err := writeDaemonStatus(&response, &responseMu, 0); err != nil {
		t.Fatal(err)
	}
}

func TestDaemonCopyArchiveReader(t *testing.T) {
	var frames bytes.Buffer
	var mu sync.Mutex
	if err := writeDaemonFrame(&frames, &mu, daemonFrameStdout, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := writeDaemonFrame(&frames, &mu, daemonFrameStderr, []byte("diagnostic")); err != nil {
		t.Fatal(err)
	}
	if err := writeDaemonFrame(&frames, &mu, daemonFrameStdout, []byte("second")); err != nil {
		t.Fatal(err)
	}
	if err := writeDaemonStatus(&frames, &mu, 7); err != nil {
		t.Fatal(err)
	}
	reader := newDaemonCopyArchiveReader(&frames)
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "firstsecond" || reader.diagnostic.String() != "diagnostic" || !reader.statusSeen || reader.status != 7 {
		t.Fatalf("archive=%q diagnostic=%q status=(%t,%d)", got, reader.diagnostic.String(), reader.statusSeen, reader.status)
	}
}

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
		{"demo@main:relative", true, selector{container: "demo", branch: defaultBranchID}, "/relative"},
		{"demo@main:/.", true, selector{container: "demo", branch: defaultBranchID}, "/"},
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

func TestOfflineCopySpecSelectsPersistentBranchAndHelper(t *testing.T) {
	repositories := []specutils.TTCVolumeRepository{
		{Source: specutils.TTCRootfsUpperSource, Path: "/data/root", Branch: ttcvolume.MainBranch, CopyMode: string(ttcvolume.CopyModeReflink)},
		{Source: "/run/ttc/volumes/data", Path: "/data/volume", Branch: ttcvolume.MainBranch, CopyMode: string(ttcvolume.CopyModeReflink)},
	}
	encoded, err := json.Marshal(repositories)
	if err != nil {
		t.Fatal(err)
	}
	original := &specs.Spec{
		Process: &specs.Process{
			Args:     []string{"/image-entrypoint"},
			Terminal: true,
			User:     specs.User{UID: 1000, GID: 1000},
		},
		Root: &specs.Root{Path: "/image"},
		Linux: &specs.Linux{
			CgroupsPath: "ttc/original",
			Resources:   &specs.LinuxResources{},
		},
		Annotations: map[string]string{
			specutils.AnnotationTTCVolumeRepositories:  string(encoded),
			"dev.timetravelcontainer.internal-console": "init",
			ttcbundle.PidsLimitAnnotation:              "sentry",
		},
	}
	cloned, err := offlineCopySpec(original, ttcbundle.ContainerInfo{ID: "family"}, "child", []string{"export", "/tmp/value"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cloned.Process.Args, []string{"ttc", copyHelperArg, "export", "/tmp/value"}; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("helper args = %q, want %q", got, want)
	}
	if cloned.Process.Terminal || cloned.Process.User.UID != 0 || cloned.Process.Capabilities == nil {
		t.Fatalf("helper process = %+v", cloned.Process)
	}
	if cloned.Linux.CgroupsPath != "" || cloned.Linux.Resources != nil {
		t.Fatalf("offline helper retained cgroup settings: %+v", cloned.Linux)
	}
	if _, ok := cloned.Annotations["dev.timetravelcontainer.internal-console"]; ok {
		t.Fatal("offline helper retained TTC console interception")
	}
	if _, ok := cloned.Annotations[ttcbundle.PidsLimitAnnotation]; ok {
		t.Fatal("offline helper retained Sentry PID-limit marker after dropping Linux resources")
	}
	gotRepositories, err := specutils.TTCVolumeRepositories(cloned)
	if err != nil {
		t.Fatal(err)
	}
	wantBranch := ttcvolume.ScopedBranchID("family", "child")
	for _, repository := range gotRepositories {
		if repository.Branch != wantBranch {
			t.Fatalf("repository %q branch = %q, want %q", repository.Source, repository.Branch, wantBranch)
		}
	}
	if original.Process.Args[0] != "/image-entrypoint" || original.Linux.CgroupsPath != "ttc/original" {
		t.Fatal("offline copy mutated the stored container spec")
	}
}

func TestOfflineCopySpecRejectsLegacyWritableRoot(t *testing.T) {
	_, err := offlineCopySpec(&specs.Spec{
		Process: &specs.Process{Args: []string{"true"}},
		Root:    &specs.Root{Path: "/image"},
	}, ttcbundle.ContainerInfo{ID: "family"}, defaultBranchID, []string{"export", "/"})
	if err == nil || !strings.Contains(err.Error(), "predates persistent rootfs") {
		t.Fatalf("offlineCopySpec error = %v", err)
	}
}

func TestOpenEmbeddedGuestCopyHelper(t *testing.T) {
	previous := guestCopyHelper
	t.Cleanup(func() { guestCopyHelper = previous })
	want := []byte("guest-elf-fixture")
	SetGuestCopyHelper(want)
	helper, err := (&runtimeBackend{}).openCopyHelper()
	if err != nil {
		t.Fatal(err)
	}
	defer helper.Close()
	got, err := io.ReadAll(helper)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("embedded helper = %q, want %q", got, want)
	}
	if _, err := os.Stat(helper.Name()); !os.IsNotExist(err) {
		t.Fatalf("embedded helper path remains after open: %v", err)
	}
}

func TestNormalizeCopyCommandArgsResolvesHostPath(t *testing.T) {
	args, err := normalizeCopyCommandArgs([]string{"cp", "-a", "local/.", "demo@main:/dest"})
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
	if args[len(args)-1] != "demo@main:/dest" {
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

	t.Run("prepared archive to container", func(t *testing.T) {
		backend := &fakeBackend{copyStdin: strings.NewReader("archive")}
		if err := runCopy([]string{copyPreparedInputArg, "-", "demo@child:/destination"}, io.Discard, io.Discard, backend); err != nil {
			t.Fatal(err)
		}
		if string(backend.copyImportData) != "archive" || backend.copyImportRaw {
			t.Fatalf("prepared import data=%q raw=%t", backend.copyImportData, backend.copyImportRaw)
		}
	})
}

func TestOfflineCopyState(t *testing.T) {
	for _, state := range []string{"dehydrated", "exited", "stopped"} {
		if !offlineCopyState(state) {
			t.Errorf("offlineCopyState(%q)=false, want true", state)
		}
	}
	for _, state := range []string{"", "created", "running", "paused", "dehydrating"} {
		if offlineCopyState(state) {
			t.Errorf("offlineCopyState(%q)=true, want false", state)
		}
	}
}
