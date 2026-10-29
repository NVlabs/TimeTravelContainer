// Copyright 2020 The gVisor Authors.
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

package vfs

import (
	"cmp"
	goContext "context"
	"fmt"
	"slices"
	"sort"
	"sync/atomic"
	"time"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/refs"
	gsync "gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/waiter"
)

// ErrCorruption indicates a failed restore due to external file system state in
// corruption.
type ErrCorruption struct {
	// Err is the wrapped error.
	Err error
}

// afterLoad is called by stateify after reconstructing a Dentry.
//
// Dentry.mu is runtime synchronization state, not part of the Linux object
// graph. In particular, a live-fork source can be stopped while a task or an
// internal VFS operation holds the mutex. A restored dentry must never inherit
// that transient ownership: the owner goroutine exists only in the source
// kernel, so retaining the locked word leaves the child permanently blocked
// in operations such as close(2), unlink(2), and branch destruction.
//
// The field is state:"nosave", which normally causes a newly allocated
// object to receive the zero value. Reset it explicitly as a restore invariant
// as well: state loading is also allowed to populate an existing object, and
// live-fork correctness must not depend on allocator freshness.
func (d *Dentry) afterLoad(goContext.Context) {
	d.mu = gsync.Mutex{}
}

// Error returns a sensible description of the restore error.
func (e ErrCorruption) Error() string {
	return "restore failed due to external file system state in corruption: " + e.Err.Error()
}

// PrependErrMsg prepends the passed prefix to the error while preserving
// special vfs errors as the outer most error.
func PrependErrMsg(prefix string, err error) error {
	switch terr := err.(type) {
	case ErrCorruption:
		terr.Err = fmt.Errorf("%s: %w", prefix, terr.Err)
		return terr
	default:
		return fmt.Errorf("%s: %w", prefix, err)
	}
}

// LiveForkFilesystemDecision describes how one mounted filesystem participates
// in an in-memory fork.
type LiveForkFilesystemDecision string

const (
	// LiveForkFilesystemClone means all observable state is serialized into a
	// branch-local filesystem graph, with file pages shared copy-on-write.
	LiveForkFilesystemClone LiveForkFilesystemDecision = "clone"
	// LiveForkFilesystemShareExternal means the cloned graph borrows a
	// read-only external resource. The caller must explicitly allow sharing.
	LiveForkFilesystemShareExternal LiveForkFilesystemDecision = "share-external"
	// LiveForkFilesystemReject means that the filesystem cannot be live-forked.
	LiveForkFilesystemReject LiveForkFilesystemDecision = "reject"
)

// LiveForkFilesystemEligibility is an implementation-level decision.
// StableIdentity identifies external resources across the save/load boundary,
// and SourceClass names the implementation-defined origin of filesystem data.
type LiveForkFilesystemEligibility struct {
	Decision              LiveForkFilesystemDecision
	RequiresReadOnlyMount bool
	ReadOnly              bool
	StableIdentity        string
	SourceClass           string
	Reason                string
}

// FilesystemImplLiveForkExtension is implemented by filesystems that have
// explicitly classified their live-fork behavior. Unclassified filesystems are
// rejected by default. The method must be read-only.
type FilesystemImplLiveForkExtension interface {
	LiveForkFilesystemEligibility() LiveForkFilesystemEligibility
}

// LiveForkFilesystemRecord records the decision for one mounted or otherwise
// referenced filesystem. MountID is zero and MountPath is empty when the
// filesystem is reachable only through another filesystem, such as a gofer
// lower hidden by OverlayFS.
type LiveForkFilesystemRecord struct {
	MountID        uint64                     `json:"mount_id,omitempty"`
	MountPath      string                     `json:"mount_path,omitempty"`
	Mounted        bool                       `json:"mounted"`
	FilesystemType string                     `json:"filesystem_type"`
	SourceClass    string                     `json:"source_class"`
	ReadOnly       bool                       `json:"read_only"`
	Decision       LiveForkFilesystemDecision `json:"decision"`
	StableIdentity string                     `json:"stable_identity"`
	Reason         string                     `json:"reason,omitempty"`
}

