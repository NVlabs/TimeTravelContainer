// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package virtgpu

import (
	"fmt"
	"time"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sync"
)

// This file implements ForkSnapshot. A save of the device, for a fork or a
// checkpoint, prepares by taking a snapshot of its renderer, which the save
// saves. The loaded device keeps the contexts and resources of the saved
// generation, and its first use of a renderer, lazily, starts one that
// restores the snapshot. The guest's mappings of the renderer's memory map
// the restored renderer's.

// fenceWaitTimeout bounds how long a save waits for the fences that the
// renderer has yet to signal.
const fenceWaitTimeout = 10 * time.Second

var _ vfs.DeviceSaveRestoreExtension = (*device)(nil)

// PrepareSave implements vfs.DeviceSaveRestoreExtension.PrepareSave. With
// ForkSnapshot, it takes a snapshot of the renderer once the renderer has
// signaled every fence: the guest, which is stopped, may be waiting for
// them.
func (d *device) PrepareSave(ctx context.Context) error {
	if d.forkPolicy != ForkSnapshot {
		return nil
	}
	d.submitMu.Lock()
	defer d.submitMu.Unlock()
	if d.renderer == nil && d.snapshot != nil && d.snapshotReference {
		// A load left a reference snapshot, which the device restores
		// before the save takes one of its own: what the reference refers
		// to is the renderer's until the device that holds it restores it.
		if _, err := d.startRendererLocked(); err != nil {
			return fmt.Errorf("restoring the GPU's snapshot: %w", err)
		}
	}
	if d.renderer == nil {
		// There is nothing to save, or the snapshot that a load left for
		// the next renderer, which d.snapshot still is.
		return nil
	}
	if err := d.waitForFencesLocked(fenceWaitTimeout); err != nil {
		return err
	}
	d.mu.Lock()
	lost := d.lost
	d.mu.Unlock()
	if lost {
		// A load loses the lost renderer's contexts and resources.
		return nil
	}
	snapshot, err := d.renderer.Snapshot()
	if err != nil {
		return fmt.Errorf("taking a snapshot of the GPU's renderer: %w", err)
	}
	d.snapshot = snapshot.Data
	d.snapshotReference = snapshot.Reference
	d.snapshotTaken = true
	if snapshot.Reference {
		takeSnapshot(snapshot.Data)
	}
	return nil
}

// BeforeResume implements vfs.DeviceSaveRestoreExtension.BeforeResume. The
// source of a save holds a reference snapshot that it took until the kernels
// that load the save are loaded: when the save ends, or, for the batch
// siblings of a live fork, once their loads end.
func (d *device) BeforeResume(ctx context.Context) {
	d.submitMu.Lock()
	var release func()
	if d.snapshotTaken {
		if d.snapshotReference {
			release = d.releaseSourceSnapshot(d.snapshot, d.renderer)
		}
		d.snapshot = nil
		d.snapshotReference = false
		d.snapshotTaken = false
	}
	d.submitMu.Unlock()
	if release == nil {
		return
	}
	if loads := vfs.LiveForkLoadsFromContext(ctx); loads != nil {
		loads.AfterLoads(release)
	} else {
		release()
	}
}

// releaseSourceSnapshot returns a function that uncounts the source of a
// save as a holder of snapshot, which renderer r took, and tells r to forget
// it if no device holds it.
func (d *device) releaseSourceSnapshot(snapshot []byte, r Renderer) func() {
	return func() {
		if !releaseSnapshot(snapshot, true /* source */) || r == nil {
			return
		}
		// No device that a load of the save made holds the snapshot.
		d.submitMu.Lock()
		defer d.submitMu.Unlock()
		r.ForgetSnapshot(snapshot)
	}
}

// Reference snapshots, which renderers keep until the devices that hold them
// restore them (see Snapshot), are counted by the devices of the Sentry's
// process that hold them: the source of a save, until the kernels that load
// the save are loaded, and the devices that the save loads, until they
// restore the snapshot. The last device to hold one restores it as the last,
// which lets the renderer take what it kept rather than copy it, or tells the
// renderer to forget it.
var referenceSnapshots = struct {
	mu sync.Mutex
	m  map[string]*snapshotHolders
}{m: make(map[string]*snapshotHolders)}

