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

package ttcvolume

import (
	"errors"
	"os"

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
