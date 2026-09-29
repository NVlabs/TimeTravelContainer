// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCloneOpenFile(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source")
	destinationPath := filepath.Join(dir, "destination")
	if err := os.WriteFile(sourcePath, []byte("source contents"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destinationPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	destination, err := os.OpenFile(destinationPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	destination, cloneErr := cloneOpenFile(destination, source)
	if destination == nil {
		t.Fatalf("cloneOpenFile returned no fallback destination: %v", cloneErr)
	}
	defer destination.Close()
	if cloneErr != nil {
		if errors.Is(cloneErr, unix.ENOTSUP) || errors.Is(cloneErr, unix.EOPNOTSUPP) || errors.Is(cloneErr, unix.EXDEV) {
			t.Skipf("host filesystem does not support cloning: %v", cloneErr)
		}
		t.Fatalf("cloneOpenFile: %v", cloneErr)
	}
	if got, err := os.ReadFile(destinationPath); err != nil || string(got) != "source contents" {
		t.Fatalf("clone contents = %q, %v", got, err)
	}
	if _, err := destination.WriteAt([]byte("branch"), 0); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(sourcePath); err != nil || string(got) != "source contents" {
		t.Fatalf("source changed after clone write: %q, %v", got, err)
	}
}