// snapshotHolders counts the devices that hold a reference snapshot.
type snapshotHolders struct {
	count int
	// sourceDone is closed when the source of the save holds the snapshot
	// no more.
	sourceDone chan struct{}
}

// takeSnapshot counts the source of a save, which took snapshot, as its
// holder.
func takeSnapshot(snapshot []byte) {
	referenceSnapshots.mu.Lock()
	defer referenceSnapshots.mu.Unlock()
	referenceSnapshots.m[string(snapshot)] = &snapshotHolders{count: 1, sourceDone: make(chan struct{})}
}

// holdSnapshot counts a loaded device as a holder of snapshot, and returns
// its holders, or nil if no device of the process took it.
func holdSnapshot(snapshot []byte) *snapshotHolders {
	referenceSnapshots.mu.Lock()
	defer referenceSnapshots.mu.Unlock()
	h := referenceSnapshots.m[string(snapshot)]
	if h != nil {
		h.count++
	}
	return h
}

// releaseSnapshot uncounts a holder of snapshot, the source of its save, or
// a device that restores it. It reports whether no device holds it any
// more, or none of the process ever did.
func releaseSnapshot(snapshot []byte, source bool) bool {
	referenceSnapshots.mu.Lock()
	defer referenceSnapshots.mu.Unlock()
	h := referenceSnapshots.m[string(snapshot)]
	if h == nil {
		return true
	}
	if source {
		close(h.sourceDone)
	}
	h.count--
	if h.count > 0 {
		return false
	}
	delete(referenceSnapshots.m, string(snapshot))
	return true
}

// sourceWaitTimeout bounds how long a loaded device waits for the source of
// a save to release a reference snapshot, before it restores it.
const sourceWaitTimeout = 30 * time.Second

// restoreEagerly restores a loaded device's reference snapshot, once the
// source of the save holds it no more, so that the device restores it as
// the last when it is.
func (d *device) restoreEagerly(h *snapshotHolders) {
	defer close(d.restoredEagerly)
	if h != nil {
		select {
		case <-h.sourceDone:
		case <-time.After(sourceWaitTimeout):
		}
	}
	d.submitMu.Lock()
	defer d.submitMu.Unlock()
	if d.renderer == nil && d.snapshot != nil {
		d.startRendererLocked()
	}
}

// waitForFencesLocked waits until no fence is pending, the renderer is lost,
// or timeout passes.
//
// Preconditions: d.submitMu is locked, so that no fence is created.
func (d *device) waitForFencesLocked(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		d.mu.Lock()
		idle := len(d.timelines) == 0 || d.lost
		d.mu.Unlock()
		if idle {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the GPU did not finish its work in %v", timeout)
		}
		time.Sleep(time.Millisecond)
	}
}

// restoreLocked gives the new renderer r, of generation gen, the snapshot
// that a load left. It returns the generation of r, which is a new one if
// the restore failed and lost the snapshot's contexts and resources.
//
// Preconditions: d.submitMu is locked. d.snapshot is not nil.
func (d *device) restoreLocked(r Renderer, gen uint64) (Renderer, uint64, error) {
	snapshot := d.snapshot
	reference := d.snapshotReference
	d.snapshot = nil
	d.snapshotReference = false
	last := true
	if reference {
		last = releaseSnapshot(snapshot, false /* source */)
	}
	sizes := make(map[uint32]uint64)
	d.mu.Lock()
	for id, res := range d.resources {
		if res.gen == gen {
			sizes[id] = res.size
		}
	}
	d.mu.Unlock()
	err := r.Restore(snapshot, sizes, last)
	if err == nil {
		return r, gen, nil
	}
	log.Warningf("virtgpu: restoring the GPU's snapshot: %v", err)
	r.Close()
	d.mu.Lock()
	d.gen++
	d.lost = false
	d.contexts = 0
	d.genResources = 0
	gen = d.gen
	d.mu.Unlock()
	r, err = d.newRenderer(&rendererEvents{dev: d, gen: gen})
	return r, gen, err
}