// LiveForkFilesystemEligibilities classifies every mounted and otherwise
// referenced filesystem in vfs. The operation is read-only.
func (vfs *VirtualFilesystem) LiveForkFilesystemEligibilities(ctx context.Context) []LiveForkFilesystemRecord {
	mounts := vfs.GetAllMounts(ctx)
	defer func() {
		for _, mount := range mounts {
			mount.DecRef(ctx)
		}
	}()
	filesystems := vfs.GetFilesystems()
	defer func() {
		for _, fs := range filesystems {
			fs.DecRef(ctx)
		}
	}()

	mountPath := func(mount *Mount) string {
		vfs.lockMounts()
		ns := mount.ns
		if ns == nil || !ns.TryIncRef() {
			vfs.unlockMounts(ctx)
			return ""
		}
		vfs.unlockMounts(ctx)
		defer ns.DecRef(ctx)

		vfsRoot := ns.Root(ctx)
		if !vfsRoot.Ok() {
			return ""
		}
		defer vfsRoot.DecRef(ctx)
		mountRoot := VirtualDentry{mount: mount, dentry: mount.Root()}
		mountRoot.IncRef()
		defer mountRoot.DecRef(ctx)
		path, err := vfs.PathnameReachable(ctx, vfsRoot, mountRoot)
		if err != nil {
			return ""
		}
		return path
	}

	classify := func(fs *Filesystem, mountID uint64, mountPath string, mounted, mountReadOnly bool) LiveForkFilesystemRecord {
		record := LiveForkFilesystemRecord{
			MountID:        mountID,
			MountPath:      mountPath,
			Mounted:        mounted,
			FilesystemType: fs.FilesystemType().Name(),
			SourceClass:    "unclassified",
			ReadOnly:       mountReadOnly,
			Decision:       LiveForkFilesystemReject,
			StableIdentity: fmt.Sprintf("filesystem:%s", fs.FilesystemType().Name()),
			Reason:         "filesystem implementation has not classified its live-fork behavior",
		}
		if mounted {
			record.StableIdentity = fmt.Sprintf("mount:%d", mountID)
		}
		requiresReadOnlyMount := false
		if ext, ok := fs.Impl().(FilesystemImplLiveForkExtension); ok {
			eligibility := ext.LiveForkFilesystemEligibility()
			requiresReadOnlyMount = eligibility.RequiresReadOnlyMount
			record.Decision = eligibility.Decision
			record.SourceClass = eligibility.SourceClass
			record.Reason = eligibility.Reason
			if !mounted {
				record.ReadOnly = eligibility.ReadOnly
			}
			if eligibility.StableIdentity != "" {
				record.StableIdentity = eligibility.StableIdentity
			}
			if record.Decision == LiveForkFilesystemShareExternal && !eligibility.ReadOnly {
				record.Decision = LiveForkFilesystemReject
				record.Reason = "external filesystem resource is not read-only"
			}
		}
		if record.SourceClass == "" {
			switch record.Decision {
			case LiveForkFilesystemClone:
				record.SourceClass = "sentry-owned"
			case LiveForkFilesystemShareExternal:
				record.SourceClass = "external-read-only"
			default:
				record.SourceClass = "unclassified"
			}
		}
		if mounted && !mountReadOnly && requiresReadOnlyMount {
			record.Decision = LiveForkFilesystemReject
			record.Reason = "filesystem requires a read-only mount"
		}
		return record
	}

	records := make([]LiveForkFilesystemRecord, 0, len(mounts)+len(filesystems))
	mountedFilesystems := make(map[*Filesystem]struct{})
	for _, mount := range mounts {
		fs := mount.Filesystem()
		mountedFilesystems[fs] = struct{}{}
		records = append(records, classify(fs, mount.ID, mountPath(mount), true, mount.ReadOnly()))
	}
	for _, fs := range filesystems {
		if _, ok := mountedFilesystems[fs]; ok {
			continue
		}
		records = append(records, classify(fs, 0, "", false, false))
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].MountID != records[j].MountID {
			return records[i].MountID < records[j].MountID
		}
		if records[i].FilesystemType != records[j].FilesystemType {
			return records[i].FilesystemType < records[j].FilesystemType
		}
		return records[i].StableIdentity < records[j].StableIdentity
	})
	return records
}

// FilesystemImplSaveRestoreExtension is an optional extension to
// FilesystemImpl.
type FilesystemImplSaveRestoreExtension interface {
	// PrepareSave prepares this filesystem for serialization.
	PrepareSave(ctx context.Context, opts PrepareSaveOptions) error

	// BeforeResume is called before the kernel is resumed after save. It can be
	// used to clean up any state that should be discarded after save.
	BeforeResume(ctx context.Context)

	// CompleteRestore completes restoration from checkpoint for this
	// filesystem after deserialization.
	CompleteRestore(ctx context.Context, opts CompleteRestoreOptions) error
}

