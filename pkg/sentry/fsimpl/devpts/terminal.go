// Copyright 2018 The gVisor Authors.
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

package devpts

import (
	"fmt"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/fspath"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

// Terminal is a pseudoterminal.
//
// Terminal implements kernel.TTYOperations.
//
// +stateify savable
type Terminal struct {
	// n is the terminal index. It is immutable.
	n uint32

	// ld is the line discipline of the terminal. It is immutable.
	ld *lineDiscipline

	// root is the rootInode for this devpts mount. It is immutable.
	root *rootInode

	// masterKTTY contains the controlling process of the master end of
	// this terminal. This field is immutable.
	masterKTTY *kernel.TTY

	// replicaKTTY contains the controlling process of the replica end of this
	// terminal. This field is immutable.
	replicaKTTY *kernel.TTY
}

var _ kernel.TTYOperations = (*Terminal)(nil)

// IncRef implements kernel.TTYOperations.IncRef.
//
// A devpts Terminal is kept alive by its master file description and the
// rootInode.replicas map rather than by a reference count.
func (t *Terminal) IncRef() {}

// DecRef implements kernel.TTYOperations.DecRef.
func (t *Terminal) DecRef(context.Context) {}

// OpenTTY implements kernel.TTYOperations.OpenTTY.
func (t *Terminal) OpenTTY(ctx context.Context, mnt *vfs.Mount, vfsd *vfs.Dentry, opts vfs.OpenOptions) (*vfs.FileDescription, error) {
	tsk := kernel.TaskFromContext(ctx)
	creds := auth.CredentialsFromContext(ctx)
	if tsk != nil {
		creds = tsk.Credentials()
	}
	t.root.mu.Lock()
	ri, ok := t.root.replicas[t.replicaKTTY.Index()]
	t.root.mu.Unlock()
	if !ok {
		return nil, linuxerr.EIO
	}
	fd := &replicaFileDescription{
		inode: ri,
	}
	fd.LockFD.Init(&ri.locks)
	if err := fd.vfsfd.Init(fd, opts.Flags, creds, mnt, vfsd, &vfs.FileDescriptionOptions{
		DenyPRead:   true,
		DenyPWrite:  true,
		SpecialFile: true,
	}); err != nil {
		return nil, err
	}
	if tsk != nil && opts.Flags&linux.O_NOCTTY == 0 {
		// Opening a replica sets the process' controlling TTY when
		// possible. An error indicates it cannot be set, and is
		// ignored silently. See Linux tty_open().
		_ = tsk.ThreadGroup().SetControllingTTY(ctx, t.replicaKTTY, false /* steal */, fd.vfsfd.IsReadable())
	}
	ri.t.ld.replicaOpen()
	return &fd.vfsfd, nil
}

// NewInternalPTY allocates a PTY from mntns's devpts instance without
// requiring an existing task. It returns references to the control-side
// master and process-side replica; the caller must release both references.
// tty is the controlling terminal associated with replica.
func NewInternalPTY(ctx context.Context, vfsObj *vfs.VirtualFilesystem, mntns *vfs.MountNamespace) (master, replica *vfs.FileDescription, tty *kernel.TTY, err error) {
	root := mntns.Root(ctx)
	if !root.Ok() {
		return nil, nil, nil, fmt.Errorf("mount namespace has no root")
	}
	defer root.DecRef(ctx)
	creds := auth.CredentialsFromContext(ctx)
	master, err = vfsObj.OpenAt(ctx, creds, &vfs.PathOperation{
		Root:               root,
		Start:              root,
		Path:               fspath.Parse("/dev/ptmx"),
		FollowFinalSymlink: true,
	}, &vfs.OpenOptions{Flags: linux.O_RDWR | linux.O_NONBLOCK | linux.O_NOCTTY})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("opening /dev/ptmx: %w", err)
	}
	mfd, ok := master.Impl().(*masterFileDescription)
	if !ok {
		master.DecRef(ctx)
		return nil, nil, nil, fmt.Errorf("/dev/ptmx returned %T, want devpts master", master.Impl())
	}
	replicaPath := fmt.Sprintf("/dev/pts/%d", mfd.t.n)
	replica, err = vfsObj.OpenAt(ctx, creds, &vfs.PathOperation{
		Root:  root,
		Start: root,
		Path:  fspath.Parse(replicaPath),
	}, &vfs.OpenOptions{Flags: linux.O_RDWR})
	if err != nil {
		master.DecRef(ctx)
		return nil, nil, nil, fmt.Errorf("opening %s: %w", replicaPath, err)
	}
	return master, replica, mfd.t.replicaKTTY, nil
}
