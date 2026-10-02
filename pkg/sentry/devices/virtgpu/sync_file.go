// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package virtgpu

import (
	"bytes"
	"fmt"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/usermem"
	"gvisor.dev/gvisor/pkg/waiter"
)

// syncFile is a sync_file: a file for a fence, which processes pass between
// them, poll, and merge.
//
// +stateify savable
type syncFile struct {
	vfsfd vfs.FileDescription
	vfs.FileDescriptionDefaultImpl
	vfs.DentryMetadataFileDescriptionImpl
	vfs.NoLockFD

	dev   *device
	fence *fence
	// name is the name that a merge gave the file, or empty.
	name string
}

var _ vfs.LiveForkFileDescription = (*syncFile)(nil)

// newSyncFile returns a sync_file for f.
func (d *device) newSyncFile(ctx context.Context, f *fence, name string) (*vfs.FileDescription, error) {
	vd := kernel.KernelFromContext(ctx).VFS().NewAnonVirtualDentry("sync_file")
	defer vd.DecRef(ctx)
	sf := &syncFile{
		dev:   d,
		fence: f,
		name:  name,
	}
	if err := sf.vfsfd.Init(sf, linux.O_RDONLY, auth.CredentialsFromContext(ctx), vd.Mount(), vd.Dentry(), &vfs.FileDescriptionOptions{
		UseDentryMetadata: true,
		DenyPRead:         true,
		DenyPWrite:        true,
	}); err != nil {
		return nil, err
	}
	return &sf.vfsfd, nil
}

// syncFileFence returns the fence of the sync_file fd.
func syncFileFence(t *kernel.Task, fd int32) (*fence, error) {
	file := t.GetFile(fd)
	if file == nil {
		return nil, linuxerr.EINVAL
	}
	defer file.DecRef(t)
	sf, ok := file.Impl().(*syncFile)
	if !ok {
		return nil, linuxerr.EINVAL
	}
	return sf.fence, nil
}

// Release implements vfs.FileDescriptionImpl.Release.
func (sf *syncFile) Release(context.Context) {}

// ValidateLiveFork implements vfs.LiveForkFileDescription.ValidateLiveFork.
func (sf *syncFile) ValidateLiveFork() ([]*vfs.FileDescription, error) {
	sf.dev.mu.Lock()
	signaled := sf.fence.signaled
	sf.dev.mu.Unlock()
	if signaled {
		return nil, nil
	}
	return nil, sf.dev.validateLiveFork()
}

// Readiness implements waiter.Waitable.Readiness.
func (sf *syncFile) Readiness(mask waiter.EventMask) waiter.EventMask {
	sf.dev.mu.Lock()
	signaled := sf.fence.signaled
	sf.dev.mu.Unlock()
	if signaled {
		return mask & waiter.ReadableEvents
	}
	return 0
}

// EventRegister implements waiter.Waitable.EventRegister.
func (sf *syncFile) EventRegister(e *waiter.Entry) error {
	sf.fence.queue.EventRegister(e)
	return nil
}

// EventUnregister implements waiter.Waitable.EventUnregister.
func (sf *syncFile) EventUnregister(e *waiter.Entry) {
	sf.fence.queue.EventUnregister(e)
}

// Epollable implements vfs.FileDescriptionImpl.Epollable.
func (sf *syncFile) Epollable() bool {
	return true
}

// Ioctl implements vfs.FileDescriptionImpl.Ioctl.
func (sf *syncFile) Ioctl(ctx context.Context, uio usermem.IO, sysno uintptr, args arch.SyscallArguments) (uintptr, error) {
	t := kernel.TaskFromContext(ctx)
	if t == nil {
		panic("Ioctl should be called from a task context")
	}
	addr := args[2].Pointer()
	switch args[1].Uint() {
	case linux.SYNC_IOC_MERGE:
		return 0, sf.merge(t, addr)
	case linux.SYNC_IOC_FILE_INFO:
		return 0, sf.fileInfo(t, addr)
	case linux.SYNC_IOC_SET_DEADLINE:
		var sd linux.SyncSetDeadline
		if _, err := sd.CopyIn(t, addr); err != nil {
			return 0, err
		}
		if sd.Pad != 0 {
			return 0, linuxerr.EINVAL
		}
		// The renderer schedules its work itself.
		return 0, nil
	default:
		return 0, linuxerr.ENOTTY
	}
}

// merge implements SYNC_IOC_MERGE.
func (sf *syncFile) merge(t *kernel.Task, addr hostarch.Addr) error {
	var data linux.SyncMergeData
	if _, err := data.CopyIn(t, addr); err != nil {
		return err
	}
	if data.Flags != 0 || data.Pad != 0 {
		return linuxerr.EINVAL
	}
	file := t.GetFile(data.FD2)
	if file == nil {
		return linuxerr.ENOENT
	}
	defer file.DecRef(t)
	other, ok := file.Impl().(*syncFile)
	if !ok {
		return linuxerr.ENOENT
	}
	d := sf.dev
	d.mu.Lock()
	merged := mergeFencesLocked(sf.fence, other.fence)
	d.mu.Unlock()
	name := data.Name[:len(data.Name)-1]
	if i := bytes.IndexByte(name, 0); i >= 0 {
		name = name[:i]
	}
	mergedFile, err := d.newSyncFile(t, merged, string(name))
	if err != nil {
		return err
	}
	defer mergedFile.DecRef(t)
	newFD, err := t.NewFDFrom(0, mergedFile, kernel.FDFlags{CloseOnExec: true})
	if err != nil {
		return err
	}
	data.Fence = newFD
	if _, err := data.CopyOut(t, addr); err != nil {
		return err
	}
	return nil
}

// fileInfo implements SYNC_IOC_FILE_INFO.
func (sf *syncFile) fileInfo(t *kernel.Task, addr hostarch.Addr) error {
	var info linux.SyncFileInfo
	if _, err := info.CopyIn(t, addr); err != nil {
		return err
	}
	if info.Flags != 0 || info.Pad != 0 {
		return linuxerr.EINVAL
	}
	d := sf.dev
	d.mu.Lock()
	leaves := sf.fence.leaves()
	if info.NumFences == 0 {
		// The caller asks only for the number of fences.
		info.Status = sf.fence.statusLocked()
	} else if int(info.NumFences) < len(leaves) {
		d.mu.Unlock()
		return linuxerr.EINVAL
	}
	var fences []linux.SyncFenceInfo
	if info.NumFences != 0 {
		info.Status = 1
		fences = make([]linux.SyncFenceInfo, len(leaves))
		for i, f := range leaves {
			fi := &fences[i]
			copy(fi.ObjName[:len(fi.ObjName)-1], f.timeline.String())
			copy(fi.DriverName[:len(fi.DriverName)-1], "virtio_gpu")
			fi.Status = f.statusLocked()
			if f.signaled {
				fi.TimestampNS = uint64(f.timestamp)
			}
			if info.Status > 0 {
				info.Status = fi.Status
			}
		}
	}
	name := sf.name
	if name == "" {
		name = fmt.Sprintf("virtio_gpu-%s-%d", sf.fence.timeline, sf.fence.id)
	}
	d.mu.Unlock()
	if len(fences) != 0 {
		if _, err := linux.CopySyncFenceInfoSliceOut(t, hostarch.Addr(info.SyncFenceInfo), fences); err != nil {
			return err
		}
	}
	info.Name = [32]byte{}
	copy(info.Name[:len(info.Name)-1], name)
	info.NumFences = uint32(len(leaves))
	_, err := info.CopyOut(t, addr)
	return err
}
