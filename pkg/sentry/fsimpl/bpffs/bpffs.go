// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package bpffs implements Linux's bpf filesystem, where bpf(2)'s
// BPF_OBJ_PIN pins eBPF programs and maps, as files, and BPF_OBJ_GET gets
// them back. Directories may be made, removed, and renamed; pinned objects
// may be renamed, chmod'ed, chown'ed, and unlinked, which unpins them.
package bpffs

import (
	"strconv"
	"strings"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/fspath"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/kernfs"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

// Name is the name of the filesystem type.
const Name = "bpf"

// bpfFSMagic is BPF_FS_MAGIC.
const bpfFSMagic = 0xcafe4a11

// Object is a pinned eBPF object: an *ebpf.Program or an *ebpf.Map.
type Object interface {
	IncRef()
	DecRef()
}

// pinKey is the key of the context value that carries the object that
// BPF_OBJ_PIN pins through vfs.VirtualFilesystem.MknodAt.
type pinKey struct{}

// FilesystemType implements vfs.FilesystemType.
//
// +stateify savable
type FilesystemType struct{}

// Name implements vfs.FilesystemType.Name.
func (*FilesystemType) Name() string {
	return Name
}

// Release implements vfs.FilesystemType.Release.
func (*FilesystemType) Release(ctx context.Context) {}

// GetFilesystem implements vfs.FilesystemType.GetFilesystem. Each mount is
// a new instance, as Linux's are.
func (fstype *FilesystemType) GetFilesystem(ctx context.Context, vfsObj *vfs.VirtualFilesystem, creds *auth.Credentials, source string, opts vfs.GetFilesystemOptions) (*vfs.Filesystem, *vfs.Dentry, error) {
	// Linux ignores options that it doesn't know.
	mopts := vfs.GenericParseMountOptions(opts.Data)
	mode := linux.FileMode(0777 | linux.S_ISVTX)
	uid := creds.FilesystemKUID
	gid := creds.FilesystemKGID
	if s, ok := mopts["mode"]; ok {
		m, err := strconv.ParseUint(s, 8, 32)
		if err != nil {
			return nil, nil, linuxerr.EINVAL
		}
		mode = linux.FileMode(m) &^ linux.FileTypeMask
	}
	if s, ok := mopts["uid"]; ok {
		n, err := strconv.ParseUint(s, 10, 32)
		if err != nil {
			return nil, nil, linuxerr.EINVAL
		}
		uid = creds.UserNamespace.MapToKUID(auth.UID(n))
		if !uid.Ok() {
			return nil, nil, linuxerr.EINVAL
		}
	}
	if s, ok := mopts["gid"]; ok {
		n, err := strconv.ParseUint(s, 10, 32)
		if err != nil {
			return nil, nil, linuxerr.EINVAL
		}
		gid = creds.UserNamespace.MapToKGID(auth.GID(n))
		if !gid.Ok() {
			return nil, nil, linuxerr.EINVAL
		}
	}

	devMinor, err := vfsObj.GetAnonBlockDevMinor()
	if err != nil {
		return nil, nil, err
	}
	fs := &filesystem{devMinor: devMinor}
	fs.VFSFilesystem().Init(vfsObj, fstype, fs)
	root := fs.newDir(ctx, uid, gid, mode)
	var rootD kernfs.Dentry
	rootD.InitRoot(&fs.Filesystem, root)
	return fs.VFSFilesystem(), rootD.VFSDentry(), nil
}

// filesystem is a bpf filesystem instance.
//
// +stateify savable
type filesystem struct {
	kernfs.Filesystem

	devMinor uint32
}

// LiveForkFilesystemEligibility implements
// vfs.LiveForkFilesystemEligibility.
func (*filesystem) LiveForkFilesystemEligibility() vfs.LiveForkFilesystemEligibility {
	return vfs.LiveForkFilesystemEligibility{Decision: vfs.LiveForkFilesystemClone, Reason: "bpffs and its eBPF objects are Sentry-owned and cloned per branch"}
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

// newDir returns a directory of mode perm, which may have the sticky, setuid,
// and setgid bits, owned by uid and gid.
func (fs *filesystem) newDir(ctx context.Context, uid auth.KUID, gid auth.KGID, perm linux.FileMode) *dirInode {
	d := &dirInode{fs: fs}
	d.InodeAttrs.InitWithIDs(ctx, uid, gid, linux.UNNAMED_MAJOR, fs.devMinor, fs.NextIno(), linux.ModeDirectory|perm&^linux.FileTypeMask)
	d.OrderedChildren.Init(kernfs.OrderedChildrenOptions{Writable: true})
	return d
}

// dirInode is a directory.
//
// +stateify savable
type dirInode struct {
	kernfs.InodeAlwaysValid
	kernfs.InodeAttrs
	kernfs.InodeNoopRefCount
	kernfs.InodeNotAnonymous
	kernfs.InodeNotSymlink
	kernfs.InodeWatches
	kernfs.OrderedChildren
	kernfs.InodeFSOwned

	locks vfs.FileLocks

	fs *filesystem
}

var _ kernfs.Inode = (*dirInode)(nil)

// Keep implements kernfs.Inode.Keep: the tree of dentries is the
// filesystem's.
func (*dirInode) Keep() bool {
	return true
}

// SetStat implements kernfs.Inode.SetStat.
func (d *dirInode) SetStat(ctx context.Context, fs *vfs.Filesystem, creds *auth.Credentials, opts vfs.SetStatOptions) error {
	return d.InodeAttrs.SetStat(ctx, fs, creds, opts)
}

// StatFS implements kernfs.Inode.StatFS.
func (*dirInode) StatFS(context.Context, *vfs.Filesystem) (linux.Statfs, error) {
	return vfs.GenericStatFS(bpfFSMagic), nil
}

// Open implements kernfs.Inode.Open.
func (d *dirInode) Open(ctx context.Context, rp *vfs.ResolvingPath, kd *kernfs.Dentry, opts vfs.OpenOptions) (*vfs.FileDescription, error) {
	fd, err := kernfs.NewGenericDirectoryFD(rp.Mount(), kd, rp.Credentials(), &d.OrderedChildren, &d.locks, &opts, kernfs.GenericDirectoryFDOptions{
		SeekEnd: kernfs.SeekEndStaticEntries,
	})
	if err != nil {
		return nil, err
	}
	return fd.VFSFileDescription(), nil
}

// Lookup implements kernfs.Inode.Lookup. As Linux's bpf_lookup, names with
// dots are reserved.
func (d *dirInode) Lookup(ctx context.Context, name string) (kernfs.Inode, error) {
	if strings.Contains(name, ".") {
		return nil, linuxerr.EPERM
	}
	return d.OrderedChildren.Lookup(ctx, name)
}

// NewFile implements kernfs.Inode.NewFile. Files are only made by pinning.
func (*dirInode) NewFile(context.Context, string, vfs.OpenOptions) (kernfs.Inode, error) {
	return nil, linuxerr.EACCES
}

// NewDir implements kernfs.Inode.NewDir.
func (d *dirInode) NewDir(ctx context.Context, name string, opts vfs.MkdirOptions) (kernfs.Inode, error) {
	creds := auth.CredentialsFromContext(ctx)
	return d.OrderedChildren.Inserter(name, func() kernfs.Inode {
		d.IncLinks(1)
		return d.fs.newDir(ctx, creds.FilesystemKUID, creds.FilesystemKGID, opts.Mode)
	})
}

// NewLink implements kernfs.Inode.NewLink.
func (*dirInode) NewLink(context.Context, string, kernfs.Inode) (kernfs.Inode, error) {
	return nil, linuxerr.EPERM
}

// NewSymlink implements kernfs.Inode.NewSymlink.
func (*dirInode) NewSymlink(context.Context, string, string) (kernfs.Inode, error) {
	return nil, linuxerr.EPERM
}

// NewNode implements kernfs.Inode.NewNode. mknod(2) can't make nodes here;
// only BPF_OBJ_PIN, whose object ctx carries, can.
func (d *dirInode) NewNode(ctx context.Context, name string, opts vfs.MknodOptions) (kernfs.Inode, error) {
	obj, ok := ctx.Value(pinKey{}).(Object)
	if !ok || opts.Mode.FileType() != linux.ModeRegular {
		return nil, linuxerr.EPERM
	}
	creds := auth.CredentialsFromContext(ctx)
	return d.OrderedChildren.Inserter(name, func() kernfs.Inode {
		o := &objectInode{fs: d.fs, obj: obj}
		o.InodeAttrs.Init(ctx, creds, linux.UNNAMED_MAJOR, d.fs.devMinor, d.fs.NextIno(), linux.ModeRegular|opts.Mode.Permissions())
		obj.IncRef()
		o.refs.Store(1)
		return o
	})
}

// RmDir implements kernfs.Inode.RmDir.
func (d *dirInode) RmDir(ctx context.Context, name string, child kernfs.Inode) error {
	if err := d.OrderedChildren.RmDir(ctx, name, child); err != nil {
		return err
	}
	d.DecLinks()
	return nil
}

// Rename implements kernfs.Inode.Rename.
func (d *dirInode) Rename(ctx context.Context, oldname, newname string, child, dstDir kernfs.Inode) error {
	dst, ok := dstDir.(*dirInode)
	if !ok {
		return linuxerr.EXDEV
	}
	if err := d.OrderedChildren.Rename(ctx, oldname, newname, child, dstDir); err != nil {
		return err
	}
	if _, isDir := child.(*dirInode); isDir && dst != d {
		d.DecLinks()
		dst.IncLinks(1)
	}
	return nil
}

// objectInode is a pinned eBPF object, which it holds a reference on until
// it is unlinked and closed.
//
// +stateify savable
type objectInode struct {
	kernfs.InodeAlwaysValid
	kernfs.InodeAttrs
	kernfs.InodeNotAnonymous
	kernfs.InodeNotDirectory
	kernfs.InodeNotSymlink
	kernfs.InodeWatches
	kernfs.InodeFSOwned

	fs   *filesystem
	refs atomicbitops.Int64
	obj  Object
}

var _ kernfs.Inode = (*objectInode)(nil)

// Keep implements kernfs.Inode.Keep.
func (*objectInode) Keep() bool {
	return true
}

// IncRef implements kernfs.Inode.IncRef.
func (o *objectInode) IncRef() {
	o.refs.Add(1)
}

// TryIncRef implements kernfs.Inode.TryIncRef.
func (o *objectInode) TryIncRef() bool {
	for {
		r := o.refs.Load()
		if r <= 0 {
			return false
		}
		if o.refs.CompareAndSwap(r, r+1) {
			return true
		}
	}
}

// DecRef implements kernfs.Inode.DecRef.
func (o *objectInode) DecRef(ctx context.Context) {
	if o.refs.Add(-1) == 0 {
		o.obj.DecRef()
	}
}

// SetStat implements kernfs.Inode.SetStat.
func (o *objectInode) SetStat(ctx context.Context, fs *vfs.Filesystem, creds *auth.Credentials, opts vfs.SetStatOptions) error {
	return o.InodeAttrs.SetStat(ctx, fs, creds, opts)
}

// StatFS implements kernfs.Inode.StatFS.
func (*objectInode) StatFS(context.Context, *vfs.Filesystem) (linux.Statfs, error) {
	return vfs.GenericStatFS(bpfFSMagic), nil
}

// Open implements kernfs.Inode.Open. As Linux's bpffs_obj_open, pinned
// objects can't be opened: BPF_OBJ_GET gets them.
func (*objectInode) Open(context.Context, *vfs.ResolvingPath, *kernfs.Dentry, vfs.OpenOptions) (*vfs.FileDescription, error) {
	return nil, linuxerr.EIO
}

// Pin pins obj as name, in parent, with permissions perm, as BPF_OBJ_PIN
// does. It fails with EPERM unless parent is a bpf filesystem's directory.
func Pin(ctx context.Context, creds *auth.Credentials, parent vfs.VirtualDentry, name string, perm linux.FileMode, obj Object) error {
	if _, ok := parent.Mount().Filesystem().Impl().(*filesystem); !ok {
		return linuxerr.EPERM
	}
	kd, ok := parent.Dentry().Impl().(*kernfs.Dentry)
	if !ok {
		return linuxerr.EPERM
	}
	if _, ok := kd.Inode().(*dirInode); !ok {
		return linuxerr.EPERM
	}
	if name == "" || strings.ContainsRune(name, '/') {
		return linuxerr.ENOENT
	}
	pop := vfs.PathOperation{
		Root:  parent,
		Start: parent,
		Path:  fspath.Parse(name),
	}
	return parent.Mount().Filesystem().VirtualFilesystem().MknodAt(context.WithValue(ctx, pinKey{}, obj), creds, &pop, &vfs.MknodOptions{
		Mode: linux.ModeRegular | perm.Permissions(),
	})
}

// Get returns the object pinned at vd, with a reference, as BPF_OBJ_GET
// does, if creds may access it as ats. It fails with EACCES if vd isn't a
// pinned object.
func Get(ctx context.Context, creds *auth.Credentials, vd vfs.VirtualDentry, ats vfs.AccessTypes) (Object, error) {
	if _, ok := vd.Mount().Filesystem().Impl().(*filesystem); !ok {
		return nil, linuxerr.EACCES
	}
	kd, ok := vd.Dentry().Impl().(*kernfs.Dentry)
	if !ok {
		return nil, linuxerr.EACCES
	}
	o, ok := kd.Inode().(*objectInode)
	if !ok {
		return nil, linuxerr.EACCES
	}
	if err := o.CheckPermissions(ctx, creds, ats); err != nil {
		return nil, err
	}
	o.obj.IncRef()
	return o.obj, nil
}
