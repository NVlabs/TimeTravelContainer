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

// Package fsgofer provides a lisafs server implementation which gives access
// to local files.
package fsgofer

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"sync"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/cleanup"
	rwfd "gvisor.dev/gvisor/pkg/fd"
	"gvisor.dev/gvisor/pkg/fsutil"
	"gvisor.dev/gvisor/pkg/lisafs"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/marshal/primitive"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/ttc/ownership"
)

// LINT.IfChange

const (
	openFlags = unix.O_NOFOLLOW | unix.O_CLOEXEC

	// UNIX_PATH_MAX as defined in include/uapi/linux/un.h.
	unixPathMax = 108
)

// Config holds configuration options for the fsgofer server.
type Config struct {
	// HostUDS signals whether the gofer can connect to host unix domain sockets.
	HostUDS config.HostUDS

	// HostFifo signals whether the gofer can connect to host FIFOs.
	HostFifo config.HostFifo

	// CharDevicePolicy signals whether the gofer can open host character
	// devices.
	CharDevicePolicy config.CharacterDevicePolicy

	// DonateMountPointFD indicates whether a host FD to the mount point should
	// be donated to the client on Mount RPC.
	DonateMountPointFD bool

	// Gofer process's RUID.
	RUID int

	// Gofer process's EUID.
	EUID int

	// Gofer process's RGID.
	RGID int

	// Gofer process's EGID.
	EGID int

	// VirtualOwners overrides host UID/GID values for an immutable TTC image
	// root. It is nil for ordinary gofer mounts.
	VirtualOwners *ownership.Manifest

	// TrustedRootOwnership makes the invoking host user and group appear as
	// Linux root, and translates guest root ownership requests back to those
	// host IDs. The explicitly unsafe gofer, which no user namespace maps,
	// uses it on Darwin and Linux to give the ownership view that a Linux user
	// namespace gives a rootless container: so that guest root may write a
	// writable bind of a host path that the invoking user owns.
	TrustedRootOwnership bool

	// LazyFiles supplies immutable whole-file data for a skeleton rootfs.
	// Ensure must complete before a regular file descriptor is exposed to the
	// client; Metadata supplies the size/mode while its host placeholder is
	// still empty and writable.
	LazyFiles LazyFileProvider
}

// LazyFileProvider mediates pack-backed immutable regular files.
type LazyFileProvider interface {
	Metadata(virtualPath string) (size int64, mode uint32, ok bool)
	Ensure(virtualPath string) error
}

var procSelfFD *rwfd.FD

// OpenProcSelfFD opens the /proc/self/fd directory, which will be used to
// reopen file descriptors.
func OpenProcSelfFD(path string) error {
	path = procSelfFDPath(path)
	d, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return fmt.Errorf("error opening /proc/self/fd: %v", err)
	}
	procSelfFD = rwfd.New(d)
	return nil
}

// ConnectionOpts returns the lisafs.ConnectionOpts for fsgofer.
func ConnectionOpts(readonly bool) lisafs.ConnectionOpts {
	return lisafs.ConnectionOpts{
		Readonly:          readonly,
		WalkStatSupported: true,
		SetAttrOnDeleted:  true,
		AllocateOnDeleted: true,
		OpenOnDeleted:     true,
	}
}

// NewConnectionImpl returns a new lisafs.ConnectionImpl for fsgofer.
func NewConnectionImpl(config *Config) lisafs.ConnectionImpl {
	return &connectionImpl{config: config}
}

// connectionImpl implements lisafs.ConnectionImpl for fsgofer.
type connectionImpl struct {
	// config is the global configuration for the gofer.
	config *Config
}

var _ lisafs.ConnectionImpl = (*connectionImpl)(nil)

func (i *connectionImpl) stat(hostFD int, virtualPath string) (lisafs.Statx, error) {
	stat, err := fstatTo(hostFD)
	if err != nil {
		return lisafs.Statx{}, err
	}
	stat.UID, stat.GID = mapHostOwnerToGuest(i.config, stat.UID, stat.GID)
	if i.config.LazyFiles != nil {
		if size, mode, ok := i.config.LazyFiles.Metadata(virtualPath); ok {
			stat.Mask |= linux.STATX_SIZE | linux.STATX_MODE
			stat.Size = uint64(size)
			stat.Mode = stat.Mode&unix.S_IFMT | uint16(mode&07777)
		}
	}
	if owner, ok := i.config.VirtualOwners.Lookup(virtualPath); ok {
		stat.Mask |= linux.STATX_UID | linux.STATX_GID
		stat.UID = owner.UID
		stat.GID = owner.GID
	}
	if mode, ok := i.config.VirtualOwners.LookupMode(virtualPath); ok {
		stat.Mask |= linux.STATX_MODE
		stat.Mode = stat.Mode&unix.S_IFMT | uint16(mode)
	}
	return stat, nil
}

// mapHostOwnerToGuest returns the owner that the guest sees of a host file that
// uid and gid own: see Config.TrustedRootOwnership.
func mapHostOwnerToGuest(config *Config, uid, gid uint32) (uint32, uint32) {
	if config == nil || !config.TrustedRootOwnership {
		return uid, gid
	}
	if int(uid) == config.RUID || int(uid) == config.EUID {
		uid = 0
	}
	if int(gid) == config.RGID || int(gid) == config.EGID {
		gid = 0
	}
	return uid, gid
}

// mapGuestOwnerToHost returns the host owner of a file that the guest asks
// uid and gid to own: see Config.TrustedRootOwnership.
func mapGuestOwnerToHost(config *Config, uid lisafs.UID, gid lisafs.GID) (lisafs.UID, lisafs.GID) {
	if config == nil || !config.TrustedRootOwnership {
		return uid, gid
	}
	if uid.Ok() && uid == 0 {
		uid = lisafs.UID(config.RUID)
	}
	if gid.Ok() && gid == 0 {
		gid = lisafs.GID(config.RGID)
	}
	return uid, gid
}

func (i *connectionImpl) fchown(hostFD int, uid lisafs.UID, gid lisafs.GID) error {
	uid, gid = mapGuestOwnerToHost(i.config, uid, gid)
	return fchown(hostFD, uid, gid)
}

func (i *connectionImpl) virtualXattr(virtualPath, name string) ([]byte, bool) {
	if i == nil || i.config == nil {
		return nil, false
	}
	return i.config.VirtualOwners.LookupXattr(virtualPath, name)
}

func (i *connectionImpl) virtualXattrNames(virtualPath string) []string {
	if i == nil || i.config == nil {
		return nil
	}
	return i.config.VirtualOwners.ListXattrs(virtualPath)
}

// Mount implements lisafs.ConnectionImpl.Mount.
//
// +checklocksread:c.server.renameMu
// +checklocksexclude:c.fdsMu
func (i *connectionImpl) Mount(c *lisafs.Connection, mountNode *lisafs.Node) (*lisafs.ControlFD, lisafs.Statx, int, error) {
	mountPath := mountNode.FilePath()
	rootHostFD, rootFlags, err := tryOpen(func(flags int) (int, error) {
		return unix.Open(mountPath, flags, 0)
	})
	if err != nil {
		return nil, lisafs.Statx{}, -1, err
	}
	cu := cleanup.Make(func() {
		_ = unix.Close(rootHostFD)
	})
	defer cu.Clean()

	stat, err := i.stat(rootHostFD, ".")
	if err != nil {
		return nil, lisafs.Statx{}, -1, err
	}

	if err := checkSupportedFileType(uint32(stat.Mode)); err != nil {
		log.Warningf("Mount: checkSupportedFileType() failed for file %q with mode %#o: %v", mountPath, stat.Mode, err)
		return nil, lisafs.Statx{}, -1, err
	}

	clientHostFD := -1
	if i.config.DonateMountPointFD {
		clientHostFD, err = unix.Dup(rootHostFD)
		if err != nil {
			return nil, lisafs.Statx{}, -1, err
		}
	}
	cu.Release()

	rootFD := &controlFDLisa{
		hostFD:         rootHostFD,
		openedFlags:    rootFlags,
		writableHostFD: atomicbitops.FromInt32(-1),
		isMountPoint:   true,
		virtualPath:    ".",
	}
	mountNode.IncRef() // Ref is transferred to ControlFD.
	rootFD.ControlFD.Init(c, mountNode, linux.FileMode(stat.Mode), rootFD)
	return rootFD.FD(), stat, clientHostFD, nil
}

