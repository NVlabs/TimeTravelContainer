// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package binderfs implements Linux's binderfs, the filesystem of binder
// devices (pkg/sentry/devices/binder): each mount is an instance with
// binder-control, whose BINDER_CTL_ADD adds devices, the devices that Linux
// creates by default, binder, hwbinder, and vndbinder, and features, which
// lists the driver's optional features.
package binderfs

import (
	"bytes"
	"strconv"
	"strings"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/devices/binder"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/kernfs"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/usermem"
)

// Name is the name of the filesystem type.
const Name = "binder"

// binderfsMagic is BINDERFS_SUPER_MAGIC.
const binderfsMagic = 0x6c6f6f70

// maxName is BINDERFS_MAX_NAME.
const maxName = 255

// defaultDevices are the devices of a new instance, as Linux's default
// CONFIG_ANDROID_BINDER_DEVICES has them.
var defaultDevices = []string{"binder", "hwbinder", "vndbinder"}

// ctlAdd is BINDER_CTL_ADD, _IOWR('b', 1, struct binderfs_device).
const ctlAdd = 3<<30 | 264<<16 | 'b'<<8 | 1

// FilesystemType implements vfs.FilesystemType.
//
// +stateify savable
type FilesystemType struct {
	mu sync.Mutex `state:"nosave"`
	// major is the major number of binderfs' devices, allocated once.
	major uint32
	// nextMinor is the next minor number of a device.
	nextMinor uint32
}

// Name implements vfs.FilesystemType.Name.
func (*FilesystemType) Name() string {
	return Name
}

// Release implements vfs.FilesystemType.Release.
func (*FilesystemType) Release(ctx context.Context) {}

// GetFilesystem implements vfs.FilesystemType.GetFilesystem.
func (fstype *FilesystemType) GetFilesystem(ctx context.Context, vfsObj *vfs.VirtualFilesystem, creds *auth.Credentials, source string, opts vfs.GetFilesystemOptions) (*vfs.Filesystem, *vfs.Dentry, error) {
	mopts := vfs.GenericParseMountOptions(opts.Data)
	maxDevices := uint64(1 << 20)
	if s, ok := mopts["max"]; ok {
		delete(mopts, "max")
		n, err := strconv.ParseUint(s, 10, 32)
		if err != nil {
			return nil, nil, linuxerr.EINVAL
		}
		maxDevices = n
	}
	if s, ok := mopts["stats"]; ok {
		delete(mopts, "stats")
		if s != "global" {
			return nil, nil, linuxerr.EINVAL
		}
	}
	if len(mopts) != 0 {
		ctx.Warningf("binderfs.FilesystemType.GetFilesystem: unknown options: %v", mopts)
		return nil, nil, linuxerr.EINVAL
	}

	fstype.mu.Lock()
	if fstype.major == 0 {
		major, err := vfsObj.GetDynamicCharDevMajor()
		if err != nil {
			fstype.mu.Unlock()
			return nil, nil, err
		}
		fstype.major = major
	}
	fstype.mu.Unlock()

	devMinor, err := vfsObj.GetAnonBlockDevMinor()
	if err != nil {
		return nil, nil, err
	}
	fs := &filesystem{
		fstype:     fstype,
		devMinor:   devMinor,
		maxDevices: maxDevices,
		devices:    make(map[string]*binder.Device),
	}
	fs.VFSFilesystem().Init(vfsObj, fstype, fs)

	root := &rootInode{}
	root.StaticDirectory.Init(ctx, creds, linux.UNNAMED_MAJOR, devMinor, fs.NextIno(), 0755, kernfs.GenericDirectoryFDOptions{
		SeekEnd: kernfs.SeekEndStaticEntries,
	})
	root.InitRefs()
	root.OrderedChildren.Init(kernfs.OrderedChildrenOptions{})
	fs.root = root

	features := kernfs.NewStaticDir(ctx, creds, linux.UNNAMED_MAJOR, devMinor, fs.NextIno(), 0755, map[string]kernfs.Inode{
		"oneway_spam_detection": fs.newFeature(ctx, creds, true),
		"extended_error":        fs.newFeature(ctx, creds, true),
		"freeze_notification":   fs.newFeature(ctx, creds, false),
	}, kernfs.GenericDirectoryFDOptions{})
	control := fs.newDevice(ctx, creds, nil)
	links := root.OrderedChildren.Populate(map[string]kernfs.Inode{
		"binder-control": control,
		"features":       features,
	})
	root.IncLinks(links)
	for _, name := range defaultDevices {
		if _, err := fs.addDevice(ctx, creds, name); err != nil {
			return nil, nil, err
		}
	}

	var rootD kernfs.Dentry
	rootD.InitRoot(&fs.Filesystem, root)
	return fs.VFSFilesystem(), rootD.VFSDentry(), nil
}

