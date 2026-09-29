// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ttcsnapshot

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// startWriteback asks the kernel to begin writing file's dirty pages. It is
// only a hint; a later sync is what makes the data durable.
func startWriteback(file *os.File) {
	_ = unix.SyncFileRange(int(file.Fd()), 0, 0, unix.SYNC_FILE_RANGE_WRITE)
}

// cloneFile reflinks source into the open, empty destination. It returns the
// file that holds the destination afterward, which is destination itself.
func cloneFile(source, destination *os.File, _ int, _ string) (*os.File, error) {
	return destination, unix.IoctlFileClone(int(destination.Fd()), int(source.Fd()))
}

func statTimesNsec(stat *syscall.Stat_t) (atime, ctime, mtime int64) {
	return stat.Atim.Sec*1e9 + stat.Atim.Nsec, stat.Ctim.Sec*1e9 + stat.Ctim.Nsec, stat.Mtim.Sec*1e9 + stat.Mtim.Nsec
}

// punchHole is a no-op: Linux filesystems leave the zeros of a sparse write
// or an extending truncate as holes already.
func punchHole(*os.File, int64, int64) {}
