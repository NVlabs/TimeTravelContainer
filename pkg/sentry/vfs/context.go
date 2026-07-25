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

package vfs

import (
	goContext "context"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
)

// contextID is this package's type for context.Context.Value keys.
type contextID int

const (
	// CtxMountNamespace is a Context.Value key for a MountNamespace.
	CtxMountNamespace contextID = iota

	// CtxRoot is a Context.Value key for a VFS root.
	CtxRoot

	// CtxRestoreFilesystemFDMap is a Context.Value key for a
	// map[checkpoint.ResourceID]int mapping filesystem unique IDs (cf.
	// gofer.InternalFilesystemOptions.UniqueID) to host FDs.
	CtxRestoreFilesystemFDMap

	// CtxRestoreFilesystemResources maps serialized filesystem identities to
	// replacement in-process resources supplied by a standalone restore.
	CtxRestoreFilesystemResources

	// CtxLiveForkFilesystemResources is a Context.Value key for in-process
	// filesystem resources indexed by stable filesystem ID.
	CtxLiveForkFilesystemResources

	// CtxLiveForkChildBranchID is the destination branch ID for an in-process
	// live fork.
	CtxLiveForkChildBranchID

	// CtxLiveForkTransactions collects filesystem-side transactions that must
	// commit only after the child kernel has been restored successfully.
	CtxLiveForkTransactions
)

// RestoreFilesystemResource associates an implementation-specific resource
// with the persistent branch that should back the restored filesystem.
type RestoreFilesystemResource struct {
	Resource any
	Branch   string
}

// RestoreFilesystemResources is keyed by the identity stored in checkpoint
// state, which may differ from the identity of a newly imported repository.
type RestoreFilesystemResources map[checkpoint.ResourceID]RestoreFilesystemResource

// RestoreFilesystemResourcesFromContext returns standalone-restore resources.
func RestoreFilesystemResourcesFromContext(ctx goContext.Context) RestoreFilesystemResources {
	resources, _ := ctx.Value(CtxRestoreFilesystemResources).(RestoreFilesystemResources)
	return resources
}

// MountNamespaceFromContext returns the MountNamespace used by ctx. If ctx is
// not associated with a MountNamespace, MountNamespaceFromContext returns nil.
//
// A reference is taken on the returned MountNamespace.
func MountNamespaceFromContext(ctx goContext.Context) *MountNamespace {
	if v := ctx.Value(CtxMountNamespace); v != nil {
		return v.(*MountNamespace)
	}
	return nil
}

// LiveForkFilesystemResources maps stable filesystem IDs to implementation-
// specific resources borrowed by an in-process clone.
type LiveForkFilesystemResources map[checkpoint.ResourceID]any

// LiveForkFilesystemResourcesFromContext returns live-fork resources, or nil.
func LiveForkFilesystemResourcesFromContext(ctx goContext.Context) LiveForkFilesystemResources {
	resources, _ := ctx.Value(CtxLiveForkFilesystemResources).(LiveForkFilesystemResources)
	return resources
}

// LiveForkChildBranchIDFromContext returns the destination live-fork branch.
func LiveForkChildBranchIDFromContext(ctx goContext.Context) string {
	id, _ := ctx.Value(CtxLiveForkChildBranchID).(string)
	return id
}

// LiveForkTransaction is a filesystem-side mutation coupled to Kernel.LiveFork.
type LiveForkTransaction interface {
	Commit() error
	Abort() error
}

// LiveForkTransactions is a transaction list populated during VFS.PrepareSave.
// VFS preparation is serialized, so it does not require an internal lock.
type LiveForkTransactions struct {
	transactions []LiveForkTransaction
}

// Add registers tx for commit or rollback by Kernel.LiveFork.
func (ts *LiveForkTransactions) Add(tx LiveForkTransaction) {
	ts.transactions = append(ts.transactions, tx)
}

// Commit commits all registered transactions in preparation order.
func (ts *LiveForkTransactions) Commit() error {
	for _, tx := range ts.transactions {
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Abort rolls all registered transactions back in reverse preparation order.
func (ts *LiveForkTransactions) Abort() error {
	var first error
	for i := len(ts.transactions) - 1; i >= 0; i-- {
		if err := ts.transactions[i].Abort(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// LiveForkTransactionsFromContext returns the filesystem transaction list.
func LiveForkTransactionsFromContext(ctx goContext.Context) *LiveForkTransactions {
	transactions, _ := ctx.Value(CtxLiveForkTransactions).(*LiveForkTransactions)
	return transactions
}

// RestoreFilesystemFDMapFromContext returns the RestoreFilesystemFDMap used
// by ctx. If ctx is not associated with a RestoreFilesystemFDMap, returns nil.
func RestoreFilesystemFDMapFromContext(ctx goContext.Context) map[checkpoint.ResourceID]int {
	fdmap, ok := ctx.Value(CtxRestoreFilesystemFDMap).(map[checkpoint.ResourceID]int)
	if !ok {
		return nil
	}
	return fdmap
}

type mountNamespaceContext struct {
	context.Context
	mntns *MountNamespace
}

// Value implements Context.Value.
func (mc mountNamespaceContext) Value(key any) any {
	switch key {
	case CtxMountNamespace:
		mc.mntns.IncRef()
		return mc.mntns
	default:
		return mc.Context.Value(key)
	}
}

// WithMountNamespace returns a copy of ctx with the given MountNamespace.
func WithMountNamespace(ctx context.Context, mntns *MountNamespace) context.Context {
	return &mountNamespaceContext{
		Context: ctx,
		mntns:   mntns,
	}
}

// RootFromContext returns the VFS root used by ctx. It takes a reference on
// the returned VirtualDentry. If ctx does not have a specific VFS root,
// RootFromContext returns a zero-value VirtualDentry.
func RootFromContext(ctx goContext.Context) VirtualDentry {
	if v := ctx.Value(CtxRoot); v != nil {
		return v.(VirtualDentry)
	}
	return VirtualDentry{}
}

type rootContext struct {
	context.Context
	root VirtualDentry
}

// WithRoot returns a copy of ctx with the given root.
func WithRoot(ctx context.Context, root VirtualDentry) context.Context {
	return &rootContext{
		Context: ctx,
		root:    root,
	}
}

// Value implements Context.Value.
func (rc rootContext) Value(key any) any {
	switch key {
	case CtxRoot:
		rc.root.IncRef()
		return rc.root
	default:
		return rc.Context.Value(key)
	}
}