// MaxMessageSize implements lisafs.ConnectionImpl.MaxMessageSize.
func (i *connectionImpl) MaxMessageSize() uint32 {
	return lisafs.MaxMessageSize()
}

// SupportedMessages implements lisafs.ConnectionImpl.SupportedMessages.
func (i *connectionImpl) SupportedMessages() []lisafs.MID {
	// Note that Flush is not supported.
	return []lisafs.MID{
		lisafs.Mount,
		lisafs.Channel,
		lisafs.FStat,
		lisafs.SetStat,
		lisafs.Walk,
		lisafs.WalkStat,
		lisafs.OpenAt,
		lisafs.OpenCreateAt,
		lisafs.Close,
		lisafs.FSync,
		lisafs.PWrite,
		lisafs.PRead,
		lisafs.MkdirAt,
		lisafs.MknodAt,
		lisafs.SymlinkAt,
		lisafs.LinkAt,
		lisafs.FStatFS,
		lisafs.FAllocate,
		lisafs.ReadLinkAt,
		lisafs.Connect,
		lisafs.UnlinkAt,
		lisafs.RenameAt,
		lisafs.Getdents64,
		lisafs.FGetXattr,
		lisafs.FSetXattr,
		lisafs.FListXattr,
		lisafs.FRemoveXattr,
		lisafs.BindAt,
		lisafs.Listen,
		lisafs.Accept,
		lisafs.ConnectWithCreds,
		lisafs.RenameAt2,
	}
}

// controlFDLisa implements lisafs.ControlFDImpl.
type controlFDLisa struct {
	lisafs.ControlFD

	// hostFD is the file descriptor which can be used to make host syscalls.
	hostFD int

	// writableHostFD is the file descriptor number for a writable FD opened on
	// the same FD as `hostFD`. It is initialized to -1, and can change in value
	// exactly once.
	writableHostFD atomicbitops.Int32

	// openedFlags is what hostFD was opened with. It is immutable, as hostFD
	// is: both are set once here and cleared together when the FD is
	// destroyed.
	openedFlags int

	// isMountpoint indicates whether this FD represents the mount point for its
	// owning connection. isMountPoint is immutable.
	isMountPoint bool

	// virtualPath is the clean path relative to this connection's root.
	virtualPath string
}

var _ lisafs.ControlFDImpl = (*controlFDLisa)(nil)

// newControlFDLisa creates a child FD while its server excludes renames.
//
// +checklocksread:parent.ControlFD.conn.server.renameMu
// +checklocksexclude:parent.ControlFD.conn.fdsMu
func newControlFDLisa(hostFD int, openedFlags int, parent *controlFDLisa, name string, mode linux.FileMode) *controlFDLisa {
	var (
		childFD    *controlFDLisa
		childNode  *lisafs.Node
		parentNode = parent.Node()
	)
	parentNode.WithChildrenMu(func() {
		childNode = parentNode.LookupChildLocked(name)
		if childNode == nil {
			// Common case. Performance hack which is used to allocate the node and
			// its control FD together in the heap. For a well-behaving client, there
			// will be a 1:1 mapping between control FD and node and their lifecycle
			// will be similar too. This will help reduce allocations and memory
			// fragmentation. This is more cache friendly too.
			temp := struct {
				node lisafs.Node
				fd   controlFDLisa
			}{}
			childFD = &temp.fd
			childNode = &temp.node
			childNode.InitLocked(name, parentNode)
		} else {
			childNode.IncRef()
			childFD = &controlFDLisa{}
		}
	})
	childFD.hostFD = hostFD
	childFD.openedFlags = openedFlags
	childFD.virtualPath = path.Join(parent.virtualPath, name)
	childFD.writableHostFD = atomicbitops.FromInt32(-1)
	// Conn returns parent.ControlFD.conn, whose server rename lock is required
	// on entry. checklocks does not retain that identity across the accessor.
	childFD.ControlFD.Init(parent.Conn(), childNode, mode, childFD) // +checklocksignore
	return childFD
}

func (fd *controlFDLisa) getWritableFD() (int, error) {
	if writableFD := fd.writableHostFD.Load(); writableFD != -1 {
		return int(writableFD), nil
	}

	writableFD, err := reopenFD(fd.hostFD, (unix.O_WRONLY|openFlags)&^unix.O_NOFOLLOW)
	if err != nil {
		return -1, err
	}
	if !fd.writableHostFD.CompareAndSwap(-1, int32(writableFD)) {
		// Race detected, use the new value and clean this up.
		unix.Close(writableFD)
		return int(fd.writableHostFD.Load()), nil
	}
	return writableFD, nil
}

// getParentFD opens the parent directory while the server excludes renames.
//
// +checklocksread:fd.ControlFD.conn.server.renameMu
func (fd *controlFDLisa) getParentFD() (int, string, error) {
	filePath := fd.Node().FilePath()
	if filePath == "/" {
		log.Warningf("getParentFD() call on the root")
		return -1, "", unix.EINVAL
	}
	parent, err := openPath(path.Dir(filePath))
	return parent, path.Base(filePath), err
}

// FD implements lisafs.ControlFDImpl.FD.
func (fd *controlFDLisa) FD() *lisafs.ControlFD {
	if fd == nil {
		return nil
	}
	return &fd.ControlFD
}

// Close implements lisafs.ControlFDImpl.Close.
func (fd *controlFDLisa) Close() {
	if fd.hostFD >= 0 {
		_ = unix.Close(fd.hostFD)
		fd.hostFD = -1
	}
	// No concurrent access is possible so no need to use atomics.
	if fd.writableHostFD.RacyLoad() >= 0 {
		_ = unix.Close(int(fd.writableHostFD.RacyLoad()))
		fd.writableHostFD = atomicbitops.FromInt32(-1)
	}
}

func (fd *controlFDLisa) stat(hostFD int, virtualPath string) (lisafs.Statx, error) {
	return fd.Conn().Impl().(*connectionImpl).stat(hostFD, virtualPath)
}

// Stat implements lisafs.ControlFDImpl.Stat.
func (fd *controlFDLisa) Stat() (lisafs.Statx, error) {
	return fd.stat(fd.hostFD, fd.virtualPath)
}

