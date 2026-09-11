// Copyright 2026 The gVisor Authors.
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

package ttcvolume

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/ttcsnapshot"
)

// ErrEmptyRepository reports a repository whose last branch pointer was
// already removed by the runtime before daemon-side lifecycle cleanup. The
// caller may remove a family-private repository as a whole; named repositories
// retain the error because losing their canonical pointer is corruption.
var ErrEmptyRepository = errors.New("repository has no published branches")

// GarbageCollect removes graph metadata and data objects that are unreachable
// from every published branch pointer. Repository mutation and collection use
// the same cross-process flock, so a concurrent Sentry fork or write either
// precedes the reachability scan or follows all removals.
//
// GarbageCollect requires a path-backed Repository opened by Manager. Donated
// Sentry repositories intentionally omit their host path and only unlink
// branch pointers; ttcd performs collection after the Kernel is gone.
func (r *Repository) GarbageCollect() error {
	if r == nil || r.path == "" {
		return errors.New("garbage collection requires a path-backed repository")
	}
	// A live Sentry may have installed complete bulk bytes whose corresponding
	// graph objects are intentionally still in its bounded metadata batch. That
	// process-local tail is invisible to a separately opened manager handle.
	// Skip this maintenance pass while any descriptor-backed runtime holds a
	// shared lifecycle lease; the Sentry flushes the tail on close, and a later
	// pass (including crash recovery) can then collect safely.
	active, err := os.OpenFile(filepath.Join(r.path, "active.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("opening repository lifecycle lock: %w", err)
	}
	if err := unix.Flock(int(active.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		closeErr := active.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return closeErr
		}
		return errors.Join(fmt.Errorf("locking repository lifecycle: %w", err), closeErr)
	}
	defer func() {
		_ = unix.Flock(int(active.Fd()), unix.LOCK_UN)
		_ = active.Close()
	}()
	return r.withLock(r.garbageCollectLocked)
}

func (r *Repository) garbageCollectLocked() error {
	if err := r.flushAllMetadataBatchesLocked(); err != nil {
		return fmt.Errorf("publishing metadata before garbage collection: %w", err)
	}
	branchDir := filepath.Join(r.path, "branches")
	entries, err := os.ReadDir(branchDir)
	if err != nil {
		return err
	}
	liveBranchIDs := make(map[string]struct{})
	var workingSnapshots []ttcsnapshot.SnapshotID
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if err := validateName(id, "branch"); err != nil {
			return fmt.Errorf("invalid branch pointer %q during garbage collection: %w", entry.Name(), err)
		}
		branch, err := r.readBranch(id)
		if err != nil {
			return err
		}
		liveBranchIDs[id] = struct{}{}
		workingSnapshots = append(workingSnapshots, ttcsnapshot.SnapshotID(branch.WorkingSnapshot))
	}
	if len(liveBranchIDs) == 0 {
		return ErrEmptyRepository
	}

	// Drop decoded state for removed branch IDs before graph collection.
	r.metadataMu.Lock()
	for id := range r.metadata {
		if _, ok := liveBranchIDs[id]; !ok {
			delete(r.metadata, id)
		}
	}
	r.metadataMu.Unlock()

	for {
		collected, err := r.graph.Collect(1024)
		if err != nil {
			return err
		}
		if collected == 0 {
			break
		}
	}
	if _, err := r.graph.CollectEmptySnapshots(workingSnapshots); err != nil {
		return err
	}
	_, _, err = r.data.ReclaimUnreferenced(r.graph)
	if err != nil {
		return err
	}
	// ReclaimUnreferenced may remove objects dirtied before their branch was
	// deleted. Drop only bookkeeping whose graph object is now absent; a later
	// durability fence must not try to reopen reclaimed payloads.
	r.dirtyMu.Lock()
	for name := range r.dirtyData {
		id, parseErr := ttcsnapshot.ParseObjectID(name)
		if parseErr != nil {
			continue
		}
		if _, ok := r.graph.Object(id); !ok {
			delete(r.dirtyData, name)
			delete(r.pendingReclaims, name)
		}
	}
	r.dirtyMu.Unlock()
	return r.graph.CheckpointExternallyCoordinated()
}
