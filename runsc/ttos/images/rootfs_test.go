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
	"testing"
)

type tarEntry struct {
	name string
	body string
	mode int64
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

func TestPrepareEmptyRootRejectsContents(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "existing"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := prepareEmptyRoot(root); err == nil {
		t.Fatal("prepareEmptyRoot unexpectedly accepted a non-empty directory")
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
		typeflag := byte(tar.TypeReg)
		if entry.name[len(entry.name)-1] == '/' {
			typeflag = tar.TypeDir
		}
		header := &tar.Header{Name: entry.name, Mode: entry.mode, Size: int64(len(entry.body)), Typeflag: typeflag}
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