// SetStat implements lisafs.ControlFDImpl.SetStat.
//
// +checklocksread:fd.ControlFD.conn.server.renameMu
func (fd *controlFDLisa) SetStat(stat lisafs.SetStatReq) (failureMask uint32, failureErr error) {
	if stat.Mask&linux.STATX_MODE != 0 {
		switch fd.FileType() {
		case unix.S_IFLNK:
			// Linux does not support changing the mode of symlinks. See
			// fs/attr.c:notify_change().
			failureMask |= linux.STATX_MODE
			failureErr = unix.EOPNOTSUPP
		case unix.S_IFSOCK:
			// Sockets use O_PATH host FDs. However, fchmod(2) fails with EBADF for
			// O_PATH FDs. Try to fchmodat(2) it from its parent.
			parent, sockName, err := fd.getParentFD()
			if err == nil {
				err = unix.Fchmodat(parent, sockName, stat.Mode&^unix.S_IFMT, 0 /* flags */)
				unix.Close(parent)
			}
			if err != nil {
				log.Warningf("SetStat fchmod failed on socket %q, err: %v", fd.Node().FilePath(), err)
				failureMask |= linux.STATX_MODE
				failureErr = err
			}
		default:
			if err := unix.Fchmod(fd.hostFD, stat.Mode&^unix.S_IFMT); err != nil {
				log.Warningf("SetStat fchmod failed %q, err: %v", fd.Node().FilePath(), err)
				failureMask |= linux.STATX_MODE
				failureErr = err
			}
		}
	}

	if stat.Mask&linux.STATX_SIZE != 0 {
		// ftruncate(2) requires the FD to be open for writing.
		writableFD, err := fd.getWritableFD()
		if err == nil {
			err = unix.Ftruncate(writableFD, int64(stat.Size))
		}
		if err != nil {
			log.Warningf("SetStat ftruncate failed %q, err: %v", fd.Node().FilePath(), err)
			failureMask |= linux.STATX_SIZE
			failureErr = err
		}
	}

	if stat.Mask&(linux.STATX_ATIME|linux.STATX_MTIME) != 0 {
		utimes := [2]unix.Timespec{
			omitTimespec(),
			omitTimespec(),
		}
		if stat.Mask&linux.STATX_ATIME != 0 {
			utimes[0].Sec = stat.Atime.Sec
			utimes[0].Nsec = stat.Atime.Nsec
		}
		if stat.Mask&linux.STATX_MTIME != 0 {
			utimes[1].Sec = stat.Mtime.Sec
			utimes[1].Nsec = stat.Mtime.Nsec
		}

		if fd.IsSymlink() {
			// utimensat operates different that other syscalls. To operate on a
			// symlink it *requires* AT_SYMLINK_NOFOLLOW with dirFD and a non-empty
			// name. We need the parent FD.
			parent, symlinkName, err := fd.getParentFD()
			if err == nil {
				err = fsutil.Utimensat(parent, symlinkName, utimes, unix.AT_SYMLINK_NOFOLLOW)
				unix.Close(parent)
			}
			if err != nil {
				failureMask |= (stat.Mask & (linux.STATX_ATIME | linux.STATX_MTIME))
				failureErr = err
			}
		} else {
			hostFD := fd.hostFD
			if fd.IsRegular() {
				// For regular files, utimensat(2) requires the FD to be open for
				// writing, see BUGS section.
				if writableFD, err := fd.getWritableFD(); err == nil {
					hostFD = writableFD
				} else {
					log.Warningf("SetStat getWritableFD failed %q, err: %v", fd.Node().FilePath(), err)
				}
			}
			// Directories and regular files can operate directly on the fd
			// using empty name.
			err := fsutil.Utimensat(hostFD, "", utimes, 0)
			if err != nil {
				log.Warningf("SetStat utimens failed %q, err: %v", fd.Node().FilePath(), err)
				failureMask |= (stat.Mask & (linux.STATX_ATIME | linux.STATX_MTIME))
				failureErr = err
			}
		}
	}

	if stat.Mask&(linux.STATX_UID|linux.STATX_GID) != 0 {
		// "If the owner or group is specified as -1, then that ID is not changed"
		// - chown(2)
		uid := lisafs.NoUID
		if stat.Mask&linux.STATX_UID != 0 {
			uid = stat.UID
		}
		gid := lisafs.NoGID
		if stat.Mask&linux.STATX_GID != 0 {
			gid = stat.GID
		}
		if err := fd.Conn().Impl().(*connectionImpl).fchown(fd.hostFD, uid, gid); err != nil {
			log.Warningf("SetStat fchown failed %q, err: %v", fd.Node().FilePath(), err)
			failureMask |= stat.Mask & (linux.STATX_UID | linux.STATX_GID)
			failureErr = err
		}
	}

	return
}

// Walk implements lisafs.ControlFDImpl.Walk.
//
// +checklocksread:fd.ControlFD.conn.server.renameMu
// +checklocksexclude:fd.ControlFD.conn.fdsMu
func (fd *controlFDLisa) Walk(name string) (*lisafs.ControlFD, lisafs.Statx, error) {
	childHostFD, childFlags, err := tryOpen(func(flags int) (int, error) {
		return unix.Openat(fd.hostFD, name, flags, 0)
	})
	if err != nil {
		return nil, lisafs.Statx{}, err
	}

	stat, err := fd.stat(childHostFD, path.Join(fd.virtualPath, name))
	if err != nil {
		_ = unix.Close(childHostFD)
		return nil, lisafs.Statx{}, err
	}

	if err := checkSupportedFileType(uint32(stat.Mode)); err != nil {
		_ = unix.Close(childHostFD)
		log.Warningf("Walk: checkSupportedFileType() failed for %q with mode %#o: %v", name, stat.Mode, err)
		return nil, lisafs.Statx{}, err
	}

	return newControlFDLisa(childHostFD, childFlags, fd, name, linux.FileMode(stat.Mode)).FD(), stat, nil
}

// WalkStat implements lisafs.ControlFDImpl.WalkStat.
func (fd *controlFDLisa) WalkStat(names lisafs.StringArray, recordStat func(lisafs.Statx)) error {
	// Note that while performing the walk below, we do not have read concurrency
	// guarantee for any descendants. So files can be created/deleted inside fd
	// while the walk is being performed. However, this should be fine from a
	// security perspective as we are using host FDs to walk and checking that
	// each opened path component is not a symlink.
	curDirFD := fd.hostFD
	curVirtualPath := fd.virtualPath
	closeCurDirFD := func() {
		if curDirFD != fd.hostFD {
			unix.Close(curDirFD)
		}
	}
	defer closeCurDirFD()
	if len(names) > 0 && len(names[0]) == 0 {
		// Write stat results for dirFD if the first path component is "".
		stat, err := fd.stat(fd.hostFD, fd.virtualPath)
		if err != nil {
			return err
		}
		recordStat(stat)
		names = names[1:]
	}

	// Don't attempt walking if parent is a symlink.
	if fd.IsSymlink() {
		return nil
	}
	for _, name := range names {
		curFD, err := openPathAt(curDirFD, name)
		if err == unix.ENOENT {
			// No more path components exist on the filesystem. Return the partial
			// walk to the client.
			break
		}
		if err != nil {
			return err
		}
		closeCurDirFD()
		curDirFD = curFD
		curVirtualPath = path.Join(curVirtualPath, name)

		stat, err := fd.stat(curFD, curVirtualPath)
		if err != nil {
			return err
		}
		if err := checkSupportedFileType(uint32(stat.Mode)); err != nil {
			log.Warningf("WalkStat: checkSupportedFileType() failed for file %q with mode %#o while walking path %+v: %v", name, stat.Mode, names, err)
			return err
		}
		recordStat(stat)

		// Symlinks terminate walk. This client gets the symlink stat result, but
		// will have to invoke Walk again with the resolved path.
		if stat.Mode&unix.S_IFMT == unix.S_IFLNK {
			break
		}
	}

	return nil
}

// Used to log rejected fifo/uds operations, one time each.
var (
	logRejectedFifoOpenOnce     sync.Once
	logRejectedUdsOpenOnce      sync.Once
	logRejectedUdsCreateOnce    sync.Once
	logRejectedUdsConnectOnce   sync.Once
	logRejectedCharDevOpenOnce  sync.Once
	logRejectedBlockDevOpenOnce sync.Once
)

