// Copyright 2026 The gVisor Authors.
// Licensed under the Apache License, Version 2.0 (the "License");

package ttcpack

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
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
	if err := Materialize(repository, id, destination); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(destination, "a", "b", "large"))
	if err != nil || !bytes.Equal(got, contents) {
		t.Fatalf("round trip mismatch: err=%v len=%d", err, len(got))
	}
	first, _ := os.Stat(filepath.Join(destination, "a", "b", "large"))
	second, _ := os.Stat(filepath.Join(destination, "hardlink"))
	if !os.SameFile(first, second) {
		t.Fatal("hardlink identity was not preserved")
	}
}

func TestMaterializeMissingFillsAncestorObjectsWithoutReplacingMetadata(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	newest := filepath.Join(root, "newest")
	ancestor := filepath.Join(root, "ancestor")
	for _, dir := range []string{newest, ancestor} {
		if err := os.MkdirAll(filepath.Join(dir, "objects"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(newest, "metadata.json"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newest, "objects", "changed"), []byte("new object"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ancestor, "metadata.json"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ancestor, "objects", "unchanged"), []byte("old object"), 0600); err != nil {
		t.Fatal(err)
	}
	newestID, err := WriteDirectory(newest, repository)
	if err != nil {
		t.Fatal(err)
	}
	ancestorID, err := WriteDirectory(ancestor, repository)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "destination")
	if err := Materialize(repository, newestID, destination); err != nil {
		t.Fatal(err)
	}
	if err := MaterializeMissing(repository, ancestorID, destination); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(destination, "metadata.json")); err != nil || string(got) != "new" {
		t.Fatalf("metadata = %q, %v; want newest", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(destination, "objects", "unchanged")); err != nil || string(got) != "old object" {
		t.Fatalf("ancestor object = %q, %v", got, err)
	}
}
