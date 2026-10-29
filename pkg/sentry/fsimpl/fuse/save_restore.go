// Copyright 2024 The gVisor Authors.
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

package fuse

import (
	goContext "context"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

var _ vfs.FilesystemImplSaveRestoreExtension = (*filesystem)(nil)

func (fRes *futureResponse) afterLoad(goContext.Context) {
	fRes.ch = make(chan struct{})
}

func (conn *connection) afterLoad(goContext.Context) {
	if conn.hostTransport {
		// The host FD was not carried across the checkpoint, and the
		// connection was aborted before it was saved.
		conn.fuseConn = &lostHostConn{conn: conn}
		return
	}
	conn.fuseConn = &deviceConn{conn: conn}
}

func (fs *filesystem) afterLoad(ctx goContext.Context) {
	fs.mf = pgalloc.MemoryFileFromContext(ctx)
	fs.cacheMu.Lock()
	defer fs.cacheMu.Unlock()
	if fs.fileCaches == nil {
		fs.fileCaches = make(map[fileCacheKey]*fileCache)
	}
}

func (conn *connection) saveFullQueueCh() int {
	return cap(conn.fullQueueCh)
}

func (conn *connection) loadFullQueueCh(_ goContext.Context, capacity int) {
	conn.fullQueueCh = make(chan struct{}, capacity)
}

// lostHostConn implements fuseConn for a restored connection whose server
// was outside the sandbox (see connection.hostTransport). Such connections are
// aborted before they are saved (see filesystem.PrepareSave), so call and
// interrupt are unreachable, but for a live fork's copy, whose source keeps
// the connection: its requests fail with ENOTCONN. lostHostConn also preserves
// hostConnection's reference-counting behavior on release.
type lostHostConn struct {
	conn *connection
}

// call implements fuseConn.call.
func (*lostHostConn) call(context.Context, *Request) (*Response, error) {
	return nil, linuxerr.ENOTCONN
}

// release implements fuseConn.release.
func (lc *lostHostConn) release(ctx context.Context) {
	// Mirrors hostConnection.release; the host FD is already gone.
	lc.conn.DecRef(ctx)
}

// interrupt implements fuseConn.interrupt.
func (*lostHostConn) interrupt(*futureResponse) {}

// PrepareSave implements vfs.FilesystemImplSaveRestoreExtension.PrepareSave.
func (fs *filesystem) PrepareSave(ctx context.Context, _ vfs.PrepareSaveOptions) error {
	if pgalloc.IsLiveForkSource(ctx) {
		// Before a live fork, revoke file-backed translations and mark every
		// cached page for lazy branch-local CoW on its next fault. The source
		// runs on, keeping its connection to a FUSE server outside the
		// sandbox; the copy's is lost (see connection.afterLoad).
		fs.cacheMu.Lock()
		for _, cache := range fs.fileCaches {
			cache.prepareLiveFork()
		}
		fs.cacheMu.Unlock()
		return nil
	}
	if fs.conn.hostTransport {
		// Connections to FUSE servers outside the sandbox can't be
		// preserved, since the host FD isn't saved. Abort the connection, as
		// in Linux when a FUSE server dies: outstanding requests fail with
		// ECONNABORTED, and subsequent requests (including after restore, or
		// if the sandbox resumes after the checkpoint) fail with ENOTCONN.
		// The filesystem must be remounted with a new connection to be used
		// again.
		//
		// Tasks have been stopped, so once the connection is aborted, no
		// requests are outstanding and none can be sent. The host
		// connection's reader goroutine may still run, but it no longer
		// modifies connection state since it has no requests to complete.
		log.Warningf("fusefs: aborting connection to FUSE server outside the sandbox, which can't be checkpointed")
		fs.conn.Abort(ctx)
	}
	return nil
}

// BeforeResume implements vfs.FilesystemImplSaveRestoreExtension.BeforeResume.
func (fs *filesystem) BeforeResume(ctx context.Context) {}

// CompleteRestore implements
// vfs.FilesystemImplSaveRestoreExtension.CompleteRestore.
func (fs *filesystem) CompleteRestore(ctx context.Context, opts vfs.CompleteRestoreOptions) error {
	return nil
}
