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

//go:build linux

package pgalloc

import (
	"os"

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

func cloneHostFileRange(dst, src *os.File, srcOffset, length, dstOffset uint64) error {
	clone := unix.FileCloneRange{
		Src_fd: int64(src.Fd()), Src_offset: srcOffset,
		Src_length: length, Dest_offset: dstOffset,
	}
	return unix.IoctlFileCloneRange(int(dst.Fd()), &clone)
}
