// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package bundle

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// cloneOpenFile replaces destination with an APFS clone of source. clonefile
// requires a nonexistent destination, so callers must adopt the returned file
// even when cloneErr is non-nil: in that case it is a newly-created empty file
// ready for the portable whole-file-copy fallback. The destination is replaced
// at the path its descriptor names now, as F_GETPATH reports it.
func cloneOpenFile(destination, source *os.File) (*os.File, error) {
	var path [unix.PathMax]byte
	if _, _, errno := unix.Syscall(unix.SYS_FCNTL, destination.Fd(), unix.F_GETPATH, uintptr(unsafe.Pointer(&path[0]))); errno != 0 {
		return destination, errno
	}
	destinationPath := unix.ByteSliceToString(path[:])
	if err := destination.Close(); err != nil {
		return nil, err
	}
	if err := unix.Unlink(destinationPath); err != nil {
		return nil, err
	}
	cloneErr := unix.Fclonefileat(int(source.Fd()), unix.AT_FDCWD, destinationPath, unix.CLONE_NOFOLLOW|unix.CLONE_NOOWNERCOPY)
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW
	if cloneErr != nil {
		// Preserve the cross-filesystem and non-APFS fallback contract.
		flags |= unix.O_CREAT | unix.O_EXCL
	}
	fd, openErr := unix.Open(destinationPath, flags, 0600)
	if openErr != nil {
		return nil, errors.Join(cloneErr, openErr)
	}
	return os.NewFile(uintptr(fd), destinationPath), cloneErr
}
