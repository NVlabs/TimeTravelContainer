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

package images

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/runsc/ttc/ownership"
)

type tarEntry struct {
	name     string
	body     string
	mode     int64
	uid      int
	gid      int
	typeflag byte
	linkname string
	xattrs   map[string]string
}

func TestApplyLayerHandlesCompressionAndWhiteouts(t *testing.T) {
	root := t.TempDir()
	base := makeLayer(t, false, []tarEntry{
		{name: "etc/", mode: 0755},
		{name: "etc/base", body: "base", mode: 0644},
		{name: "etc/remove", body: "old", mode: 0644},
	})
	if err := applyLayer(context.Background(), root, bytes.NewReader(base)); err != nil {
		t.Fatalf("applying base layer: %v", err)
	}
	upper := makeLayer(t, true, []tarEntry{
		{name: "etc/.wh.remove", mode: 0600},
		{name: "etc/base", body: "upper", mode: 0750},
	})
	if err := applyLayer(context.Background(), root, bytes.NewReader(upper)); err != nil {
		t.Fatalf("applying upper layer: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "etc/base"))
	if err != nil {
		t.Fatalf("reading overlaid file: %v", err)
	}
	if string(got) != "upper" {
		t.Errorf("overlaid contents=%q, want upper", got)
	}
	if _, err := os.Stat(filepath.Join(root, "etc/remove")); !os.IsNotExist(err) {
		t.Errorf("whiteouted file stat error=%v, want not-exist", err)
	}
}

func TestApplyLayerAllowsLaterWhiteoutOfDeclaredDirectory(t *testing.T) {
	root := t.TempDir()
	layer := makeLayer(t, true, []tarEntry{
		{name: "transient/", mode: 0755},
		{name: "transient/value", body: "temporary", mode: 0644},
		{name: ".wh.transient", mode: 0600},
		{name: "retained", body: "complete", mode: 0644},
	})
	if err := applyLayer(context.Background(), root, bytes.NewReader(layer)); err != nil {
		t.Fatalf("applying layer with later directory whiteout: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "transient")); !os.IsNotExist(err) {
		t.Fatalf("whiteouted transient directory stat error = %v, want not-exist", err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "retained")); err != nil || string(got) != "complete" {
		t.Fatalf("retained entry = %q, err %v", got, err)
	}
}

func TestApplyLayerDefersRestrictiveDirectoryModes(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() {
		// testing.TempDir cleanup is intentionally unprivileged too.
		_ = os.Chmod(filepath.Join(root, "root"), 0700)
		_ = os.Chmod(filepath.Join(root, "lower"), 0700)
	})
	base := makeLayer(t, false, []tarEntry{
		{name: "root/", mode: 0550},
		{name: "root/.bash_logout", body: "logout", mode: 0644},
		{name: "lower/", mode: 0500},
	})
	if err := applyLayer(context.Background(), root, bytes.NewReader(base)); err != nil {
		t.Fatalf("applying restrictive base layer: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "root", ".bash_logout")); err != nil || string(got) != "logout" {
		t.Fatalf("reading file created after restrictive directory: got %q, err %v", got, err)
	}
	if info, err := os.Stat(filepath.Join(root, "root")); err != nil {
		t.Fatal(err)
	} else if got, want := info.Mode().Perm(), os.FileMode(0550); got != want {
		t.Fatalf("root mode=%#o, want %#o", got, want)
	}

	upper := makeLayer(t, false, []tarEntry{
		{name: "lower/from-upper", body: "upper", mode: 0644},
	})
	if err := applyLayer(context.Background(), root, bytes.NewReader(upper)); err != nil {
		t.Fatalf("applying into restrictive directory from an earlier layer: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "lower", "from-upper")); err != nil || string(got) != "upper" {
		t.Fatalf("reading upper-layer file: got %q, err %v", got, err)
	}
	if info, err := os.Stat(filepath.Join(root, "lower")); err != nil {
		t.Fatal(err)
	} else if got, want := info.Mode().Perm(), os.FileMode(0500); got != want {
		t.Fatalf("lower mode=%#o, want %#o", got, want)
	}
}

func TestApplyLayerPreservesEmptyDirectory(t *testing.T) {
	root := t.TempDir()
	layer := makeLayer(t, true, []tarEntry{
		{name: "run/", mode: 0755},
		{name: "var/", mode: 0755},
		{name: "var/run", mode: 0777, typeflag: tar.TypeSymlink, linkname: "../run"},
	})
	if err := applyLayer(context.Background(), root, bytes.NewReader(layer)); err != nil {
		t.Fatalf("applying layer with empty directory: %v", err)
	}
	if info, err := os.Stat(filepath.Join(root, "run")); err != nil || !info.IsDir() {
		t.Fatalf("empty directory was not preserved: info=%v err=%v", info, err)
	}
}

func TestApplyLayerVirtualizesXattrs(t *testing.T) {
	root := t.TempDir()
	owners := newOwnershipState()
	layer := makeLayer(t, false, []tarEntry{
		{
			name: "usr/bin/newgidmap",
			body: "binary",
			mode: 0755,
			xattrs: map[string]string{
				"security.capability": "capability-data",
				"user.image-metadata": "portable-data",
			},
		},
		{
			name:   "replace",
			body:   "file",
			mode:   0644,
			xattrs: map[string]string{"security.capability": "removed-capability"},
		},
	})
	if err := applyLayerWithOwnership(context.Background(), root, bytes.NewReader(layer), owners); err != nil {
		t.Fatalf("applying layer with privileged xattr: %v", err)
	}
	replacement := makeLayer(t, false, []tarEntry{
		{name: "replace/", mode: 0755},
		{name: "replace/child", body: "child", mode: 0644},
	})
	if err := applyLayerWithOwnership(context.Background(), root, bytes.NewReader(replacement), owners); err != nil {
		t.Fatalf("replacing capable file with a directory: %v", err)
	}
	manifest, err := owners.finalize(root)
	if err != nil {
		t.Fatal(err)
	}
	value, ok := manifest.LookupXattr("usr/bin/newgidmap", "security.capability")
	if !ok || string(value) != "capability-data" {
		t.Fatalf("virtual capability=%q, ok=%t", value, ok)
	}
	if _, err := unix.Lgetxattr(filepath.Join(root, "usr/bin/newgidmap"), "security.capability", nil); !isMissingXattr(err) {
		t.Fatalf("physical security.capability unexpectedly exists: %v", err)
	}
	value, ok = manifest.LookupXattr("usr/bin/newgidmap", "user.image-metadata")
	if !ok || string(value) != "portable-data" {
		t.Fatalf("virtual user xattr=%q, ok=%t", value, ok)
	}
	if _, err := unix.Lgetxattr(filepath.Join(root, "usr/bin/newgidmap"), "user.image-metadata", nil); !isMissingXattr(err) {
		t.Fatalf("physical user.image-metadata unexpectedly exists: %v", err)
	}
	if _, ok := manifest.LookupXattr("replace", "security.capability"); ok {
		t.Fatal("replacement directory retained the replaced file's capability")
	}
}

func TestApplyLayerSkipsHostDeviceCreation(t *testing.T) {
	root := t.TempDir()
	owners := newOwnershipState()
	layer := makeLayer(t, false, []tarEntry{
		{name: "dev/", mode: 0755},
		{name: "dev/image-device", mode: 0600, typeflag: tar.TypeChar},
		{name: "payload", body: "ok", mode: 0644},
	})
	if err := applyLayerWithOwnership(context.Background(), root, bytes.NewReader(layer), owners); err != nil {
		t.Fatalf("applying layer containing a device node: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "dev/image-device")); !os.IsNotExist(err) {
		t.Fatalf("image device exists on host or stat failed: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "payload")); err != nil || string(got) != "ok" {
		t.Fatalf("payload=%q, err=%v", got, err)
	}
}

func TestFinalizeMakesRestrictiveImageFilesHostReadable(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(root, "private"), 0700)
	})
	owners := newOwnershipState()
	layer := makeLayer(t, false, []tarEntry{
		{name: "etc/", mode: 0700},
		{name: "etc/shadow", body: "root:*:1:0:99999:7:::", mode: 0000},
		{name: "hard-target", body: "hard", mode: 0000},
		{name: "hard-copy", mode: 0000, typeflag: tar.TypeLink, linkname: "hard-target"},
		{name: "private/", mode: 0000},
		{name: "private/value", body: "value", mode: 0000},
	})
	if err := applyLayerWithOwnership(context.Background(), root, bytes.NewReader(layer), owners); err != nil {
		t.Fatalf("applying restrictive image layer: %v", err)
	}
	manifest, err := owners.finalize(root)
	if err != nil {
		t.Fatalf("finalizing restrictive image: %v", err)
	}
	for _, name := range []string{"etc/shadow", "hard-copy", "hard-target", "private/value"} {
		if mode, ok := manifest.LookupMode(name); !ok || mode != 0 {
			t.Errorf("virtual mode for %q = %#o, %t; want 0, true", name, mode, ok)
		}
		if data, err := os.ReadFile(filepath.Join(root, name)); err != nil || len(data) == 0 {
			t.Errorf("host backing for %q is unreadable: data=%q err=%v", name, data, err)
		}
	}
	if mode, ok := manifest.LookupMode("private"); !ok || mode != 0 {
		t.Errorf("virtual directory mode = %#o, %t; want 0, true", mode, ok)
	}
	if info, err := os.Stat(filepath.Join(root, "private")); err != nil || info.Mode().Perm()&0500 != 0500 {
		t.Errorf("host backing directory is not searchable: info=%v err=%v", info, err)
	}
}

func TestPrepareEmptyRootNormalizesGuestRootMode(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := prepareEmptyRoot(root); err != nil {
		t.Fatalf("prepareEmptyRoot: %v", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0755); got != want {
		t.Errorf("root mode=%#o, want %#o", got, want)
	}
}

func TestPrepareEmptyRootRejectsContents(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "existing"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := prepareEmptyRoot(root); err == nil {
		t.Fatal("prepareEmptyRoot unexpectedly accepted a non-empty directory")
	}
}

func TestOwnershipStateTracksLayersAndAllFileTypes(t *testing.T) {
	root := t.TempDir()
	owners := newOwnershipState()
	base := makeLayer(t, false, []tarEntry{
		{name: "etc/", mode: 0755},
		{name: "etc/remove", body: "old", mode: 0644, uid: 10, gid: 11},
		{name: "etc/keep", body: "base", mode: 0644, uid: 20, gid: 21},
		{name: "hard-target", body: "hard", mode: 0644, uid: 30, gid: 31},
		{name: "symbolic", mode: 0777, uid: 40, gid: 41, typeflag: tar.TypeSymlink, linkname: "etc/keep"},
		{name: "synth/child", body: "child", mode: 0644, uid: 50, gid: 51},
		{name: "opaque/", mode: 0755, uid: 70, gid: 71},
		{name: "opaque/lower", body: "lower", mode: 0644, uid: 72, gid: 73},
	})
	if err := applyLayerWithOwnership(context.Background(), root, bytes.NewReader(base), owners); err != nil {
		t.Fatalf("applying base layer: %v", err)
	}
	upper := makeLayer(t, false, []tarEntry{
		{name: "etc/.wh.remove", mode: 0600},
		{name: "etc/keep", body: "upper", mode: 0644, uid: 100, gid: 101},
		{name: "hard-copy", mode: 0644, uid: 60, gid: 61, typeflag: tar.TypeLink, linkname: "hard-target"},
		{name: "opaque/.wh..wh..opq", mode: 0600},
		{name: "opaque/upper", body: "upper", mode: 0644, uid: 80, gid: 81},
	})
	if err := applyLayerWithOwnership(context.Background(), root, bytes.NewReader(upper), owners); err != nil {
		t.Fatalf("applying upper layer: %v", err)
	}
	removeLink := makeLayer(t, false, []tarEntry{{name: ".wh.hard-copy", mode: 0600}})
	if err := applyLayerWithOwnership(context.Background(), root, bytes.NewReader(removeLink), owners); err != nil {
		t.Fatalf("removing hardlink in later layer: %v", err)
	}
	manifest, err := owners.finalize(root)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	want := map[string]ownership.Owner{
		".":            {},
		"etc":          {},
		"etc/keep":     {UID: 100, GID: 101},
		"hard-target":  {UID: 60, GID: 61},
		"symbolic":     {UID: 40, GID: 41},
		"synth":        {},
		"synth/child":  {UID: 50, GID: 51},
		"opaque":       {UID: 70, GID: 71},
		"opaque/upper": {UID: 80, GID: 81},
	}
	for name, owner := range want {
		if got, ok := manifest.Lookup(name); !ok || got != owner {
			t.Errorf("owner[%q]=%+v, %t; want %+v", name, got, ok, owner)
		}
	}
	for _, removed := range []string{"etc/remove", "opaque/lower", "hard-copy"} {
		if got, ok := manifest.Lookup(removed); ok {
			t.Errorf("removed owner[%q]=%+v", removed, got)
		}
	}
	if stat, err := os.Lstat(filepath.Join(root, "etc/keep")); err != nil {
		t.Fatal(err)
	} else if stat.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		t.Errorf("physical uid=%d, want invoking uid %d", stat.Sys().(*syscall.Stat_t).Uid, os.Geteuid())
	}
}

// Replacing a path deletes what was recorded beneath it, including beneath
// directories that no header named, and a replaced hard link leaves its group.
func TestOwnershipStateDeletesSubtreesAndRelinks(t *testing.T) {
	owners := newOwnershipState()
	record := func(name string, typeflag byte, uid int, linkname string) {
		t.Helper()
		if _, err := owners.record(&tar.Header{Name: name, Typeflag: typeflag, Uid: uid, Gid: uid, Linkname: linkname}); err != nil {
			t.Fatalf("recording %q: %v", name, err)
		}
	}
	record("implicit/child/file", tar.TypeReg, 10, "")
	record("implicit/other", tar.TypeReg, 11, "")
	record("kept/file", tar.TypeReg, 12, "")
	record("kept/alias", tar.TypeLink, 13, "kept/file")
	// A file replaces the directory its children implied.
	record("implicit", tar.TypeReg, 14, "")
	// An opaque directory drops what lay beneath it.
	record("kept/.wh..wh..opq", tar.TypeReg, 0, "")
	record("kept/file", tar.TypeReg, 15, "")
	record("kept/alias", tar.TypeReg, 16, "")
	record("kept/file2", tar.TypeLink, 17, "kept/file")
	got := make(map[string]int)
	for name, entry := range owners.entries {
		got[name] = int(entry.owner.UID)
	}
	want := map[string]int{".": 0, "implicit": 14, "kept/file": 17, "kept/alias": 16, "kept/file2": 17}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("owners %v, want %v", got, want)
	}
	group := owners.groups["kept/file"]
	if owners.groups["kept/file2"] != group || owners.groups["kept/alias"] == group {
		t.Errorf("hardlink groups %v", owners.groups)
	}
	if members := owners.members[group]; len(members) != 2 {
		t.Errorf("group of kept/file has members %v, want kept/file and kept/file2", members)
	}
}

// Recording a layer costs what each entry touches, not a scan of every path
// recorded before it: 200,000 files took seven minutes when it did.
func TestOwnershipStateScalesWithTheLayer(t *testing.T) {
	owners := newOwnershipState()
	start := time.Now()
	for i := 0; i < 200000; i++ {
		if _, err := owners.record(&tar.Header{Name: fmt.Sprintf("many/%d/%d", i/1000, i%1000), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := owners.record(&tar.Header{Name: "many/.wh.7", Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("recording 200,000 files took %v", elapsed)
	}
	if got, want := len(owners.entries), 200000-1000+1; got != want {
		t.Errorf("%d paths recorded, want %d", got, want)
	}
}

func makeLayer(t *testing.T, compressed bool, entries []tarEntry) []byte {
	t.Helper()
	var output bytes.Buffer
	var gzipWriter *gzip.Writer
	writer := tar.NewWriter(&output)
	if compressed {
		gzipWriter = gzip.NewWriter(&output)
		writer = tar.NewWriter(gzipWriter)
	}
	for _, entry := range entries {
		typeflag := entry.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
			if entry.name[len(entry.name)-1] == '/' {
				typeflag = tar.TypeDir
			}
		}
		size := int64(0)
		if typeflag == tar.TypeReg {
			size = int64(len(entry.body))
		}
		header := &tar.Header{Name: entry.name, Mode: entry.mode, Size: size, Typeflag: typeflag, Uid: entry.uid, Gid: entry.gid, Linkname: entry.linkname}
		if len(entry.xattrs) != 0 {
			header.PAXRecords = make(map[string]string, len(entry.xattrs))
			for name, value := range entry.xattrs {
				header.PAXRecords["SCHILY.xattr."+name] = value
			}
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if entry.body != "" {
			if _, err := writer.Write([]byte(entry.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if gzipWriter != nil {
		if err := gzipWriter.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return output.Bytes()
}
