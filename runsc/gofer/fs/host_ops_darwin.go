// Copyright 2022 The gVisor Authors.
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

//go:build darwin

package fs

import (
	"math"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/lisafs"
)

const (
	darwinUtimeOmit = -2
	darwinMaxPath   = 1024
)

func procSelfFDPath(_ string) string {
	return "/dev/fd"
}

func reopenFD(fd int, flags int) (int, error) {
	var path [darwinMaxPath]byte
	_, _, errno := unix.Syscall(unix.SYS_FCNTL, uintptr(fd), unix.F_GETPATH, uintptr(unsafe.Pointer(&path[0])))
	if errno != 0 {
		return -1, errno
	}
	return unix.Open(unix.ByteSliceToString(path[:]), flags, 0)
}

// fchmodPathFD changes the mode of the file that fd, a path reference,
// refers to, by its name in its parent: Darwin's /dev/fd entries are the
// descriptors, not the files.
func fchmodPathFD(fd *controlFDLisa, mode uint32) error {
	parent, name, err := fd.getParentFD()
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	return unix.Fchmodat(parent, name, mode, 0 /* flags */)
}

func omitTimespec() unix.Timespec {
	return unix.Timespec{Nsec: darwinUtimeOmit}
}

// pathOpenFlagMask identifies control FDs opened only as path references:
// the Darwin substitutes returned by pathOpenFlags for Linux O_PATH.
const pathOpenFlagMask = unix.O_SYMLINK | unix.O_EVTONLY

func pathOpenFlags() []int {
	// O_SYMLINK opens a symlink itself; O_EVTONLY is the closest descriptor
	// capability to Linux O_PATH for sockets and other non-readable nodes.
	return []int{unix.O_SYMLINK, unix.O_EVTONLY}
}

func combineOpenFlags(flags int) int {
	flags |= openFlags
	if flags&unix.O_SYMLINK != 0 {
		// O_SYMLINK already means open the link itself; Darwin rejects its
		// combination with O_NOFOLLOW as ELOOP.
		flags &^= unix.O_NOFOLLOW
	}
	return flags
}

func openPath(name string) (int, error) {
	var err error
	for _, flags := range pathOpenFlags() {
		var fd int
		fd, err = unix.Open(name, combineOpenFlags(flags), 0)
		if err == nil {
			return fd, nil
		}
	}
	return -1, err
}

func openPathAt(dirFD int, name string) (int, error) {
	var err error
	for _, flags := range pathOpenFlags() {
		var fd int
		fd, err = unix.Openat(dirFD, name, combineOpenFlags(flags), 0)
		if err == nil {
			return fd, nil
		}
	}
	return -1, err
}

// openDirAt opens the directory name in dirFD only to walk through it,
// refusing a symbolic link.
func openDirAt(dirFD int, name string) (int, error) {
	return unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_DIRECTORY|openFlags, 0)
}

func mknodAt(dirFD int, name string, mode linux.FileMode, minor, major uint32) error {
	switch mode.FileType() {
	case linux.S_IFREG:
		fd, err := unix.Openat(dirFD, name, unix.O_CREAT|unix.O_EXCL|unix.O_RDONLY|openFlags, uint32(mode&^linux.FileTypeMask))
		if err == nil {
			err = unix.Close(fd)
		}
		return err
	case linux.S_IFCHR:
		if minor == 0 && major == 0 {
			// APFS has no Linux overlayfs whiteout node representation. Upper
			// layers must use their portable whiteout encoding on Darwin.
			return unix.EOPNOTSUPP
		}
	}
	return unix.EPERM
}

func readlinkFD(_ int, hostPath string, dst []byte) (int, error) {
	// Darwin has no AT_EMPTY_PATH equivalent for readlinkat. This path is
	// allowed only in the explicitly trusted gofer mode.
	return unix.Readlink(hostPath, dst)
}

