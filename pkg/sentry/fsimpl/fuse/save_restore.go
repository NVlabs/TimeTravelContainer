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

package fuse

import (
	goContext "context"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

var _ vfs.FilesystemImplSaveRestoreExtension = (*filesystem)(nil)

func (fRes *futureResponse) afterLoad(goContext.Context) {
	fRes.ch = make(chan struct{})
}

func (conn *connection) afterLoad(goContext.Context) {
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

// PrepareSave implements vfs.FilesystemImplSaveRestoreExtension. Before a
// live fork, revoke file-backed translations and mark every cached page for
// lazy branch-local CoW on its next fault.
func (fs *filesystem) PrepareSave(ctx context.Context) error {
	if !pgalloc.IsLiveForkSource(ctx) {
		return nil
	}
	fs.cacheMu.Lock()
	for _, cache := range fs.fileCaches {
		cache.prepareLiveFork()
	}
	fs.cacheMu.Unlock()
	return nil
}

// BeforeResume implements vfs.FilesystemImplSaveRestoreExtension.
func (fs *filesystem) BeforeResume(context.Context) {}

// CompleteRestore implements vfs.FilesystemImplSaveRestoreExtension.
func (fs *filesystem) CompleteRestore(context.Context, vfs.CompleteRestoreOptions) error {
	return nil
}

func (conn *connection) saveFullQueueCh() int {
	return cap(conn.fullQueueCh)
}

func (conn *connection) loadFullQueueCh(_ goContext.Context, capacity int) {
	conn.fullQueueCh = make(chan struct{}, capacity)
}
