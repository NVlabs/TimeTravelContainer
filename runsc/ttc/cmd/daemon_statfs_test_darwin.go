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

//go:build darwin

package cmd

import (
	"testing"

	"golang.org/x/sys/unix"
)

const testLinuxHost = false

func setTestNFSStatfs(statfs *unix.Statfs_t) {
	setTestFilesystemName(statfs, "nfs")
}

func setTestLocalStatfs(statfs *unix.Statfs_t) {
	setTestFilesystemName(statfs, "apfs")
}

func setTestFilesystemName(statfs *unix.Statfs_t, name string) {
	for i := range statfs.Fstypename {
		statfs.Fstypename[i] = 0
	}
	for i := 0; i < len(name) && i < len(statfs.Fstypename); i++ {
		statfs.Fstypename[i] = name[i]
	}
}

func TestCurrentNodeMemoryBytesDarwin(t *testing.T) {
	resident, err := currentNodeMemoryBytes("")
	if err != nil {
		t.Fatalf("currentNodeMemoryBytes: %v", err)
	}
	if resident == 0 {
		t.Fatal("currentNodeMemoryBytes returned zero")
	}
}