// Open implements lisafs.ControlFDImpl.Open.
//
// +checklocksexclude:fd.ControlFD.conn.fdsMu
// +checklocksexclude:fd.ControlFD.openFDsMu
func (fd *controlFDLisa) Open(flags uint32) (*lisafs.OpenFD, int, error) {
	ftype := fd.FileType()
	impl := fd.Conn().Impl().(*connectionImpl)
	if ftype == unix.S_IFREG && impl.config.LazyFiles != nil {
		if err := impl.config.LazyFiles.Ensure(fd.virtualPath); err != nil {
			log.Warningf("materializing lazy image file %q before open: %v", fd.virtualPath, err)
			return nil, -1, fmt.Errorf("materializing lazy image file %q: %w", fd.virtualPath, err)
		}
	}
	switch ftype {
	case unix.S_IFIFO:
		if !impl.config.HostFifo.AllowOpen() {
			logRejectedFifoOpenOnce.Do(func() {
				log.Warningf("Rejecting attempt to open fifo/pipe from host filesystem: %q. If you want to allow this, set flag --host-fifo=open", fd.ControlFD.Node().FilePath())
			})
			return nil, -1, unix.EPERM
		}
	case unix.S_IFSOCK:
		if !impl.config.HostUDS.AllowOpen() {
			logRejectedUdsOpenOnce.Do(func() {
				log.Warningf("Rejecting attempt to open unix domain socket from host filesystem: %q. If you want to allow this, set flag --host-uds=open", fd.ControlFD.Node().FilePath())
			})
			return nil, -1, unix.EPERM
		}
	case unix.S_IFCHR:
		if !impl.config.CharDevicePolicy.AllowsPassthrough() {
			logRejectedCharDevOpenOnce.Do(func() {
				log.Warningf("Rejecting attempt to open character device from host filesystem: %q. If you want to allow this, set flag --character-device-policy=prefer-emulated or --character-device-policy=passthrough", fd.ControlFD.Node().FilePath())
			})
			return nil, -1, unix.EPERM
		}
	case unix.S_IFBLK:
		// This should be unreachable: checkSupportedFileType() rejects block
		// devices at Mount/Walk/WalkStat time, and Mknod cannot create them,
		// so no control FD should refer to one. Check again here for defense
		// in depth. Unlike character devices, there is deliberately no policy
		// that allows opening host block devices.
		logRejectedBlockDevOpenOnce.Do(func() {
			log.Warningf("Rejecting attempt to open block device from host filesystem: %q.", fd.ControlFD.Node().FilePath())
		})
		return nil, -1, unix.EPERM
	}
	// A read-only handle on a regular file does not need a descriptor of its
	// own. The control FD is already an O_RDONLY descriptor on the same file;
	// reads and writes here are positional, so nothing depends on the file
	// description's offset; and an OpenFD holds a reference on its control FD
	// for its whole lifetime, so the descriptor cannot be closed underneath it.
	//
	// This is most of what a gofer holds. A dentry the Sentry is caching keeps
	// its control FD, and the read handle cached alongside it kept a second
	// descriptor on the same file until the dentry was destroyed — 3503 of one
	// failed sandbox's 8146, against a limit of 8191 it could not raise.
	if fd.lendableFor(flags, ftype) {
		openFD := fd.newBorrowedOpenFDLisa(fd.hostFD, flags)
		hostFDToDonate, _ := unix.Dup(fd.hostFD)
		return openFD.FD(), hostFDToDonate, nil
	}

	flags |= openFlags
	openHostFD, err := reopenFD(fd.hostFD, int(flags)&^unix.O_NOFOLLOW)
	if err != nil {
		return nil, -1, err
	}

	hostFDToDonate := -1
	switch {
	case ftype == unix.S_IFREG:
		// Best effort to donate file to the Sentry (for performance only).
		hostFDToDonate, _ = unix.Dup(openHostFD)

	case ftype == unix.S_IFIFO,
		ftype == unix.S_IFCHR,
		fd.isMountPoint && impl.config.DonateMountPointFD:

		// Pipes and character devices (which only get here when
		// --character-device-policy allows passthrough) can block indefinitely
		// during reads/writes, which is not allowed for gofer operations.
		// Ensure that it donates an FD back to the caller, so it can wait on
		// the FD when reads/writes return EWOULDBLOCK. For mount points, if
		// DonateMountPointFD option is set, an FD must be donated.
		var err error
		hostFDToDonate, err = unix.Dup(openHostFD)
		if err != nil {
			return nil, 0, err
		}
	}

	openFD := fd.newOpenFDLisa(openHostFD, flags)
	return openFD.FD(), hostFDToDonate, nil
}

// OpenCreate implements lisafs.ControlFDImpl.OpenCreate.
//
// +checklocksread:fd.ControlFD.conn.server.renameMu
// +checklocksexclude:fd.ControlFD.conn.fdsMu
func (fd *controlFDLisa) OpenCreate(mode linux.FileMode, uid lisafs.UID, gid lisafs.GID, name string, flags uint32) (*lisafs.ControlFD, lisafs.Statx, *lisafs.OpenFD, int, error) {
	createFlags := unix.O_CREAT | unix.O_EXCL | unix.O_RDONLY | unix.O_NONBLOCK | openFlags
	childHostFD, err := unix.Openat(fd.hostFD, name, createFlags, uint32(mode&^linux.FileTypeMask))
	if err != nil {
		return nil, lisafs.Statx{}, nil, -1, err
	}

	cu := cleanup.Make(func() {
		// Best effort attempt to remove the file in case of failure.
		if err := unix.Unlinkat(fd.hostFD, name, 0); err != nil {
			log.Warningf("error unlinking file %q after failure: %v", path.Join(fd.Node().FilePath(), name), err)
		}
		unix.Close(childHostFD)
	})
	defer cu.Clean()

	// Set the owners as requested by the client.
	if err := fd.Conn().Impl().(*connectionImpl).fchown(childHostFD, uid, gid); err != nil {
		return nil, lisafs.Statx{}, nil, -1, err
	}

	// Get stat results.
	childStat, err := fd.stat(childHostFD, path.Join(fd.virtualPath, name))
	if err != nil {
		return nil, lisafs.Statx{}, nil, -1, err
	}

	// Now open an FD to the newly created file with the flags requested by the client.
	flags |= openFlags
	newHostFD, err := reopenFD(childHostFD, int(flags)&^unix.O_NOFOLLOW)
	if err != nil {
		return nil, lisafs.Statx{}, nil, -1, err
	}
	cu.Release()

	childFD := newControlFDLisa(childHostFD, createFlags, fd, name, linux.ModeRegular)
	newFD := childFD.newOpenFDLisa(newHostFD, uint32(flags))

	// Donate FD because open(O_CREAT|O_EXCL) always creates a regular file.
	// Since FD donation is a destructive operation, we should duplicate the
	// to-be-donated FD. Eat the error if one occurs, it is better to have an FD
	// without a host FD, than failing the Open attempt.
	hostOpenFD := -1
	if dupFD, err := unix.Dup(newFD.hostFD); err == nil {
		hostOpenFD = dupFD
	}

	return childFD.FD(), childStat, newFD.FD(), hostOpenFD, nil
}

// Mkdir implements lisafs.ControlFDImpl.Mkdir.
//
// +checklocksread:fd.ControlFD.conn.server.renameMu
// +checklocksexclude:fd.ControlFD.conn.fdsMu
func (fd *controlFDLisa) Mkdir(mode linux.FileMode, uid lisafs.UID, gid lisafs.GID, name string) (*lisafs.ControlFD, lisafs.Statx, error) {
	if err := unix.Mkdirat(fd.hostFD, name, uint32(mode&^linux.FileTypeMask)); err != nil {
		return nil, lisafs.Statx{}, err
	}
	cu := cleanup.Make(func() {
		// Best effort attempt to remove the dir in case of failure.
		if err := unix.Unlinkat(fd.hostFD, name, unix.AT_REMOVEDIR); err != nil {
			log.Warningf("error unlinking dir %q after failure: %v", path.Join(fd.Node().FilePath(), name), err)
		}
	})
	defer cu.Clean()

	// Open directory to change ownership.
	childDirFd, childDirFlags, err := tryOpen(func(flags int) (int, error) {
		return unix.Openat(fd.hostFD, name, flags|unix.O_DIRECTORY, 0)
	})
	if err != nil {
		return nil, lisafs.Statx{}, err
	}
	if err := fd.Conn().Impl().(*connectionImpl).fchown(childDirFd, uid, gid); err != nil {
		unix.Close(childDirFd)
		return nil, lisafs.Statx{}, err
	}

	// Get stat results.
	childDirStat, err := fd.stat(childDirFd, path.Join(fd.virtualPath, name))
	if err != nil {
		unix.Close(childDirFd)
		return nil, lisafs.Statx{}, err
	}

	cu.Release()
	return newControlFDLisa(childDirFd, childDirFlags, fd, name, linux.ModeDirectory).FD(), childDirStat, nil
}

