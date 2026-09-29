// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kernel

import (
	"fmt"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/tmpfs"
	"gvisor.dev/gvisor/pkg/sentry/mm"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
)

// ResidentPageVersions returns the physical-and-logical identities of all
// resident guest pages reachable from k. This includes pages mapped by tasks
// and tmpfs/shmem regular-file contents, even when the latter are not mapped.
// Multiple graph paths to the same resident page contribute one identity.
func (k *Kernel) ResidentPageVersions() (map[mm.ResidentPageVersion]struct{}, error) {
	managers := make(map[*mm.MemoryManager]struct{})
	ts := k.TaskSet()
	ts.mu.RLock()
	for tg := range k.RootPIDNamespace().tgids {
		tg.ForEachTaskLocked(func(task *Task) bool {
			if memoryManager := task.MemoryManager(); memoryManager != nil {
				if _, exists := managers[memoryManager]; !exists && memoryManager.IncUsers() {
					managers[memoryManager] = struct{}{}
				}
			}
			return true
		})
	}
	ts.mu.RUnlock()
	defer func() {
		for memoryManager := range managers {
			memoryManager.DecUsers(k.SupervisorContext())
		}
	}()
	pages := make(map[mm.ResidentPageVersion]struct{})
	for memoryManager := range managers {
		for page := range memoryManager.ResidentPageVersions() {
			pages[page] = struct{}{}
		}
	}

	// A tmpfs file can retain resident memory without being mapped by a task.
	// This is the common case for /dev/shm after a writer closes the file. The
	// VFS filesystem registry and tmpfs live-file registry let us enumerate
	// these allocations without walking directory trees; CaptureMemoryFileRanges
	// also includes unlinked files that remain reachable through an FD or VMA.
	ctx := k.SupervisorContext()
	filesystems := k.vfs.GetFilesystems()
	capture := pgalloc.NewMemoryRangeCapture()
	for fsIndex, fs := range filesystems {
		// Disk-backed TTC volumes own their data in host files and are excluded
		// by tmpfs.CaptureMemoryFileRanges.
		if err := tmpfs.CaptureMemoryFileRanges(fs, capture); err != nil {
			for _, remaining := range filesystems[fsIndex:] {
				remaining.DecRef(ctx)
			}
			return nil, fmt.Errorf("capturing resident filesystem memory: %w", err)
		}
		fs.DecRef(ctx)
	}
	for _, captured := range capture.Ranges() {
		for _, run := range captured.MemoryFile.LogicalPageRanges(captured.Range) {
			count := run.Range.Length() / hostarch.PageSize
			for index := uint64(0); index < count; index++ {
				pages[mm.ResidentPageVersion{
					File: captured.MemoryFile,
					Version: pgalloc.PageVersionID{
						Allocator: run.FirstVersion.Allocator,
						Sequence:  run.FirstVersion.Sequence + index,
					},
				}] = struct{}{}
			}
		}
	}
	return pages, nil
}
