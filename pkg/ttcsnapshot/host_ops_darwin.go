// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package ttcsnapshot

import (
	"errors"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// startWriteback is a no-op: Darwin has no sync_file_range, and the hint is
// not needed for durability.
func startWriteback(*os.File) {}

// createEmptyFile creates the empty regular file path, relative to
// directoryFD, which must not exist. Darwin's mknodat makes no regular files.
func createEmptyFile(directoryFD int, path string) error {
	fd, err := unix.Openat(directoryFD, path, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	return unix.Close(fd)
}

// cloneFile uses APFS clonefile semantics. Darwin requires the destination
// not to exist, whereas Linux FICLONE fills an already-open destination, so
// replace the empty destination at destinationPath (relative to
// destinationDirFD) and return a read-write file for it. On a failed clone the
// returned file is a new empty destination, so callers may fall back to
// copying; it is nil only if no destination could be reopened.
func cloneFile(source, destination *os.File, destinationDirFD int, destinationPath string) (*os.File, error) {
	if err := destination.Close(); err != nil {
		return nil, err
	}
	if err := unix.Unlinkat(destinationDirFD, destinationPath, 0); err != nil {
		return nil, err
	}
	cloneErr := unix.Fclonefileat(int(source.Fd()), destinationDirFD, destinationPath, unix.CLONE_NOFOLLOW|unix.CLONE_NOOWNERCOPY)
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW
	if cloneErr != nil {
		flags |= unix.O_CREAT | unix.O_EXCL
	}
	fd, openErr := unix.Openat(destinationDirFD, destinationPath, flags, 0600)
	if openErr != nil {
		return nil, errors.Join(cloneErr, openErr)
	}
	return os.NewFile(uintptr(fd), destinationPath), cloneErr
}

func statTimesNsec(stat *syscall.Stat_t) (atime, ctime, mtime int64) {
	return stat.Atimespec.Sec*1e9 + stat.Atimespec.Nsec, stat.Ctimespec.Sec*1e9 + stat.Ctimespec.Nsec, stat.Mtimespec.Sec*1e9 + stat.Mtimespec.Nsec
}

// darwinPunchhole is struct fpunchhole from <sys/fcntl.h>.
type darwinPunchhole struct {
	flags    uint32
	reserved uint32
	offset   int64
	length   int64
}

// punchHole deallocates the whole blocks of file in [offset, offset+length),
// which must read as zeros. APFS allocates the zeros of an extending write or
// truncate, so sparse objects need their holes made explicitly. It is best
// effort: a filesystem without F_PUNCHHOLE keeps the zeros allocated.
func punchHole(file *os.File, offset, length int64) {
	const block = 4096
	start := (offset + block - 1) &^ (block - 1)
	end := (offset + length) &^ (block - 1)
	if end <= start {
		return
	}
	hole := darwinPunchhole{offset: start, length: end - start}
	_, _, _ = unix.Syscall(unix.SYS_FCNTL, file.Fd(), unix.F_PUNCHHOLE, uintptr(unsafe.Pointer(&hole)))
}
