// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package dmaheap implements Linux's DMA-BUF heaps (drivers/dma-buf) with
// the system heap, /dev/dma_heap/system, whose dma-bufs are the sentry's
// memory. Android's media codecs allocate their buffers from it.
package dmaheap

import (
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/marshal/primitive"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/tmpfs"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/usermem"
	"gvisor.dev/gvisor/pkg/waiter"
)

// The UAPI, from include/uapi/linux/dma-heap.h and dma-buf.h.
const (
	ioctlHeapAlloc = 0xc0184800 // DMA_HEAP_IOCTL_ALLOC
	ioctlSync      = 0x40086200 // DMA_BUF_IOCTL_SYNC
	ioctlSetNameA  = 0x40046201 // DMA_BUF_SET_NAME_A
	ioctlSetNameB  = 0x40086201 // DMA_BUF_SET_NAME_B, and DMA_BUF_SET_NAME on 64-bit

	// allocationDataSize is the size of struct dma_heap_allocation_data:
	// len, fd, fd_flags, and heap_flags.
	allocationDataSize = 24

	syncRW         = 3 // DMA_BUF_SYNC_RW
	syncEnd        = 4 // DMA_BUF_SYNC_END
	syncValidFlags = syncRW | syncEnd

	// nameLen is DMA_BUF_NAME_LEN.
	nameLen = 32
)

// Register registers the system heap in vfsObj as /dev/dma_heap/system,
// with a char major of its own, as Linux's heaps have.
func Register(vfsObj *vfs.VirtualFilesystem) error {
	major, err := vfsObj.GetDynamicCharDevMajor()
	if err != nil {
		return err
	}
	return vfsObj.RegisterDevice(vfs.CharDevice, major, 0, systemHeap{}, &vfs.RegisterDeviceOptions{
		GroupName: "dma_heap",
		Pathname:  "dma_heap/system",
		FilePerms: 0444,
	})
}

// systemHeap is /dev/dma_heap/system.
//
// +stateify savable
type systemHeap struct{}

// Open implements vfs.Device.Open.
func (systemHeap) Open(ctx context.Context, mnt *vfs.Mount, d *vfs.Dentry, opts vfs.OpenOptions) (*vfs.FileDescription, error) {
	fd := &heapFD{}
	if err := fd.vfsfd.Init(fd, opts.Flags, auth.CredentialsFromContext(ctx), mnt, d, &vfs.FileDescriptionOptions{
		UseDentryMetadata: true,
		SpecialFile:       true,
	}); err != nil {
		return nil, err
	}
	return &fd.vfsfd, nil
}

// heapFD is an open heap.
//
// +stateify savable
type heapFD struct {
	vfsfd vfs.FileDescription
	vfs.FileDescriptionDefaultImpl
	vfs.DentryMetadataFileDescriptionImpl
	vfs.NoLockFD
}

// Release implements vfs.FileDescriptionImpl.Release.
func (fd *heapFD) Release(context.Context) {}

// Ioctl implements vfs.FileDescriptionImpl.Ioctl. DMA_HEAP_IOCTL_ALLOC
// allocates a dma-buf, as drivers/dma-buf/dma-heap.c:dma_heap_ioctl_allocate()
// does; a heap opened read-only allocates too.
func (fd *heapFD) Ioctl(ctx context.Context, uio usermem.IO, sysno uintptr, args arch.SyscallArguments) (uintptr, error) {
	t := kernel.TaskFromContext(ctx)
	if t == nil {
		panic("Ioctl should be called from a task context")
	}
	if args[1].Uint() != ioctlHeapAlloc {
		return 0, linuxerr.ENOTTY
	}
	addr := args[2].Pointer()
	var data [allocationDataSize]byte
	if _, err := t.CopyInBytes(addr, data[:]); err != nil {
		return 0, err
	}
	length := hostarch.ByteOrder.Uint64(data[0:])
	fdFlags := hostarch.ByteOrder.Uint32(data[12:])
	heapFlags := hostarch.ByteOrder.Uint64(data[16:])
	if hostarch.ByteOrder.Uint32(data[8:]) != 0 ||
		fdFlags&^(linux.O_ACCMODE|linux.O_CLOEXEC) != 0 || heapFlags != 0 {
		return 0, linuxerr.EINVAL
	}
	size, ok := hostarch.PageRoundUp(length)
	if !ok || size == 0 {
		return 0, linuxerr.EINVAL
	}
	buf, err := newBuffer(t, size, fdFlags&linux.O_ACCMODE)
	if err != nil {
		return 0, err
	}
	defer buf.DecRef(t)
	newFD, err := t.NewFDFrom(0, buf, kernel.FDFlags{CloseOnExec: fdFlags&linux.O_CLOEXEC != 0})
	if err != nil {
		return 0, err
	}
	hostarch.ByteOrder.PutUint32(data[8:], uint32(newFD))
	if _, err := t.CopyOutBytes(addr, data[:]); err != nil {
		if file := t.FDTable().Remove(t, newFD); file != nil {
			file.DecRef(t)
		}
		return 0, err
	}
	return 0, nil
}

