// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package copyhelper

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	source := filepath.Join(t.TempDir(), "tree")
	if err := os.MkdirAll(filepath.Join(source, "nested"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "value"), []byte("payload\x00data"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nested/value", filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	var stream bytes.Buffer
	if err := WriteArchive(source, false, &stream); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	if err := ExtractArchive(&stream, destination, false, os.Geteuid(), os.Getegid(), false); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(destination, "tree", "nested", "value"))
	if err != nil || string(got) != "payload\x00data" {
		t.Fatalf("round-trip value=%q err=%v", got, err)
	}
	if got, err := os.Readlink(filepath.Join(destination, "tree", "link")); err != nil || got != "nested/value" {
		t.Fatalf("round-trip link=%q err=%v", got, err)
	}
}

func TestRejectsTraversalAndSymlinkAncestor(t *testing.T) {
	for _, test := range []struct {
		name    string
		headers []*tar.Header
	}{
		{"traversal", []*tar.Header{{Name: "../escape", Typeflag: tar.TypeReg, Mode: 0644, Size: 1}}},
		{"symlink ancestor", []*tar.Header{
			{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../outside", Mode: 0777},
			{Name: "link/payload", Typeflag: tar.TypeReg, Mode: 0644, Size: 1},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stream bytes.Buffer
			writer := tar.NewWriter(&stream)
			for _, header := range test.headers {
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
			if err := os.Mkdir(destination, 0755); err != nil {
				t.Fatal(err)
			}
			if err := ExtractArchive(&stream, destination, false, os.Geteuid(), os.Getegid(), true); err == nil {
				t.Fatal("unsafe archive unexpectedly extracted")
			}
		})
	}
}

func TestRunValidation(t *testing.T) {
	var stderr bytes.Buffer
	if status := Run(nil, bytes.NewReader(nil), &bytes.Buffer{}, &stderr); status != 2 {
		t.Fatalf("Run(nil) status=%d, stderr=%q", status, stderr.String())
	}
	stderr.Reset()
	if status := Run([]string{"unknown"}, bytes.NewReader(nil), &bytes.Buffer{}, &stderr); status != 1 {
		t.Fatalf("Run(unknown) status=%d, stderr=%q", status, stderr.String())
	}
}
