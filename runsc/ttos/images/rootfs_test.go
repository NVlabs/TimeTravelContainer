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
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"gvisor.dev/gvisor/runsc/ttos/ownership"
)

type tarEntry struct {
	name     string
	body     string
	mode     int64
	uid      int
	gid      int
	typeflag byte
	linkname string
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