// filesystem is a binderfs instance.
//
// +stateify savable
type filesystem struct {
	kernfs.Filesystem

	fstype     *FilesystemType
	devMinor   uint32
	maxDevices uint64
	root       *rootInode

	mu sync.Mutex `state:"nosave"`
	// devices are the instance's binder devices, by name.
	devices map[string]*binder.Device
}

// LiveForkFilesystemEligibility implements
// vfs.LiveForkFilesystemEligibility.
func (*filesystem) LiveForkFilesystemEligibility() vfs.LiveForkFilesystemEligibility {
	return vfs.LiveForkFilesystemEligibility{Decision: vfs.LiveForkFilesystemClone, Reason: "binderfs and its binder state are Sentry-owned and cloned per branch"}
}

// Release implements vfs.FilesystemImpl.Release.
func (fs *filesystem) Release(ctx context.Context) {
	fs.Filesystem.VFSFilesystem().VirtualFilesystem().PutAnonBlockDevMinor(fs.devMinor)
	fs.Filesystem.Release(ctx)
}

// MountOptions implements vfs.FilesystemImpl.MountOptions.
func (fs *filesystem) MountOptions() string {
	return ""
}

// addDevice adds a binder device named name, and returns its minor number.
func (fs *filesystem) addDevice(ctx context.Context, creds *auth.Credentials, name string) (uint32, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
		return 0, linuxerr.EACCES
	}
	if len(name) > maxName {
		return 0, linuxerr.ENAMETOOLONG
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if uint64(len(fs.devices)) >= fs.maxDevices {
		return 0, linuxerr.ENOSPC
	}
	if _, ok := fs.devices[name]; ok || name == "binder-control" || name == "features" {
		return 0, linuxerr.EEXIST
	}
	dev := binder.NewDevice(name)
	inode := fs.newDevice(ctx, creds, dev)
	if err := fs.root.OrderedChildren.Insert(name, inode); err != nil {
		return 0, err
	}
	fs.devices[name] = dev
	return inode.minor, nil
}

// newDevice returns the inode of a binder device, or, for a nil dev,
// binder-control.
func (fs *filesystem) newDevice(ctx context.Context, creds *auth.Credentials, dev *binder.Device) *deviceInode {
	fs.fstype.mu.Lock()
	minor := fs.fstype.nextMinor
	fs.fstype.nextMinor++
	fs.fstype.mu.Unlock()
	inode := &deviceInode{fs: fs, dev: dev, minor: minor}
	inode.InodeAttrs.Init(ctx, creds, linux.UNNAMED_MAJOR, fs.devMinor, fs.NextIno(), linux.ModeCharacterDevice|0600)
	return inode
}

// newFeature returns the file of a feature, which reads 1 if enabled.
func (fs *filesystem) newFeature(ctx context.Context, creds *auth.Credentials, enabled bool) kernfs.Inode {
	value := "0\n"
	if enabled {
		value = "1\n"
	}
	f := &featureFile{value: value}
	f.DynamicBytesFile.Init(ctx, creds, linux.UNNAMED_MAJOR, fs.devMinor, fs.NextIno(), f, 0444)
	return f
}

// rootInode is an instance's root directory. Unlike kernfs' static
// directories, it may be chmod'ed, as Android's init does.
//
// +stateify savable
type rootInode struct {
	kernfs.StaticDirectory
}

// SetStat implements kernfs.Inode.SetStat.
func (r *rootInode) SetStat(ctx context.Context, fs *vfs.Filesystem, creds *auth.Credentials, opts vfs.SetStatOptions) error {
	return r.InodeAttrs.SetStat(ctx, fs, creds, opts)
}

