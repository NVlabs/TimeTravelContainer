// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package virtgpu

import (
	"fmt"
	"time"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
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
	d.snapshot = snapshot
	d.snapshotTaken = true
	return nil
}

// BeforeResume implements vfs.DeviceSaveRestoreExtension.BeforeResume.
func (d *device) BeforeResume(ctx context.Context) {
	d.submitMu.Lock()
	defer d.submitMu.Unlock()
	if d.snapshotTaken {
		d.snapshot = nil
		d.snapshotTaken = false
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
	d.snapshot = nil
	sizes := make(map[uint32]uint64)
	d.mu.Lock()
	for id, res := range d.resources {
		if res.gen == gen {
			sizes[id] = res.size
		}
	}
	d.mu.Unlock()
	err := r.Restore(snapshot, sizes)
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
