// Copyright 2019 The gVisor Authors.
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

package pgalloc

import (
	"context"

	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
)

// contextID is this package's type for context.Context.Value keys.
type contextID int

const (
	// CtxMemoryFile is a Context.Value key for a MemoryFile.
	CtxMemoryFile contextID = iota

	// CtxMemoryCgroupID is the memory cgroup id which the task belongs to.
	CtxMemoryCgroupID

	// CtxMemoryFileMap is a Context.Value key for mapping
	// MemoryFileOpts.RestoreID to *MemoryFile. This is used for save/restore.
	CtxMemoryFileMap

	// CtxLiveForkClone indicates that state is being loaded as an in-memory
	// live fork. Restored page owners must acquire references to existing
	// MemoryFile ranges instead of relying on MemoryFile restore to create them.
	CtxLiveForkClone

	// CtxLiveForkSource indicates that save preparation is for an in-memory
	// live fork. Mutable filesystem objects must establish CoW before metadata
	// serialization begins.
	CtxLiveForkSource

	// CtxLiveForkRemoteMemoryClone indicates that a live-fork child uses a
	// separately cloned MemoryFile whose allocator metadata already contains
	// the complete target object graph's page references. Live-fork restore
	// hooks must still run, but must not increment page references again.
	CtxLiveForkRemoteMemoryClone

	// CtxMemoryRangeCapture identifies the branch-scoped set of MemoryFile
	// ranges referenced by the state graph currently being saved. Save hooks
	// for MemoryFile-backed objects use this to opt their ranges into sparse
	// distributed-fork and dehydration images.
	CtxMemoryRangeCapture
)

// IsLiveForkClone returns true if ctx is loading an in-memory live fork.
func IsLiveForkClone(ctx context.Context) bool {
	if v := ctx.Value(CtxLiveForkClone); v != nil {
		return v.(bool)
	}
	return false
}

// IsLiveForkRemoteMemoryClone returns true if the live-fork target has a
// distinct, metadata-cloned MemoryFile.
func IsLiveForkRemoteMemoryClone(ctx context.Context) bool {
	if v := ctx.Value(CtxLiveForkRemoteMemoryClone); v != nil {
		return v.(bool)
	}
	return false
}

// LiveForkCloneNeedsPageRefs returns true when restore hooks must acquire
// references in a shared source MemoryFile. Remote-memory clones inherit the
// graph's reference counts in their cloned allocator metadata instead.
func LiveForkCloneNeedsPageRefs(ctx context.Context) bool {
	return IsLiveForkClone(ctx) && !IsLiveForkRemoteMemoryClone(ctx)
}

// IsLiveForkSource returns true if ctx is preparing an in-memory live fork.
func IsLiveForkSource(ctx context.Context) bool {
	if v := ctx.Value(CtxLiveForkSource); v != nil {
		return v.(bool)
	}
	return false
}

// MemoryFileFromContext returns the MemoryFile used by ctx, or nil if no such
// MemoryFile exists.
func MemoryFileFromContext(ctx context.Context) *MemoryFile {
	if v := ctx.Value(CtxMemoryFile); v != nil {
		return v.(*MemoryFile)
	}
	return nil
}

// MemoryCgroupIDFromContext returns the memory cgroup id of the ctx, or
// zero if the ctx does not belong to any memory cgroup.
func MemoryCgroupIDFromContext(ctx context.Context) uint32 {
	if v := ctx.Value(CtxMemoryCgroupID); v != nil {
		return v.(uint32)
	}
	return 0
}

// MemoryFileMapFromContext returns the memory file map used by ctx, or nil if
// no such map exists.
func MemoryFileMapFromContext(ctx context.Context) map[checkpoint.ResourceID]*MemoryFile {
	if v := ctx.Value(CtxMemoryFileMap); v != nil {
		return v.(map[checkpoint.ResourceID]*MemoryFile)
	}
	return nil
}

// MemoryRangeCaptureFromContext returns the branch-scoped memory capture used
// by ctx, or nil when the caller is performing an ordinary checkpoint.
func MemoryRangeCaptureFromContext(ctx context.Context) *MemoryRangeCapture {
	if v := ctx.Value(CtxMemoryRangeCapture); v != nil {
		return v.(*MemoryRangeCapture)
	}
	return nil
}
