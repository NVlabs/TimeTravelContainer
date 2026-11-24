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

package gofer

import (
	goContext "context"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/lisafs"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

// directfs is a Linux host optimization based on O_PATH, statx, and Linux
// *at syscalls. It deliberately remains unavailable on Darwin; the normal
// lisafs/gofer data plane is the portable macOS implementation.
//
// +stateify savable
type directfsInode struct {
	inode
	controlFD     int
	controlFDLisa lisafs.ClientFD `state:"nosave"`
}

// afterLoad is invoked by stateify.
func (i *directfsInode) afterLoad(goContext.Context) {
	i.controlFD = -1
}

func tryOpen(open func(int) (int, error)) (int, error) { return -1, linuxerr.EOPNOTSUPP }

func (fs *filesystem) getDirectfsRootDentry(ctx context.Context, rootHostFD int, rootControlFD lisafs.ClientFD) (*dentry, error) {
	if rootHostFD >= 0 {
		_ = unix.Close(rootHostFD)
	}
	rootControlFD.Close(ctx, false)
	return nil, linuxerr.EOPNOTSUPP
}

func (fs *filesystem) newDirectfsDentry(controlFD int) (*dentry, error) {
	if controlFD >= 0 {
		_ = unix.Close(controlFD)
	}
	return nil, linuxerr.EOPNOTSUPP
}

func (i *directfsInode) openHandle(context.Context, uint32, *dentry) (handle, error) {
	return noHandle, linuxerr.EOPNOTSUPP
}
func (i *directfsInode) updateMetadataLocked(handle) error { return linuxerr.EOPNOTSUPP }
func (i *directfsInode) prepareSetStat(context.Context, *linux.Statx, *dentry) error {
	return linuxerr.EOPNOTSUPP
}
func (i *directfsInode) chmod(context.Context, uint16, *dentry) error { return linuxerr.EOPNOTSUPP }
func (i *directfsInode) setStatLocked(_ context.Context, stat *linux.Statx, _ *dentry) (uint32, error) {
	return stat.Mask, linuxerr.EOPNOTSUPP
}
func (i *directfsInode) destroy(ctx context.Context) {
	if i.controlFD >= 0 {
		_ = unix.Close(i.controlFD)
		i.controlFD = -1
	}
	if i.controlFDLisa.Ok() {
		i.controlFDLisa.Close(ctx, false)
	}
}
func (i *directfsInode) getHostChild(string) (*dentry, error) {
	return nil, linuxerr.EOPNOTSUPP
}
func (i *directfsInode) getXattr(context.Context, string, uint64, *dentry) (string, error) {
	return "", linuxerr.EOPNOTSUPP
}
func (i *directfsInode) setXattr(context.Context, *vfs.SetXattrOptions, *dentry) error {
	return linuxerr.EOPNOTSUPP
}
func (i *directfsInode) listXattr(context.Context, uint64, *dentry) ([]string, error) {
	return nil, linuxerr.EOPNOTSUPP
}
func (i *directfsInode) removeXattr(context.Context, string, *dentry) error {
	return linuxerr.EOPNOTSUPP
}
func (i *directfsInode) mknod(context.Context, string, *auth.Credentials, *vfs.MknodOptions, *dentry) (*dentry, error) {
	return nil, linuxerr.EOPNOTSUPP
}
func (i *directfsInode) link(*dentry, string, *dentry) (*dentry, error) {
	return nil, linuxerr.EOPNOTSUPP
}
func (i *directfsInode) mkdir(string, linux.FileMode, auth.KUID, auth.KGID, bool, *dentry) (*dentry, error) {
	return nil, linuxerr.EOPNOTSUPP
}
func (i *directfsInode) symlink(string, string, *auth.Credentials, *dentry) (*dentry, error) {
	return nil, linuxerr.EOPNOTSUPP
}
func (i *directfsInode) openCreate(string, uint32, linux.FileMode, auth.KUID, auth.KGID, bool, *dentry) (*dentry, handle, error) {
	return nil, noHandle, linuxerr.EOPNOTSUPP
}
func (i *directfsInode) getDirentsLocked(func(string, inoKey, uint8), *dentry) error {
	return linuxerr.EOPNOTSUPP
}
func (i *directfsInode) connect(context.Context, linux.SockType, lisafs.UID, lisafs.GID, []lisafs.GID, *dentry) (int, error) {
	return -1, linuxerr.EOPNOTSUPP
}
func (i *directfsInode) readlink() (string, error) { return "", linuxerr.EOPNOTSUPP }
func (i *directfsInode) statfs() (linux.Statfs, error) {
	return linux.Statfs{}, linuxerr.EOPNOTSUPP
}
func (i *directfsInode) restoreFile(context.Context, int, *vfs.CompleteRestoreOptions, *dentry) error {
	return linuxerr.EOPNOTSUPP
}
func doRevalidationDirectfs(context.Context, *vfs.VirtualFilesystem, *revalidateState, **[]*dentry) error {
	return linuxerr.EOPNOTSUPP
}