// PrepareSave prepares all filesystems for serialization.
func (vfs *VirtualFilesystem) PrepareSave(ctx context.Context, opts PrepareSaveOptions) error {
	fss := vfs.GetFilesystems()
	defer func() {
		for _, fs := range fss {
			fs.DecRef(ctx)
		}
	}()
	for _, fs := range fss {
		if ext, ok := fs.impl.(FilesystemImplSaveRestoreExtension); ok {
			if err := ext.PrepareSave(ctx, opts); err != nil {
				return err
			}
		}
	}
	// Devices prepare last: what they prepare, such as a GPU's snapshot, is
	// wasted if a filesystem fails.
	devices := vfs.saveRestoreDevices()
	started := time.Now()
	for i, dev := range devices {
		if err := dev.PrepareSave(ctx); err != nil {
			for _, prepared := range devices[:i] {
				prepared.BeforeResume(ctx)
			}
			return err
		}
	}
	if metrics := LiveForkPreparationMetricsFromContext(ctx); metrics != nil {
		metrics.Devices += time.Since(started)
	}
	return nil
}

// PrepareSaveOptions contains options to
// VirtualFilesystem.PrepareSave() and
// FilesystemImplSaveRestoreExtension.PrepareSave().
type PrepareSaveOptions struct {
	// If ValidateMetadata is true, filesystem implementations backed by
	// remote filesystems should refresh metadata during save in case
	// metadata validation will be performed during restore.
	ValidateMetadata bool
}

// BeforeResume is called before the kernel is resumed after save and allows
// filesystems and devices to clean up S/R state.
func (vfs *VirtualFilesystem) BeforeResume(ctx context.Context) {
	for _, fs := range vfs.GetFilesystems() {
		if ext, ok := fs.impl.(FilesystemImplSaveRestoreExtension); ok {
			ext.BeforeResume(ctx)
		}
		fs.DecRef(ctx)
	}
	for _, dev := range vfs.saveRestoreDevices() {
		dev.BeforeResume(ctx)
	}
}

// DeviceSaveRestoreExtension is a Device with state outside the sandbox that
// it prepares for a save, of a checkpoint or a live fork.
type DeviceSaveRestoreExtension interface {
	Device

	// PrepareSave prepares the device for a save. An error fails the save.
	PrepareSave(ctx context.Context) error

	// BeforeResume undoes PrepareSave, once the save is done.
	BeforeResume(ctx context.Context)
}

// LiveForkLoads is the loads of a save that happen after the save ends: the
// batch siblings of a live fork load the metadata that the save of their
// primary sibling captured. What a device keeps for the kernels that load its
// state outlives such a save until the loads end.
type LiveForkLoads struct {
	mu    gsync.Mutex
	done  bool
	after []func()
}

// AfterLoads calls f once the loads end, or now if they did.
func (l *LiveForkLoads) AfterLoads(f func()) {
	l.mu.Lock()
	if !l.done {
		l.after = append(l.after, f)
		l.mu.Unlock()
		return
	}
	l.mu.Unlock()
	f()
}

// Done ends the loads, which succeeded or failed.
func (l *LiveForkLoads) Done() {
	l.mu.Lock()
	after := l.after
	l.after = nil
	l.done = true
	l.mu.Unlock()
	for _, f := range after {
		f()
	}
}

// LiveForkLoadsFromContext returns the loads that follow the save of ctx,
// or nil if the save's loads, if any, end before the save does.
func LiveForkLoadsFromContext(ctx goContext.Context) *LiveForkLoads {
	loads, _ := ctx.Value(CtxLiveForkLoads).(*LiveForkLoads)
	return loads
}