// StatFS implements kernfs.Inode.StatFS.
func (r *rootInode) StatFS(context.Context, *vfs.Filesystem) (linux.Statfs, error) {
	return vfs.GenericStatFS(binderfsMagic), nil
}

// deviceInode is a binder device, or binder-control.
//
// +stateify savable
type deviceInode struct {
	kernfs.InodeAlwaysValid
	kernfs.InodeAttrs
	kernfs.InodeNoopRefCount
	kernfs.InodeNotAnonymous
	kernfs.InodeNotDirectory
	kernfs.InodeNotSymlink
	kernfs.InodeWatches
	kernfs.InodeFSOwned

	locks vfs.FileLocks

	fs *filesystem
	// dev is the device, or nil for binder-control.
	dev   *binder.Device
	minor uint32
}

var _ kernfs.Inode = (*deviceInode)(nil)

// Open implements kernfs.Inode.Open.
func (i *deviceInode) Open(ctx context.Context, rp *vfs.ResolvingPath, d *kernfs.Dentry, opts vfs.OpenOptions) (*vfs.FileDescription, error) {
	if i.dev != nil {
		return i.dev.Open(ctx, rp.Mount(), d.VFSDentry(), opts)
	}
	fd := &controlFD{fs: i.fs}
	if err := fd.vfsfd.Init(fd, opts.Flags, rp.Credentials(), rp.Mount(), d.VFSDentry(), &vfs.FileDescriptionOptions{
		UseDentryMetadata: true,
	}); err != nil {
		return nil, err
	}
	return &fd.vfsfd, nil
}

// Stat implements kernfs.Inode.Stat.
func (i *deviceInode) Stat(ctx context.Context, vfsfs *vfs.Filesystem, opts vfs.StatOptions) (linux.Statx, error) {
	statx, err := i.InodeAttrs.Stat(ctx, vfsfs, opts)
	if err != nil {
		return statx, err
	}
	statx.RdevMajor = i.fs.fstype.major
	statx.RdevMinor = i.minor
	return statx, nil
}

// StatFS implements kernfs.Inode.StatFS.
func (i *deviceInode) StatFS(context.Context, *vfs.Filesystem) (linux.Statfs, error) {
	return vfs.GenericStatFS(binderfsMagic), nil
}

// controlFD is a file of binder-control.
//
// +stateify savable
type controlFD struct {
	vfsfd vfs.FileDescription
	vfs.FileDescriptionDefaultImpl
	vfs.DentryMetadataFileDescriptionImpl
	vfs.NoLockFD

	fs *filesystem
}

// Release implements vfs.FileDescriptionImpl.Release.
func (fd *controlFD) Release(context.Context) {}

// Ioctl implements vfs.FileDescriptionImpl.Ioctl.
func (fd *controlFD) Ioctl(ctx context.Context, uio usermem.IO, sysno uintptr, args arch.SyscallArguments) (uintptr, error) {
	t := kernel.TaskFromContext(ctx)
	if t == nil || args[1].Uint() != ctlAdd {
		return 0, linuxerr.EINVAL
	}
	// struct binderfs_device { char name[BINDERFS_MAX_NAME + 1]; __u32 major, minor; }
	var b [maxName + 1 + 8]byte
	addr := args[2].Pointer()
	if _, err := t.CopyInBytes(addr, b[:]); err != nil {
		return 0, err
	}
	name := b[:maxName+1]
	if i := bytes.IndexByte(name, 0); i >= 0 {
		name = name[:i]
	} else {
		name = name[:maxName]
	}
	minor, err := fd.fs.addDevice(ctx, t.Credentials(), string(name))
	if err != nil {
		return 0, err
	}
	le32(b[maxName+1:], fd.fs.fstype.major)
	le32(b[maxName+5:], minor)
	_, err = t.CopyOutBytes(addr, b[:])
	return 0, err
}

func le32(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
}

// featureFile is a file of features.
//
// +stateify savable
type featureFile struct {
	kernfs.DynamicBytesFile
	value string
}

// Generate implements vfs.DynamicBytesSource.Generate.
func (f *featureFile) Generate(ctx context.Context, buf *bytes.Buffer) error {
	buf.WriteString(f.value)
	return nil
}
