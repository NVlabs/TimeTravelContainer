// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package ttcvolume

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// cloneFile uses APFS clonefile semantics. Darwin requires the destination not
// to exist, whereas Linux FICLONE operates on an already-open destination, so
// replace the probe/reservation file and return an FD for the new clone.
func cloneFile(source, destination *os.File, destinationDirFD int, destinationPath string) (*os.File, error) {
	if err := destination.Close(); err != nil {
		return destination, err
	}
	if err := unix.Unlinkat(destinationDirFD, destinationPath, 0); err != nil {
		return nil, err
	}
	cloneErr := unix.Fclonefileat(int(source.Fd()), destinationDirFD, destinationPath, unix.CLONE_NOFOLLOW|unix.CLONE_NOOWNERCOPY)
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW
	if cloneErr != nil {
		// Leave a usable empty file behind so callers that permit full-copy
		// fallback can continue without a second host-specific path.
		flags |= unix.O_CREAT | unix.O_EXCL
	}
	fd, openErr := unix.Openat(destinationDirFD, destinationPath, flags, 0600)
	if openErr != nil {
		return nil, errors.Join(cloneErr, openErr)
	}
	return os.NewFile(uintptr(fd), destinationPath), cloneErr
}

// Darwin has no fdatasync. fsync is a valid, stronger implementation of the
// data-only durability contract.
func syncFileData(file *os.File) error {
	return file.Sync()
}

// allocateFile implements Linux fallocate(mode=0) semantics as closely as
// Darwin permits: reserve additional blocks from the physical EOF, then grow
// the logical size to the requested end. Existing ranges need no logical
// change and APFS may already represent them sparsely.
func allocateFile(file *os.File, offset, length int64) error {
	end := offset + length
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if end > info.Size() {
		store := &unix.Fstore_t{
			Flags:   unix.F_ALLOCATEALL,
			Posmode: unix.F_PEOFPOSMODE,
			Length:  end - info.Size(),
		}
		if err := unix.FcntlFstore(file.Fd(), unix.F_PREALLOCATE, store); err != nil {
			return err
		}
	}
	return file.Truncate(end)
}

// directoryFDPath returns a path that names the open directory dir. Darwin
// has no /proc/self/fd directory traversal; F_GETPATH recovers the path,
// which is valid because the trusted-mode Sentry shares the host namespace.
func directoryFDPath(dir *os.File) (string, error) {
	var path [unix.PathMax]byte
	if _, _, errno := unix.Syscall(unix.SYS_FCNTL, dir.Fd(), unix.F_GETPATH, uintptr(unsafe.Pointer(&path[0]))); errno != 0 {
		return "", errno
	}
	return unix.ByteSliceToString(path[:]), nil
}

func hostCapacity(dir *os.File) (Capacity, error) {
	var st unix.Statfs_t
	if err := unix.Fstatfs(int(dir.Fd()), &st); err != nil {
		return Capacity{}, err
	}
	size := uint64(st.Bsize)
	return Capacity{
		TotalBytes:     st.Blocks * size,
		FreeBytes:      st.Bfree * size,
		AvailableBytes: st.Bavail * size,
		TotalFiles:     st.Files,
		FreeFiles:      st.Ffree,
	}, nil
}
