// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kernel

import (
	"fmt"

	"gvisor.dev/gvisor/pkg/sentry/fsimpl/tmpfs"
	"gvisor.dev/gvisor/pkg/sentry/mm"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
)

// ResidentFileRanges returns the MemoryFile ranges of all resident guest pages
// reachable from k: those that its tasks' address spaces map, and the contents
// of tmpfs and shmem regular files, even where no task maps them. Ranges may
// overlap, where processes share pages or a task maps a file's pages; a page
// that several paths reach is resident once.
func (k *Kernel) ResidentFileRanges() ([]pgalloc.CapturedMemoryRange, error) {
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
	var ranges []pgalloc.CapturedMemoryRange
	for memoryManager := range managers {
		ranges = memoryManager.AppendResidentFileRanges(ranges)
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
	return append(ranges, capture.Ranges()...), nil
}
