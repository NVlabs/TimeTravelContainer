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

	"gvisor.dev/gvisor/pkg/ttcsnapshot"
)

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
	return r.withLock(r.garbageCollectLocked)
}

func (r *Repository) garbageCollectLocked() error {
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
		return errors.New("repository has no published branches")
	}

	// Drop decoded state for removed branch IDs before graph collection.
	r.metadataMu.Lock()
	for id := range r.metadata {
		if _, ok := liveBranchIDs[id]; !ok {
			delete(r.metadata, id)
		}
	}
	r.metadataMu.Unlock()

	if err := r.graph.ReloadForWrite(); err != nil {
		return err
	}
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
	return nil
}