func allocate(fd int, mode, off, length uint64) error {
	if mode & ^uint64(linux.FALLOC_FL_KEEP_SIZE) != 0 {
		return unix.EOPNOTSUPP
	}
	if off > math.MaxInt64 || length > math.MaxInt64 || off+length < off || off+length > math.MaxInt64 {
		return unix.EFBIG
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	end := int64(off + length)
	if end > stat.Size {
		store := &unix.Fstore_t{
			Flags:   unix.F_ALLOCATEALL,
			Posmode: unix.F_PEOFPOSMODE,
			Length:  end - stat.Size,
		}
		if err := unix.FcntlFstore(uintptr(fd), unix.F_PREALLOCATE, store); err != nil {
			return err
		}
		if mode&linux.FALLOC_FL_KEEP_SIZE == 0 {
			return unix.Ftruncate(fd, end)
		}
	}
	return nil
}

func acceptNonblockCloexec(fd, _ int) (int, error) {
	// Darwin has no accept4. Hold ForkLock until the new descriptor is
	// close-on-exec so a concurrent fork cannot inherit it.
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	nfd, _, err := unix.Accept(fd)
	if err != nil {
		return -1, err
	}
	if err := unix.SetNonblock(nfd, true); err != nil {
		_ = unix.Close(nfd)
		return -1, err
	}
	unix.CloseOnExec(nfd)
	return nfd, nil
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
	return unix.Fchown(hostFD, u, g)
}

func fstatTo(hostFD int) (lisafs.Statx, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(hostFD, &stat); err != nil {
		return lisafs.Statx{}, err
	}
	dev := uint64(uint32(stat.Dev))
	rdev := uint64(uint32(stat.Rdev))
	return lisafs.Statx{
		Mask:      linux.STATX_BASIC_STATS | linux.STATX_BTIME,
		Mode:      stat.Mode,
		DevMinor:  unix.Minor(dev),
		DevMajor:  unix.Major(dev),
		Ino:       stat.Ino,
		Nlink:     uint32(stat.Nlink),
		UID:       stat.Uid,
		GID:       stat.Gid,
		RdevMinor: unix.Minor(rdev),
		RdevMajor: unix.Major(rdev),
		Size:      uint64(stat.Size),
		Blksize:   uint32(stat.Blksize),
		Blocks:    uint64(stat.Blocks),
		Atime:     lisafs.StatxTimestamp{Sec: stat.Atim.Sec, Nsec: uint32(stat.Atim.Nsec)},
		Btime:     lisafs.StatxTimestamp{Sec: stat.Btim.Sec, Nsec: uint32(stat.Btim.Nsec)},
		Mtime:     lisafs.StatxTimestamp{Sec: stat.Mtim.Sec, Nsec: uint32(stat.Mtim.Nsec)},
		Ctime:     lisafs.StatxTimestamp{Sec: stat.Ctim.Sec, Nsec: uint32(stat.Ctim.Nsec)},
	}, nil
}

func statFS(hostFD int) (lisafs.StatFS, error) {
	var stat unix.Statfs_t
	if err := unix.Fstatfs(hostFD, &stat); err != nil {
		return lisafs.StatFS{}, err
	}
	return lisafs.StatFS{
		Type:            uint64(stat.Type),
		BlockSize:       int64(stat.Bsize),
		Blocks:          stat.Blocks,
		BlocksFree:      stat.Bfree,
		BlocksAvailable: stat.Bavail,
		Files:           stat.Files,
		FilesFree:       stat.Ffree,
		NameLength:      255,
	}, nil
}

func statDeviceMajor(stat unix.Stat_t) uint32 {
	return unix.Major(uint64(uint32(stat.Dev)))
}

func statDeviceMinor(stat unix.Stat_t) uint32 {
	return unix.Minor(uint64(uint32(stat.Dev)))
}

// isThreadGroupLeader is never asked on macOS, whose gofer does not set a
// connection's supplementary groups (specutils.SetgroupsAllowed). No thread
// counts as the leader, so that connectOnItsOwnThread could not recurse.
func isThreadGroupLeader() bool {
	return false
}