// Mknod implements lisafs.ControlFDImpl.Mknod.
//
// +checklocksread:fd.ControlFD.conn.server.renameMu
// +checklocksexclude:fd.ControlFD.conn.fdsMu
func (fd *controlFDLisa) Mknod(mode linux.FileMode, uid lisafs.UID, gid lisafs.GID, name string, minor uint32, major uint32) (*lisafs.ControlFD, lisafs.Statx, error) {
	// Only allow creating regular files or overlayfs whiteouts. Linux's
	// vfs_mknod() exempts whiteouts from the CAP_MKNOD check, so we do not need
	// CAP_MKNOD on the gofer process. This enables using gofer as an overlay
	// upper layer. Return EPERM otherwise; from mknod(2) man page:
	// "EPERM: [...] if the filesystem containing pathname does not support
	// the type of node requested."
	switch mode.FileType() {
	case linux.S_IFREG:
	case linux.S_IFCHR:
		if minor == 0 && major == 0 {
			break
		}
		fallthrough
	default:
		return nil, lisafs.Statx{}, unix.EPERM
	}

	if err := mknodAt(fd.hostFD, name, mode, minor, major); err != nil {
		return nil, lisafs.Statx{}, err
	}
	cu := cleanup.Make(func() {
		// Best effort attempt to remove the file in case of failure.
		if err := unix.Unlinkat(fd.hostFD, name, 0); err != nil {
			log.Warningf("error unlinking file %q after failure: %v", path.Join(fd.Node().FilePath(), name), err)
		}
	})
	defer cu.Clean()

	// Open file to change ownership.
	childFD, childFlags, err := tryOpen(func(flags int) (int, error) {
		return unix.Openat(fd.hostFD, name, flags, 0)
	})
	if err != nil {
		return nil, lisafs.Statx{}, err
	}
	if err := fd.Conn().Impl().(*connectionImpl).fchown(childFD, uid, gid); err != nil {
		unix.Close(childFD)
		return nil, lisafs.Statx{}, err
	}

	// Get stat results.
	childStat, err := fd.stat(childFD, path.Join(fd.virtualPath, name))
	if err != nil {
		unix.Close(childFD)
		return nil, lisafs.Statx{}, err
	}
	cu.Release()

	return newControlFDLisa(childFD, childFlags, fd, name, mode).FD(), childStat, nil
}

// Symlink implements lisafs.ControlFDImpl.Symlink.
//
// +checklocksread:fd.ControlFD.conn.server.renameMu
// +checklocksexclude:fd.ControlFD.conn.fdsMu
func (fd *controlFDLisa) Symlink(name string, target string, uid lisafs.UID, gid lisafs.GID) (*lisafs.ControlFD, lisafs.Statx, error) {
	if err := unix.Symlinkat(target, fd.hostFD, name); err != nil {
		return nil, lisafs.Statx{}, err
	}
	cu := cleanup.Make(func() {
		// Best effort attempt to remove the symlink in case of failure.
		if err := unix.Unlinkat(fd.hostFD, name, 0); err != nil {
			log.Warningf("error unlinking file %q after failure: %v", path.Join(fd.Node().FilePath(), name), err)
		}
	})
	defer cu.Clean()

	// Open symlink to change ownership.
	symlinkFD, err := openPathAt(fd.hostFD, name)
	if err != nil {
		return nil, lisafs.Statx{}, err
	}
	if err := fd.Conn().Impl().(*connectionImpl).fchown(symlinkFD, uid, gid); err != nil {
		unix.Close(symlinkFD)
		return nil, lisafs.Statx{}, err
	}

	symlinkStat, err := fd.stat(symlinkFD, path.Join(fd.virtualPath, name))
	if err != nil {
		unix.Close(symlinkFD)
		return nil, lisafs.Statx{}, err
	}
	cu.Release()
	return newControlFDLisa(symlinkFD, pathOpenFlags()[0]|openFlags, fd, name, linux.ModeSymlink).FD(), symlinkStat, nil
}

// Link implements lisafs.ControlFDImpl.Link.
//
// +checklocksread:fd.ControlFD.conn.server.renameMu
// +checklocksexclude:fd.ControlFD.conn.fdsMu
func (fd *controlFDLisa) Link(dir lisafs.ControlFDImpl, name string) (*lisafs.ControlFD, lisafs.Statx, error) {
	// Using linkat(targetFD, "", newdirfd, name, AT_EMPTY_PATH) requires
	// CAP_DAC_READ_SEARCH in the *root* userns. The gofer process has
	// CAP_DAC_READ_SEARCH in its own userns. But sometimes the gofer may be
	// running in a different userns. So we can't use AT_EMPTY_PATH. Fallback
	// to using olddirfd to call linkat(2).
	oldDirFD, oldName, err := fd.getParentFD()
	if err != nil {
		return nil, lisafs.Statx{}, err
	}
	dirFD := dir.(*controlFDLisa)
	if err := unix.Linkat(oldDirFD, oldName, dirFD.hostFD, name, 0); err != nil {
		return nil, lisafs.Statx{}, err
	}
	cu := cleanup.Make(func() {
		// Best effort attempt to remove the hard link in case of failure.
		if err := unix.Unlinkat(dirFD.hostFD, name, 0); err != nil {
			log.Warningf("error unlinking file %q after failure: %v", path.Join(dirFD.Node().FilePath(), name), err)
		}
	})
	defer cu.Clean()

	linkFD, linkFlags, err := tryOpen(func(flags int) (int, error) {
		return unix.Openat(dirFD.hostFD, name, flags, 0)
	})
	if err != nil {
		return nil, lisafs.Statx{}, err
	}

	linkStat, err := dirFD.stat(linkFD, path.Join(dirFD.virtualPath, name))
	if err != nil {
		return nil, lisafs.Statx{}, err
	}
	cu.Release()
	// LinkAtHandler obtains source and destination from the same connection.
	// Its rename lock is held; checklocks loses the destination owner through
	// the ControlFDImpl interface and this type assertion.
	return newControlFDLisa(linkFD, linkFlags, dirFD, name, linux.FileMode(linkStat.Mode)).FD(), linkStat, nil // +checklocksignore
}

// StatFS implements lisafs.ControlFDImpl.StatFS.
func (fd *controlFDLisa) StatFS() (lisafs.StatFS, error) {
	return statFS(fd.hostFD)
}

// Readlink implements lisafs.ControlFDImpl.Readlink.
func (fd *controlFDLisa) Readlink(getLinkBuf func(uint32) []byte) (uint16, error) {
	// This is similar to what os.Readlink does.
	for linkLen := 128; linkLen < math.MaxUint16; linkLen *= 2 {
		b := getLinkBuf(uint32(linkLen))
		n, err := readlinkFD(fd.hostFD, fd.Node().FilePath(), b)
		if err != nil {
			return 0, err
		}
		if n < int(linkLen) {
			return uint16(n), nil
		}
	}
	return 0, unix.ENOMEM
}

