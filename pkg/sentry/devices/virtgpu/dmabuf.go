// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package virtgpu

import (
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/usermem"
	"gvisor.dev/gvisor/pkg/waiter"
)

// dmabufFD is a dma-buf of a resource, which processes pass between them,
// map, and import into files of the render node.
//
// +stateify savable
type dmabufFD struct {
	vfsfd vfs.FileDescription
	vfs.FileDescriptionDefaultImpl
	vfs.DentryMetadataFileDescriptionImpl
	vfs.NoLockFD

	// res is the resource, on which the dma-buf holds a reference.
	res *resource

	// name is the name that DMA_BUF_SET_NAME gave the buffer. It is
	// protected by device.mu.
	name string
}

var _ vfs.LiveForkFileDescription = (*dmabufFD)(nil)

// export returns r's dma-buf, with a reference for the caller. Like Linux,
// the device creates one dma-buf for a resource, and exports it again while
// it exists: see drm_gem_prime_handle_to_dmabuf(). Its access mode is that
// of the export that created it.
func (d *device) export(t *kernel.Task, r *resource, rdwr bool) (*vfs.FileDescription, error) {
	d.mu.Lock()
	if db := r.dmabuf; db != nil && db.vfsfd.TryIncRef() {
		d.mu.Unlock()
		return &db.vfsfd, nil
	}
	d.mu.Unlock()

	vd := t.Kernel().VFS().NewAnonVirtualDentry("dmabuf")
	defer vd.DecRef(t)
	db := &dmabufFD{res: r}
	flags := uint32(linux.O_RDONLY)
	if rdwr {
		flags = linux.O_RDWR
	}
	r.IncRef()
	if err := db.vfsfd.Init(db, flags, auth.CredentialsFromContext(t), vd.Mount(), vd.Dentry(), &vfs.FileDescriptionOptions{
		UseDentryMetadata: true,
		DenyPRead:         true,
		DenyPWrite:        true,
	}); err != nil {
		r.DecRef(t)
		return nil, err
	}

	d.mu.Lock()
	if other := r.dmabuf; other != nil && other.vfsfd.TryIncRef() {
		// Another export won.
		d.mu.Unlock()
		db.vfsfd.DecRef(t)
		return &other.vfsfd, nil
	}
	r.dmabuf = db
	d.mu.Unlock()
	return &db.vfsfd, nil
}

// Release implements vfs.FileDescriptionImpl.Release.
func (db *dmabufFD) Release(ctx context.Context) {
	d := db.res.dev
	d.mu.Lock()
	if db.res.dmabuf == db {
		db.res.dmabuf = nil
	}
	d.mu.Unlock()
	db.res.DecRef(ctx)
}

// ValidateLiveFork implements vfs.LiveForkFileDescription.ValidateLiveFork.
func (db *dmabufFD) ValidateLiveFork() ([]*vfs.FileDescription, error) {
	return nil, db.res.dev.validateLiveFork()
}

// ConfigureMMap implements vfs.FileDescriptionImpl.ConfigureMMap.
func (db *dmabufFD) ConfigureMMap(ctx context.Context, opts *memmap.MMapOpts) error {
	return db.res.configureMMap(opts, opts.Offset)
}

// Seek implements vfs.FileDescriptionImpl.Seek. Like Linux, it finds only the
// buffer's end, which is how its users learn its size.
func (db *dmabufFD) Seek(ctx context.Context, offset int64, whence int32) (int64, error) {
	if offset != 0 {
		return 0, linuxerr.EINVAL
	}
	switch whence {
	case linux.SEEK_SET:
		return 0, nil
	case linux.SEEK_END:
		return int64(db.res.size), nil
	default:
		return 0, linuxerr.EINVAL
	}
}