// newBuffer returns a dma-buf of size bytes of zeroed memory, opened with
// flags.
func newBuffer(t *kernel.Task, size uint64, flags uint32) (*vfs.FileDescription, error) {
	// The memory is a memfd's, so that it is saved, restored, and cloned by
	// live forks with the memfd.
	mem, err := tmpfs.NewMemfd(t, t.Credentials(), t.Kernel().ShmMount(), false /* allowSeals */, "dmabuf")
	if err != nil {
		return nil, err
	}
	if err := mem.SetStat(t, vfs.SetStatOptions{Stat: linux.Statx{Mask: linux.STATX_SIZE, Size: size}}); err != nil {
		mem.DecRef(t)
		return nil, err
	}
	vd := t.Kernel().VFS().NewAnonVirtualDentry("dmabuf")
	defer vd.DecRef(t)
	fd := &bufferFD{mem: mem}
	if err := fd.vfsfd.Init(fd, flags, t.Credentials(), vd.Mount(), vd.Dentry(), &vfs.FileDescriptionOptions{
		DenyPRead:  true,
		DenyPWrite: true,
	}); err != nil {
		mem.DecRef(t)
		return nil, err
	}
	return &fd.vfsfd, nil
}

// bufferFD is a dma-buf of the system heap.
//
// +stateify savable
type bufferFD struct {
	vfsfd vfs.FileDescription
	vfs.FileDescriptionDefaultImpl
	vfs.NoLockFD

	// mem is the memfd that holds the buffer's memory.
	mem *vfs.FileDescription
}

// Release implements vfs.FileDescriptionImpl.Release.
func (fd *bufferFD) Release(ctx context.Context) {
	fd.mem.DecRef(ctx)
}

// ConfigureMMap implements vfs.FileDescriptionImpl.ConfigureMMap.
func (fd *bufferFD) ConfigureMMap(ctx context.Context, opts *memmap.MMapOpts) error {
	return fd.mem.ConfigureMMap(ctx, opts)
}

// Seek implements vfs.FileDescriptionImpl.Seek. Users find a dma-buf's
// size by seeking its end.
func (fd *bufferFD) Seek(ctx context.Context, offset int64, whence int32) (int64, error) {
	return fd.mem.Seek(ctx, offset, whence)
}

// Stat implements vfs.FileDescriptionImpl.Stat.
func (fd *bufferFD) Stat(ctx context.Context, opts vfs.StatOptions) (linux.Statx, error) {
	return fd.mem.Stat(ctx, opts)
}

// SetStat implements vfs.FileDescriptionImpl.SetStat. A dma-buf's
// attributes are its heap's.
func (fd *bufferFD) SetStat(context.Context, vfs.SetStatOptions) error {
	return linuxerr.EPERM
}

// Readiness implements waiter.Waitable.Readiness. The sentry's dma-bufs have
// no fences, so they are always ready.
func (fd *bufferFD) Readiness(mask waiter.EventMask) waiter.EventMask {
	return mask & (waiter.ReadableEvents | waiter.WritableEvents)
}

// EventRegister implements waiter.Waitable.EventRegister.
func (fd *bufferFD) EventRegister(*waiter.Entry) error {
	return nil
}

// EventUnregister implements waiter.Waitable.EventUnregister.
func (fd *bufferFD) EventUnregister(*waiter.Entry) {}

// Epollable implements vfs.FileDescriptionImpl.Epollable.
func (fd *bufferFD) Epollable() bool {
	return true
}

// Ioctl implements vfs.FileDescriptionImpl.Ioctl.
func (fd *bufferFD) Ioctl(ctx context.Context, uio usermem.IO, sysno uintptr, args arch.SyscallArguments) (uintptr, error) {
	t := kernel.TaskFromContext(ctx)
	if t == nil {
		panic("Ioctl should be called from a task context")
	}
	switch args[1].Uint() {
	case ioctlSync:
		// CPU access needs no synchronization: the memory is coherent.
		var flags primitive.Uint64
		if _, err := flags.CopyIn(t, args[2].Pointer()); err != nil {
			return 0, err
		}
		if uint64(flags)&^syncValidFlags != 0 || uint64(flags)&syncRW == 0 {
			return 0, linuxerr.EINVAL
		}
		return 0, nil
	case ioctlSetNameA, ioctlSetNameB:
		// The name is for debugging, in Linux's fdinfo and debugfs, which
		// the sentry does not show.
		if _, err := t.CopyInString(args[2].Pointer(), nameLen); err != nil {
			if linuxerr.Equals(linuxerr.ENAMETOOLONG, err) {
				return 0, linuxerr.EINVAL
			}
			return 0, err
		}
		return 0, nil
	}
	return 0, linuxerr.ENOTTY
}
