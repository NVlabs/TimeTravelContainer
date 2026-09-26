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