// Readiness implements waiter.Waitable.Readiness. The buffer is ready once
// the submissions that use it are done, as Linux's dma_buf_poll() reports for
// implicit synchronization.
func (db *dmabufFD) Readiness(mask waiter.EventMask) waiter.EventMask {
	d := db.res.dev
	d.mu.Lock()
	idle := len(db.res.fences) == 0
	d.mu.Unlock()
	if idle {
		return mask & (waiter.ReadableEvents | waiter.WritableEvents)
	}
	return 0
}

// EventRegister implements waiter.Waitable.EventRegister.
func (db *dmabufFD) EventRegister(e *waiter.Entry) error {
	db.res.queue.EventRegister(e)
	return nil
}

// EventUnregister implements waiter.Waitable.EventUnregister.
func (db *dmabufFD) EventUnregister(e *waiter.Entry) {
	db.res.queue.EventUnregister(e)
}

// Epollable implements vfs.FileDescriptionImpl.Epollable.
func (db *dmabufFD) Epollable() bool {
	return true
}

// Ioctl implements vfs.FileDescriptionImpl.Ioctl.
func (db *dmabufFD) Ioctl(ctx context.Context, uio usermem.IO, sysno uintptr, args arch.SyscallArguments) (uintptr, error) {
	t := kernel.TaskFromContext(ctx)
	if t == nil {
		panic("Ioctl should be called from a task context")
	}
	addr := args[2].Pointer()
	d := db.res.dev
	switch args[1].Uint() {
	case linux.DMA_BUF_IOCTL_SYNC:
		var s linux.DMABufSync
		if _, err := s.CopyIn(t, addr); err != nil {
			return 0, err
		}
		if s.Flags&^(linux.DMA_BUF_SYNC_RW|linux.DMA_BUF_SYNC_END) != 0 || s.Flags&linux.DMA_BUF_SYNC_RW == 0 {
			return 0, linuxerr.EINVAL
		}
		// The renderer's memory is coherent with the guest's mappings.
		return 0, nil

	case linux.DMA_BUF_SET_NAME_A, linux.DMA_BUF_SET_NAME_B:
		name, err := t.CopyInString(addr, linux.DMA_BUF_NAME_LEN)
		if err != nil {
			if linuxerr.Equals(linuxerr.ENAMETOOLONG, err) {
				err = linuxerr.EINVAL
			}
			return 0, err
		}
		d.mu.Lock()
		db.name = name
		d.mu.Unlock()
		return 0, nil

	case linux.DMA_BUF_IOCTL_EXPORT_SYNC_FILE:
		var s linux.DMABufSyncFile
		if _, err := s.CopyIn(t, addr); err != nil {
			return 0, err
		}
		if s.Flags&^linux.DMA_BUF_SYNC_RW != 0 || s.Flags&linux.DMA_BUF_SYNC_RW == 0 {
			return 0, linuxerr.EINVAL
		}
		d.mu.Lock()
		f := db.res.fenceLocked()
		d.mu.Unlock()
		file, err := d.newSyncFile(t, f, "")
		if err != nil {
			return 0, err
		}
		defer file.DecRef(t)
		newFD, err := t.NewFDFrom(0, file, kernel.FDFlags{CloseOnExec: true})
		if err != nil {
			return 0, err
		}
		s.FD = newFD
		_, err = s.CopyOut(t, addr)
		return 0, err

	case linux.DMA_BUF_IOCTL_IMPORT_SYNC_FILE:
		var s linux.DMABufSyncFile
		if _, err := s.CopyIn(t, addr); err != nil {
			return 0, err
		}
		if s.Flags&^linux.DMA_BUF_SYNC_RW != 0 || s.Flags&linux.DMA_BUF_SYNC_RW == 0 {
			return 0, linuxerr.EINVAL
		}
		f, err := syncFileFence(t, s.FD)
		if err != nil {
			return 0, err
		}
		d.mu.Lock()
		db.res.addFenceLocked(f)
		d.mu.Unlock()
		return 0, nil

	default:
		return 0, linuxerr.ENOTTY
	}
}