func isSockTypeSupported(sockType uint32) bool {
	switch sockType {
	case unix.SOCK_STREAM, unix.SOCK_DGRAM, unix.SOCK_SEQPACKET:
		return true
	default:
		log.Debugf("socket type %d is not supported", sockType)
		return false
	}
}

// Connect implements lisafs.ControlFDImpl.Connect.
func (fd *controlFDLisa) Connect(sockType uint32) (int, error) {
	if !fd.Conn().Impl().(*connectionImpl).config.HostUDS.AllowOpen() {
		logRejectedUdsConnectOnce.Do(func() {
			log.Warningf("Rejecting attempt to connect to unix domain socket from host filesystem: %q. If you want to allow this, set flag --host-uds=open", fd.ControlFD.Node().FilePath())
		})
		return -1, unix.EPERM
	}

	// TODO(gvisor.dev/issue/1003): Due to different app vs replacement
	// mappings, the app path may have fit in the sockaddr, but we can't fit
	// hostPath in our sockaddr. We'd need to redirect through a shorter path
	// in order to actually connect to this socket.
	hostPath := fd.Node().FilePath()
	if len(hostPath) >= linux.UnixPathMax {
		return -1, unix.EINVAL
	}

	if !isSockTypeSupported(sockType) {
		return -1, unix.ENXIO
	}

	sock, err := unix.Socket(unix.AF_UNIX, int(sockType), 0)
	if err != nil {
		return -1, err
	}

	sa := unix.SockaddrUnix{Name: hostPath}
	if err := unix.Connect(sock, &sa); err != nil {
		unix.Close(sock)
		return -1, err
	}
	return sock, nil
}

// ConnectWithCreds implements lisafs.ControlFDImpl.ConnectWithCreds.
func (fd *controlFDLisa) ConnectWithCreds(sockType uint32, uid lisafs.UID, gid lisafs.GID) (int, error) {
	impl := fd.Conn().Impl().(*connectionImpl)
	uid, gid = mapGuestOwnerToHost(impl.config, uid, gid)
	if !impl.config.HostUDS.AllowOpen() {
		logRejectedUdsConnectOnce.Do(func() {
			log.Warningf("Rejecting attempt to connect to unix domain socket from host filesystem: %q. If you want to allow this, set flag --host-uds=open", fd.ControlFD.Node().FilePath())
		})
		return -1, unix.EPERM
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// As per capabilities(7), "If the effective user ID is changed from 0 to
	// nonzero, then all capabilities are cleared from the effective set. If the
	// effective user ID is changed from nonzero to 0, then the permitted set is
	// copied to the effective set." Below, we temporarily change the effective
	// UID and GID. So the effective capability set is cleared and restored; the
	// permitted set stays the same. We change GID first, and then UID. Because
	// once the UID is changed, capabilities needed to change GID are dropped.
	uidChanged, gidChanged := false, false
	if int(gid) != impl.config.EGID {
		_, _, err := unix.Syscall(unix.SYS_SETREGID, uintptr(impl.config.RGID), uintptr(gid), 0)
		if err != 0 {
			log.Warningf("Failed to set egid; err: %v", err)
		} else {
			log.Debugf("Successfully set egid to %d", gid)
			gidChanged = true
		}
	}

	if int(uid) != impl.config.EUID {
		_, _, err := unix.Syscall(unix.SYS_SETREUID, uintptr(impl.config.RUID), uintptr(uid), 0)
		if err != 0 {
			log.Warningf("Failed to set euid; err: %v", err)
		} else {
			log.Debugf("Successfully set euid to %d", uid)
			uidChanged = true
		}
	}

	defer func() {
		if uidChanged {
			_, _, err := unix.Syscall(unix.SYS_SETREUID, uintptr(impl.config.RUID), uintptr(impl.config.EUID), 0)
			if err != 0 {
				panic(fmt.Sprintf("Failed to restore euid; err: %v", err))
			}
			log.Debugf("Successfully restored euid to %d", impl.config.EUID)
		}

		if gidChanged {
			_, _, err := unix.Syscall(unix.SYS_SETREGID, uintptr(impl.config.RGID), uintptr(impl.config.EGID), 0)
			if err != 0 {
				panic(fmt.Sprintf("Failed to restore egid; err: %v", err))
			}
			log.Debugf("Successfully restored egid to %d", impl.config.EGID)
		}
	}()

	return fd.Connect(sockType)
}

// BindAt implements lisafs.ControlFDImpl.BindAt.
//
// +checklocksread:fd.ControlFD.conn.server.renameMu
// +checklocksexclude:fd.ControlFD.conn.fdsMu
func (fd *controlFDLisa) BindAt(name string, sockType uint32, mode linux.FileMode, uid lisafs.UID, gid lisafs.GID) (*lisafs.ControlFD, lisafs.Statx, *lisafs.BoundSocketFD, int, error) {
	if !fd.Conn().Impl().(*connectionImpl).config.HostUDS.AllowCreate() {
		logRejectedUdsCreateOnce.Do(func() {
			log.Warningf("Rejecting attempt to create unix domain socket from host filesystem: %q. If you want to allow this, set flag --host-uds=create", name)
		})
		return nil, lisafs.Statx{}, nil, -1, unix.EPERM
	}

	// Because there is no "bindat" syscall in Linux, we must create an
	// absolute path to the socket we are creating,
	socketPath := filepath.Join(fd.Node().FilePath(), name)

	// TODO(gvisor.dev/issue/1003): Due to different app vs replacement
	// mappings, the app path may have fit in the sockaddr, but we can't fit
	// hostPath in our sockaddr. We'd need to redirect through a shorter path
	// in order to actually connect to this socket.
	if len(socketPath) >= linux.UnixPathMax {
		log.Warningf("BindAt called with name too long: %q (len=%d)", socketPath, len(socketPath))
		return nil, lisafs.Statx{}, nil, -1, unix.EINVAL
	}

	// Only the following types are supported.
	if !isSockTypeSupported(sockType) {
		return nil, lisafs.Statx{}, nil, -1, unix.ENXIO
	}

	// Create and bind the socket using the sockPath which may be a
	// symlink.
	sockFD, err := unix.Socket(unix.AF_UNIX, int(sockType), 0)
	if err != nil {
		return nil, lisafs.Statx{}, nil, -1, err
	}
	cu := cleanup.Make(func() {
		_ = unix.Close(sockFD)
	})
	defer cu.Clean()

	// fchmod(2) has to happen *before* the bind(2). sockFD's file mode will
	// be used in creating the filesystem-object in bind(2).
	if err := unix.Fchmod(sockFD, uint32(mode&^linux.FileTypeMask)); err != nil {
		return nil, lisafs.Statx{}, nil, -1, err
	}

	if err := unix.Bind(sockFD, &unix.SockaddrUnix{Name: socketPath}); err != nil {
		return nil, lisafs.Statx{}, nil, -1, err
	}
	cu.Add(func() {
		_ = unix.Unlink(socketPath)
	})

	sockFileFD, sockFileFlags, err := tryOpen(func(flags int) (int, error) {
		return unix.Openat(fd.hostFD, name, flags, 0)
	})
	if err != nil {
		return nil, lisafs.Statx{}, nil, -1, err
	}
	cu.Add(func() {
		_ = unix.Close(sockFileFD)
	})

	if err := fd.Conn().Impl().(*connectionImpl).fchown(sockFileFD, uid, gid); err != nil {
		return nil, lisafs.Statx{}, nil, -1, err
	}

	// Stat the socket.
	sockStat, err := fd.stat(sockFileFD, path.Join(fd.virtualPath, name))
	if err != nil {
		return nil, lisafs.Statx{}, nil, -1, err
	}

	// Create an FD that will be donated to the sandbox.
	sockFDToDonate, err := unix.Dup(sockFD)
	if err != nil {
		return nil, lisafs.Statx{}, nil, -1, err
	}
	cu.Release()

	socketControlFD := newControlFDLisa(sockFD, sockFileFlags, fd, name, linux.ModeSocket)
	boundSocketFD := &boundSocketFDLisa{
		sock: os.NewFile(uintptr(sockFD), socketPath),
	}
	boundSocketFD.Init(socketControlFD.FD(), boundSocketFD)

	return socketControlFD.FD(), sockStat, boundSocketFD.FD(), sockFDToDonate, nil
}

// Unlink implements lisafs.ControlFDImpl.Unlink.
func (fd *controlFDLisa) Unlink(name string, flags uint32) error {
	return unix.Unlinkat(fd.hostFD, name, int(flags))
}

// RenameAt implements lisafs.ControlFDImpl.RenameAt.
func (fd *controlFDLisa) RenameAt(oldName string, newDir lisafs.ControlFDImpl, newName string) error {
	return fsutil.RenameAt(fd.hostFD, oldName, newDir.(*controlFDLisa).hostFD, newName)
}

// RenameAt2 implements lisafs.ControlFDImpl.RenameAt2.
func (fd *controlFDLisa) RenameAt2(oldName string, newDir lisafs.ControlFDImpl, newName string, flags uint32) error {
	return fsutil.RenameAt2(fd.hostFD, oldName, newDir.(*controlFDLisa).hostFD, newName, flags)
}

// Renamed implements lisafs.ControlFDImpl.Renamed.
func (fd *controlFDLisa) Renamed() {
	// controlFDLisa does not have any state to update on rename.
}

// GetXattr implements lisafs.ControlFDImpl.GetXattr.
func (fd *controlFDLisa) GetXattr(name string, size uint32, getValueBuf func(uint32) []byte) (uint16, error) {
	impl := fd.Conn().Impl().(*connectionImpl)
	if value, ok := impl.virtualXattr(fd.virtualPath, name); ok {
		if len(value) > linux.XATTR_SIZE_MAX || len(value) > math.MaxUint16 {
			return 0, unix.E2BIG
		}
		if size != 0 && uint32(len(value)) > size {
			return 0, unix.ERANGE
		}
		data := getValueBuf(uint32(len(value)))
		copy(data, value)
		return uint16(len(value)), nil
	}
	// getxattr(2) called with size 0 should return the attribute size. As a
	// result, we need to return the entire attribute here so that the sentry
	// can return the correct value.
	if size > linux.XATTR_SIZE_MAX || size == 0 {
		size = linux.XATTR_SIZE_MAX
	}
	data := getValueBuf(size)
	if fd.IsSocket() || fd.IsSymlink() {
		// Sockets and symlinks use O_PATH host FDs. However, fgetxattr(2) fails
		// with EBADF for O_PATH FDs. Use lgetxattr(2) instead.
		xattrSize, err := unix.Lgetxattr(fd.Node().FilePath(), name, data)
		return uint16(xattrSize), err
	}
	xattrSize, err := unix.Fgetxattr(fd.hostFD, name, data)
	return uint16(xattrSize), err
}

// SetXattr implements lisafs.ControlFDImpl.SetXattr.
func (fd *controlFDLisa) SetXattr(name string, value string, flags uint32) error {
	if fd.IsSocket() || fd.IsSymlink() {
		// Sockets and symlinks use O_PATH host FDs. However, fsetxattr(2) fails
		// with EBADF for O_PATH FDs. Use lsetxattr(2) instead.
		return unix.Lsetxattr(fd.Node().FilePath(), name, []byte(value), int(flags))
	}
	return unix.Fsetxattr(fd.hostFD, name, []byte(value), int(flags))
}

func (fd *controlFDLisa) listXattr(data []byte) (int, error) {
	if fd.IsSocket() || fd.IsSymlink() {
		// Sockets and symlinks use O_PATH host FDs. However, flistxattr(2) fails
		// with EBADF for O_PATH FDs. Use llistxattr(2) instead.
		return unix.Llistxattr(fd.Node().FilePath(), data)
	}
	return unix.Flistxattr(fd.hostFD, data)
}

var listXattrBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, linux.XATTR_LIST_MAX)
		return &b
	},
}

