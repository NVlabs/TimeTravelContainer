// Copyright 2021 The gVisor Authors.
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

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package fs

import (
	"strconv"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/lisafs"
)

func procSelfFDPath(path string) string {
	return path
}

func reopenFD(fd int, flags int) (int, error) {
	return unix.Openat(int(procSelfFD.FD()), strconv.Itoa(fd), flags, 0)
}

// fchmodPathFD changes the mode of the file that fd, a path reference,
// refers to: fchmodat(2) of its /proc/self/fd entry follows it to the file.
func fchmodPathFD(fd *controlFDLisa, mode uint32) error {
	return unix.Fchmodat(int(procSelfFD.FD()), strconv.Itoa(fd.hostFD), mode, 0 /* flags */)
}

func omitTimespec() unix.Timespec {
	return unix.Timespec{Nsec: unix.UTIME_OMIT}
}

// pathOpenFlagMask identifies control FDs opened only as path references.
const pathOpenFlagMask = unix.O_PATH

func pathOpenFlags() []int {
	return []int{unix.O_PATH}
}

func combineOpenFlags(flags int) int {
	return flags | openFlags
}

func openPath(name string) (int, error) {
	return unix.Open(name, unix.O_PATH|openFlags, 0)
}

func openPathAt(dirFD int, name string) (int, error) {
	return unix.Openat(dirFD, name, unix.O_PATH|openFlags, 0)
}

// openDirAt opens the directory name in dirFD only to walk through it,
// refusing a symbolic link.
func openDirAt(dirFD int, name string) (int, error) {
	return unix.Openat(dirFD, name, unix.O_PATH|unix.O_DIRECTORY|openFlags, 0)
}

func mknodAt(dirFD int, name string, mode linux.FileMode, _, _ uint32) error {
	return unix.Mknodat(dirFD, name, uint32(mode), 0)
}

func readlinkFD(fd int, _ string, dst []byte) (int, error) {
	return unix.Readlinkat(fd, "", dst)
}

func allocate(fd int, mode, off, length uint64) error {
	return unix.Fallocate(fd, uint32(mode), int64(off), int64(length))
}

func acceptNonblockCloexec(fd, flags int) (int, error) {
	nfd, _, err := unix.Accept4(fd, flags)
	return nfd, err
}

func fchown(hostFD int, uid lisafs.UID, gid lisafs.GID) error {
	if !uid.Ok() && !gid.Ok() {
		return nil
	}
	u := -1
	g := -1
	if uid.Ok() {
		u = int(uid)
	}
	if gid.Ok() {
		g = int(gid)
	}
	return unix.Fchownat(hostFD, "", u, g, unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW)
}

func fstatTo(hostFD int) (lisafs.Statx, error) {
	var stat unix.Statx_t
	if err := unix.Statx(hostFD, "", unix.AT_EMPTY_PATH, linux.STATX_BASIC_STATS|linux.STATX_BTIME, &stat); err != nil {
		if err != unix.ENOSYS {
			return lisafs.Statx{}, err
		}
		var legacy unix.Stat_t
		if err := unix.Fstat(hostFD, &legacy); err != nil {
			return lisafs.Statx{}, err
		}
		return lisafs.Statx{
			Mask:      linux.STATX_BASIC_STATS,
			Mode:      uint16(legacy.Mode),
			DevMinor:  uint32(unix.Minor(legacy.Dev)),
			DevMajor:  uint32(unix.Major(legacy.Dev)),
			Ino:       legacy.Ino,
			Nlink:     uint32(legacy.Nlink),
			UID:       legacy.Uid,
			GID:       legacy.Gid,
			RdevMinor: uint32(unix.Minor(legacy.Rdev)),
			RdevMajor: uint32(unix.Major(legacy.Rdev)),
			Size:      uint64(legacy.Size),
			Blksize:   uint32(legacy.Blksize),
			Blocks:    uint64(legacy.Blocks),
			Atime:     lisafs.StatxTimestamp{Sec: legacy.Atim.Sec, Nsec: uint32(legacy.Atim.Nsec)},
			Mtime:     lisafs.StatxTimestamp{Sec: legacy.Mtim.Sec, Nsec: uint32(legacy.Mtim.Nsec)},
			Ctime:     lisafs.StatxTimestamp{Sec: legacy.Ctim.Sec, Nsec: uint32(legacy.Ctim.Nsec)},
		}, nil
	}
	return lisafs.Statx{
		Mask:      stat.Mask,
		Mode:      stat.Mode,
		DevMinor:  stat.Dev_minor,
		DevMajor:  stat.Dev_major,
		Ino:       stat.Ino,
		Nlink:     stat.Nlink,
		UID:       stat.Uid,
		GID:       stat.Gid,
		RdevMinor: stat.Rdev_minor,
		RdevMajor: stat.Rdev_major,
		Size:      stat.Size,
		Blksize:   stat.Blksize,
		Blocks:    stat.Blocks,
		Atime:     lisafs.StatxTimestamp{Sec: stat.Atime.Sec, Nsec: stat.Atime.Nsec},
		Btime:     lisafs.StatxTimestamp{Sec: stat.Btime.Sec, Nsec: stat.Btime.Nsec},
		Mtime:     lisafs.StatxTimestamp{Sec: stat.Mtime.Sec, Nsec: stat.Mtime.Nsec},
		Ctime:     lisafs.StatxTimestamp{Sec: stat.Ctime.Sec, Nsec: stat.Ctime.Nsec},
	}, nil
}

func statFS(hostFD int) (lisafs.StatFS, error) {
	var stat unix.Statfs_t
	if err := unix.Fstatfs(hostFD, &stat); err != nil {
		return lisafs.StatFS{}, err
	}
	return lisafs.StatFS{
		Type:            uint64(stat.Type),
		BlockSize:       stat.Bsize,
		Blocks:          stat.Blocks,
		BlocksFree:      stat.Bfree,
		BlocksAvailable: stat.Bavail,
		Files:           stat.Files,
		FilesFree:       stat.Ffree,
		NameLength:      uint64(stat.Namelen),
	}, nil
}

func statDeviceMajor(stat unix.Stat_t) uint32 {
	return uint32(unix.Major(stat.Dev))
}

func statDeviceMinor(stat unix.Stat_t) uint32 {
	return uint32(unix.Minor(stat.Dev))
}

// isThreadGroupLeader returns true if the calling thread is the thread group
// leader, whose thread ID is the process ID.
func isThreadGroupLeader() bool {
	return unix.Gettid() == unix.Getpid()
}
