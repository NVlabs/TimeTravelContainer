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

//go:build darwin

package fsutil

import (
	"bytes"
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
)

// UnixDirentMaxSize is the maximum size of unix.Dirent in bytes.
var UnixDirentMaxSize = int(unsafe.Sizeof(unix.Dirent{}))

func readDirents(fd int, buf []byte) (int, error) {
	var base uintptr
	return unix.Getdirentries(fd, buf, &base)
}

// Utimensat implements the gofer's descriptor-relative timestamp update.
func Utimensat(dirFD int, name string, times [2]unix.Timespec, flags int) error {
	if name != "" {
		return unix.UtimesNanoAt(dirFD, name, times[:], flags)
	}
	if flags != 0 {
		return unix.EINVAL
	}
	// x/sys/unix does not expose Darwin's futimens wrapper. fdescfs resolves
	// this path to the already-open file, preserving descriptor identity while
	// retaining nanosecond timestamps and UTIME_NOW/UTIME_OMIT semantics.
	return unix.UtimesNano(fmt.Sprintf("/dev/fd/%d", dirFD), times[:])
}

// RenameAt2 translates Linux renameat2 flags to Darwin renameatx_np.
func RenameAt2(oldDirFD int, oldName string, newDirFD int, newName string, flags uint32) error {
	if flags == 0 {
		return unix.Renameat(oldDirFD, oldName, newDirFD, newName)
	}
	var darwinFlags uint32
	if flags&linux.RENAME_NOREPLACE != 0 {
		darwinFlags |= unix.RENAME_EXCL
		flags &^= linux.RENAME_NOREPLACE
	}
	if flags&linux.RENAME_EXCHANGE != 0 {
		darwinFlags |= unix.RENAME_SWAP
		flags &^= linux.RENAME_EXCHANGE
	}
	if flags&linux.RENAME_WHITEOUT != 0 {
		return unix.EOPNOTSUPP
	}
	if flags != 0 {
		return unix.EINVAL
	}
	return unix.RenameatxNp(oldDirFD, oldName, newDirFD, newName, darwinFlags)
}

// StatAt is a convenience wrapper around fstatat(2).
func StatAt(dirFD int, name string) (unix.Stat_t, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(dirFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return unix.Stat_t{}, err
	}
	return stat, nil
}

// ParseDirents parses Darwin getdirentries(2) records.
func ParseDirents(buf []byte, handleDirent DirentHandler) {
	for len(buf) >= int(unsafe.Sizeof(unix.Dirent{}.Reclen)) {
		dirent := *(*unix.Dirent)(unsafe.Pointer(&buf[0]))
		recordLength := int(dirent.Reclen)
		nameOffset := int(unsafe.Offsetof(dirent.Name))
		if recordLength <= nameOffset || recordLength > len(buf) {
			return
		}
		nameBuf := buf[nameOffset:recordLength]
		nameLength := bytes.IndexByte(nameBuf, 0)
		if nameLength < 0 {
			nameLength = len(nameBuf)
		}
		name := string(nameBuf[:nameLength])
		buf = buf[recordLength:]
		if name == "." || name == ".." {
			continue
		}
		handleDirent(dirent.Ino, int64(dirent.Seekoff), dirent.Type, name, dirent.Reclen)
	}
}