// ListXattr implements lisafs.ControlFDImpl.ListXattr.
func (fd *controlFDLisa) ListXattr(size uint64) (lisafs.StringArray, error) {
	impl := fd.Conn().Impl().(*connectionImpl)
	virtualNames := impl.virtualXattrNames(fd.virtualPath)
	// listxattr(2) called with size 0 should return the list size. As a result,
	// we need to return the entire list here so that the sentry can return the
	// correct value.
	hostSize := size
	if hostSize > linux.XATTR_LIST_MAX || hostSize == 0 || len(virtualNames) != 0 {
		hostSize = linux.XATTR_LIST_MAX
	}
	bPtr := listXattrBufPool.Get().(*[]byte)
	defer listXattrBufPool.Put(bPtr)
	data := (*bPtr)[:hostSize]
	sz, err := fd.listXattr(data)
	if err != nil {
		return nil, err
	}
	data = data[:sz]
	var names []string
	for len(data) > 0 {
		i := bytes.IndexByte(data, 0)
		if i < 0 {
			break
		}
		names = append(names, string(data[:i]))
		data = data[i+1:]
	}
	if len(virtualNames) != 0 {
		seen := make(map[string]struct{}, len(names)+len(virtualNames))
		for _, name := range names {
			seen[name] = struct{}{}
		}
		for _, name := range virtualNames {
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			names = append(names, name)
		}
		sort.Strings(names)
		var required uint64
		for _, name := range names {
			required += uint64(len(name) + 1)
		}
		if required > linux.XATTR_LIST_MAX {
			return nil, unix.E2BIG
		}
		if size != 0 && required > size {
			return nil, unix.ERANGE
		}
	}
	return names, nil
}

// RemoveXattr implements lisafs.ControlFDImpl.RemoveXattr.
func (fd *controlFDLisa) RemoveXattr(name string) error {
	if fd.IsSocket() || fd.IsSymlink() {
		// Sockets and symlinks use O_PATH host FDs. However, fremovexattr(2) fails
		// with EBADF for O_PATH FDs. Use lremovexattr(2) instead.
		return unix.Lremovexattr(fd.Node().FilePath(), name)
	}
	return unix.Fremovexattr(fd.hostFD, name)
}

// openFDLisa implements lisafs.OpenFDImpl.
type openFDLisa struct {
	lisafs.OpenFD

	// hostFD is the host file descriptor which can be used to make syscalls.
	hostFD int

	// borrowed says hostFD belongs to the control FD this was opened on, so
	// closing this must not close it. The reference an OpenFD holds on its
	// control FD is what keeps it alive for that long.
	borrowed bool
}

var _ lisafs.OpenFDImpl = (*openFDLisa)(nil)

// +checklocksexclude:fd.ControlFD.conn.fdsMu
// +checklocksexclude:fd.ControlFD.openFDsMu
func (fd *controlFDLisa) newOpenFDLisa(hostFD int, flags uint32) *openFDLisa {
	newFD := &openFDLisa{
		hostFD: hostFD,
	}
	newFD.OpenFD.Init(fd.FD(), flags, newFD)
	return newFD
}

// newBorrowedOpenFDLisa opens on a descriptor this does not own.
func (fd *controlFDLisa) newBorrowedOpenFDLisa(hostFD int, flags uint32) *openFDLisa {
	newFD := &openFDLisa{
		hostFD:   hostFD,
		borrowed: true,
	}
	newFD.OpenFD.Init(fd.FD(), flags, newFD)
	return newFD
}

