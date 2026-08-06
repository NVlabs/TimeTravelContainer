// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tmpfs

import (
	"testing"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/ttcvolume"
)

func TestDiskVolumeWriteMetadataIsCoalesced(t *testing.T) {
	manager, err := ttcvolume.OpenManager(t.TempDir(), true)
	if err != nil {
		t.Fatalf("OpenManager: %v", err)
	}
	repository, err := manager.Create("writeback-test")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer repository.Close()
	ino, err := repository.CreateRegular(ttcvolume.MainBranch, ttcvolume.RootInode, "download", 0644, 0, 0)
	if err != nil {
		t.Fatalf("CreateRegular: %v", err)
	}
	branchBefore, err := repository.Branch(ttcvolume.MainBranch)
	if err != nil {
		t.Fatal(err)
	}

	fs := &filesystem{
		diskVolume:    &diskVolumeState{Repository: repository, Branch: ttcvolume.MainBranch},
		liveForkFiles: make(map[*regularFile]struct{}),
	}
	file := &regularFile{}
	file.inode.fs = fs
	file.inode.impl = file
	file.inode.ino = ino
	file.inode.mode.Store(uint32(linux.S_IFREG | 0644))
	file.inode.nlink.Store(1)
	file.size.Store(8 << 20)
	fs.liveForkFiles[file] = struct{}{}

	file.inode.mu.Lock()
	file.markDiskMetadataDirtyLocked()
	file.inode.mu.Unlock()
	meta, err := repository.Inode(ttcvolume.MainBranch, ino)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Size != 0 {
		t.Fatalf("metadata was published before a flush: size=%d", meta.Size)
	}
	branchDirty, err := repository.Branch(ttcvolume.MainBranch)
	if err != nil {
		t.Fatal(err)
	}
	if branchDirty.Root != branchBefore.Root {
		t.Fatalf("ordinary write changed repository root: before=%q after=%q", branchBefore.Root, branchDirty.Root)
	}

	if err := fs.flushDiskWriteMetadata(); err != nil {
		t.Fatalf("flushDiskWriteMetadata: %v", err)
	}
	meta, err = repository.Inode(ttcvolume.MainBranch, ino)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Size != 8<<20 {
		t.Fatalf("flushed size=%d, want %d", meta.Size, 8<<20)
	}
	if file.diskMetadataDirty.Load() != 0 {
		t.Fatal("successful flush left inode dirty")
	}
	branchFlushed, err := repository.Branch(ttcvolume.MainBranch)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.flushDiskWriteMetadata(); err != nil {
		t.Fatalf("second flushDiskWriteMetadata: %v", err)
	}
	branchAgain, err := repository.Branch(ttcvolume.MainBranch)
	if err != nil {
		t.Fatal(err)
	}
	if branchAgain.Root != branchFlushed.Root {
		t.Fatalf("clean flush rewrote repository root: before=%q after=%q", branchFlushed.Root, branchAgain.Root)
	}
}
