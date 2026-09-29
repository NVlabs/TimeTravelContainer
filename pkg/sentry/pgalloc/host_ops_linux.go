// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package pgalloc

import (
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
)

func adviseChunkMapping(addr, length uintptr, huge, adviseHuge, adviseNoHuge bool) {
	advice := 0
	name := ""
	if huge && adviseHuge {
		advice = unix.MADV_HUGEPAGE
		name = "MADV_HUGEPAGE"
	} else if !huge && adviseNoHuge {
		advice = unix.MADV_NOHUGEPAGE
		name = "MADV_NOHUGEPAGE"
	} else {
		return
	}
	if _, _, errno := unix.Syscall(unix.SYS_MADVISE, addr, length, uintptr(advice)); errno != 0 {
		log.Warningf("madvise(%#x, %d, %s) failed: %s", addr, length, name, errno)
	}
}

var madvPopulateWriteDisabled atomicbitops.Uint32

func canPopulateMadv() bool { return madvPopulateWriteDisabled.Load() == 0 }

func tryPopulateMadv(b safemem.Block) bool {
	if madvPopulateWriteDisabled.Load() != 0 {
		return false
	}
	if b.Len() <= hostarch.PageSize {
		return true
	}
	_, _, errno := unix.Syscall(unix.SYS_MADVISE, b.Addr(), uintptr(b.Len()), unix.MADV_POPULATE_WRITE)
	if errno != 0 {
		if errno == unix.EINVAL {
			log.Infof("Disabling pgalloc.MemoryFile.AllocateAndFill pre-population: madvise failed: %s", errno)
		} else {
			log.Warningf("Disabling pgalloc.MemoryFile.AllocateAndFill pre-population: madvise failed: %s", errno)
		}
		madvPopulateWriteDisabled.Store(1)
		return false
	}
	return true
}

func commitHostFile(fd int, fr memmap.FileRange) error {
	return unix.Fallocate(fd, 0, int64(fr.Start), int64(fr.Length()))
}

func decommitHostFile(fd int, fr memmap.FileRange) error {
	return unix.Fallocate(fd, unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, int64(fr.Start), int64(fr.Length()))
}