// lendableFor reports whether this control FD's descriptor can serve an Open
// with these flags instead of a new one being made.
//
// Deliberately narrow. Regular files only: a directory's OpenFD seeks its
// descriptor in Getdent64, so two users of one description would read each
// other's position, and everything else the Sentry can open — symlinks,
// sockets — has an O_PATH control FD that cannot be read from at all. Only a
// bare O_RDONLY, which is exactly what the Sentry sends for a read handle;
// anything else it may ask for (O_TRUNC alongside, a writable mode) goes the
// long way round and behaves as it always did, including in what it fails
// with.
func (fd *controlFDLisa) lendableFor(flags uint32, ftype linux.FileMode) bool {
	if ftype != unix.S_IFREG || flags != unix.O_RDONLY {
		return false
	}
	if fd.openedFlags&pathOpenFlagMask != 0 {
		return false
	}
	return fd.openedFlags&unix.O_ACCMODE == unix.O_RDONLY
}

// FD implements lisafs.OpenFDImpl.FD.
func (fd *openFDLisa) FD() *lisafs.OpenFD {
	if fd == nil {
		return nil
	}
	return &fd.OpenFD
}

// Close implements lisafs.OpenFDImpl.Close.
func (fd *openFDLisa) Close() {
	if fd.borrowed {
		// The control FD owns it and outlives this.
		fd.hostFD = -1
		return
	}
	if fd.hostFD >= 0 {
		_ = unix.Close(fd.hostFD)
		fd.hostFD = -1
	}
}

// Stat implements lisafs.OpenFDImpl.Stat.
func (fd *openFDLisa) Stat() (lisafs.Statx, error) {
	controlFD := fd.ControlFD().(*controlFDLisa)
	return controlFD.stat(fd.hostFD, controlFD.virtualPath)
}

// Sync implements lisafs.OpenFDImpl.Sync.
func (fd *openFDLisa) Sync() error {
	return unix.Fsync(fd.hostFD)
}

// Write implements lisafs.OpenFDImpl.Write.
func (fd *openFDLisa) Write(buf []byte, off uint64) (uint64, error) {
	rw := rwfd.NewReadWriter(fd.hostFD)
	n, err := rw.WriteAt(buf, int64(off))
	return uint64(n), err
}

// Read implements lisafs.OpenFDImpl.Read.
func (fd *openFDLisa) Read(buf []byte, off uint64) (uint64, error) {
	rw := rwfd.NewReadWriter(fd.hostFD)
	n, err := rw.ReadAt(buf, int64(off))
	if err != nil && err != io.EOF {
		return 0, err
	}
	return uint64(n), nil
}

// Allocate implements lisafs.OpenFDImpl.Allocate.
func (fd *openFDLisa) Allocate(mode, off, length uint64) error {
	return allocate(fd.hostFD, mode, off, length)
}

// Flush implements lisafs.OpenFDImpl.Flush.
func (fd *openFDLisa) Flush() error {
	return nil
}

// Getdent64 implements lisafs.OpenFDImpl.Getdent64.
func (fd *openFDLisa) Getdent64(count uint32, seek0 bool, recordDirent func(lisafs.Dirent64)) error {
	if seek0 {
		if _, err := unix.Seek(fd.hostFD, 0, 0); err != nil {
			return err
		}
	}

	var direntsBuf [8192]byte
	var bytesRead int
	for bytesRead < int(count) {
		bufEnd := len(direntsBuf)
		if remaining := int(count) - bytesRead; remaining < bufEnd {
			bufEnd = remaining
		}
		n, err := fsutil.ReadDirents(fd.hostFD, direntsBuf[:bufEnd])
		if err != nil {
			if err == unix.EINVAL && bufEnd < fsutil.UnixDirentMaxSize {
				// getdents64(2) returns EINVAL is returned when the result
				// buffer is too small. If bufEnd is smaller than the max
				// size of unix.Dirent, then just break here to return all
				// dirents collected till now.
				break
			}
			return err
		}
		if n <= 0 {
			break
		}

		fsutil.ParseDirents(direntsBuf[:n], func(ino uint64, off int64, ftype uint8, name string, reclen uint16) {
			dirent := lisafs.Dirent64{
				Ino:  primitive.Uint64(ino),
				Off:  primitive.Uint64(off),
				Type: primitive.Uint8(ftype),
				Name: lisafs.SizedString(name),
			}

			// The client also wants the device ID, which annoyingly incurs an
			// additional syscall per dirent.
			// TODO(gvisor.dev/issue/6665): Get rid of per-dirent stat.
			stat, err := fsutil.StatAt(fd.hostFD, name)
			if err != nil {
				log.Warningf("Getdent64: skipping file %q with failed stat, err: %v", path.Join(fd.ControlFD().FD().Node().FilePath(), name), err)
				return
			}
			dirent.DevMinor = primitive.Uint32(statDeviceMinor(stat))
			dirent.DevMajor = primitive.Uint32(statDeviceMajor(stat))
			recordDirent(dirent)
			bytesRead += int(reclen)
		})
	}
	return nil
}

// Renamed implements lisafs.OpenFDImpl.Renamed.
func (fd *openFDLisa) Renamed() {
	// openFDLisa does not have any state to update on rename.
}

type boundSocketFDLisa struct {
	lisafs.BoundSocketFD

	sock *os.File
}

var _ lisafs.BoundSocketFDImpl = (*boundSocketFDLisa)(nil)

// Close implements lisafs.BoundSocketFD.Close.
func (fd *boundSocketFDLisa) Close() {
	fd.sock.Close()
}

// FD implements lisafs.BoundSocketFD.FD.
func (fd *boundSocketFDLisa) FD() *lisafs.BoundSocketFD {
	if fd == nil {
		return nil
	}
	return &fd.BoundSocketFD
}

// Listen implements lisafs.BoundSocketFD.Listen.
func (fd *boundSocketFDLisa) Listen(backlog int32) error {
	return unix.Listen(int(fd.sock.Fd()), int(backlog))
}

// Listen implements lisafs.BoundSocketFD.Accept.
func (fd *boundSocketFDLisa) Accept() (int, string, error) {
	flags := unix.O_NONBLOCK | unix.O_CLOEXEC
	nfd, err := acceptNonblockCloexec(int(fd.sock.Fd()), flags)
	if err != nil {
		return -1, "", err
	}
	// Return an empty peer address so that we don't leak the actual host
	// address.
	return nfd, "", err
}

// tryOpen tries to open() with different modes as documented.
// tryOpen returns the descriptor and the flags it was opened with.
//
// Which of the two attempts succeeded is worth carrying: an O_PATH descriptor
// cannot be read from, and a readable one can be lent to an OpenFD instead of
// opening the file a second time.
func tryOpen(open func(int) (int, error)) (hostFD int, openedFlags int, err error) {
	// Attempt to open file in the following in order:
	//   1. RDONLY | NONBLOCK: for all files, directories, ro mounts, FIFOs.
	//      Use non-blocking to prevent getting stuck inside open(2) for
	//      FIFOs. This option has no effect on regular files.
	//   2. PATH: for symlinks, sockets.
	flags := append([]int{unix.O_RDONLY | unix.O_NONBLOCK}, pathOpenFlags()...)

	for _, flag := range flags {
		hostFD, err = open(combineOpenFlags(flag))
		if err == nil {
			return hostFD, flag | openFlags, nil
		}

		if e := lisafs.ExtractErrno(err); e == unix.ENOENT {
			// File doesn't exist, no point in retrying.
			return -1, 0, e
		}
	}
	return hostFD, 0, err
}

func checkSupportedFileType(mode uint32) error {
	switch mode & unix.S_IFMT {
	case unix.S_IFREG, unix.S_IFDIR, unix.S_IFLNK, unix.S_IFCHR, unix.S_IFSOCK, unix.S_IFIFO:
		return nil

	default:
		return unix.EPERM
	}
}

// LINT.ThenChange(../../pkg/sentry/fsimpl/gofer/directfs_inode.go)