// saveRestoreDevices returns the registered devices that implement
// DeviceSaveRestoreExtension, in the order of their device numbers.
func (vfs *VirtualFilesystem) saveRestoreDevices() []DeviceSaveRestoreExtension {
	vfs.devicesMu.RLock()
	defer vfs.devicesMu.RUnlock()
	tuples := make([]devTuple, 0, len(vfs.devices))
	for tup, rd := range vfs.devices {
		if _, ok := rd.dev.(DeviceSaveRestoreExtension); ok {
			tuples = append(tuples, tup)
		}
	}
	slices.SortFunc(tuples, func(a, b devTuple) int {
		return cmp.Or(cmp.Compare(a.kind, b.kind), cmp.Compare(a.major, b.major), cmp.Compare(a.minor, b.minor))
	})
	devices := make([]DeviceSaveRestoreExtension, 0, len(tuples))
	for _, tup := range tuples {
		devices = append(devices, vfs.devices[tup].dev.(DeviceSaveRestoreExtension))
	}
	return devices
}

// CompleteRestore completes restoration from checkpoint for all filesystems
// after deserialization.
func (vfs *VirtualFilesystem) CompleteRestore(ctx context.Context, opts *CompleteRestoreOptions) error {
	fss := vfs.GetFilesystems()
	defer func() {
		for _, fs := range fss {
			fs.DecRef(ctx)
		}
	}()
	for _, fs := range fss {
		if ext, ok := fs.impl.(FilesystemImplSaveRestoreExtension); ok {
			if err := ext.CompleteRestore(ctx, *opts); err != nil {
				return PrependErrMsg(fmt.Sprintf("failed to complete restore for filesystem type %q", fs.fsType.Name()), err)
			}
		}
	}
	return nil
}

// CompleteRestoreOptions contains options to
// VirtualFilesystem.CompleteRestore() and
// FilesystemImplSaveRestoreExtension.CompleteRestore().
type CompleteRestoreOptions struct {
	// If ValidateFileSizes is true, filesystem implementations backed by
	// remote filesystems should verify that file sizes have not changed
	// between checkpoint and restore.
	ValidateFileSizes bool

	// If ValidateFileModificationTimestamps is true, filesystem
	// implementations backed by remote filesystems should validate that file
	// mtimes have not changed between checkpoint and restore.
	ValidateFileModificationTimestamps bool
}

// saveMounts is called by stateify.
func (vfs *VirtualFilesystem) saveMounts() []*Mount {
	if atomic.LoadPointer(&vfs.mounts.slots) == nil {
		// vfs.Init() was never called.
		return nil
	}
	mounts := make([]*Mount, 0)
	vfs.mounts.Range(func(mount *Mount) bool {
		mounts = append(mounts, mount)
		return true
	})
	return mounts
}

// saveKey is called by stateify.
func (mnt *Mount) saveKey() VirtualDentry { return mnt.getKey() }

// saveMountPromises is called by stateify.
func (vfs *VirtualFilesystem) saveMountPromises() map[VirtualDentry]*mountPromise {
	m := make(map[VirtualDentry]*mountPromise)
	vfs.mountPromises.Range(func(key any, val any) bool {
		m[key.(VirtualDentry)] = val.(*mountPromise)
		return true
	})
	return m
}

// loadMounts is called by stateify.
func (vfs *VirtualFilesystem) loadMounts(_ goContext.Context, mounts []*Mount) {
	if mounts == nil {
		return
	}
	vfs.mounts.Init()
	for _, mount := range mounts {
		vfs.mounts.Insert(mount)
	}
}

// loadKey is called by stateify.
func (mnt *Mount) loadKey(_ goContext.Context, vd VirtualDentry) { mnt.setKey(vd) }

// loadMountPromises is called by stateify.
func (vfs *VirtualFilesystem) loadMountPromises(_ goContext.Context, mps map[VirtualDentry]*mountPromise) {
	for vd, mp := range mps {
		vfs.mountPromises.Store(vd, mp)
	}
}

// afterLoad is called by stateify.
func (mnt *Mount) afterLoad(goContext.Context) {
	if mnt.refs.Load() != 0 {
		refs.Register(mnt)
	}
}

// afterLoad is called by stateify.
func (epi *epollInterest) afterLoad(goContext.Context) {
	// Mark all epollInterests as ready after restore so that the next call to
	// EpollInstance.ReadEvents() rechecks their readiness.
	if epi.mask != 0 {
		epi.waiter.NotifyEvent(waiter.EventMaskFromLinux(epi.mask))
	}
}

// afterLoad is called by stateify.
func (vfs *VirtualFilesystem) afterLoad(goContext.Context) {
	if vfs.dynCharDevMajorShared == nil {
		vfs.dynCharDevMajorShared = make(map[SharedDynamicCharDevMajorKey]uint32)
	}
}
