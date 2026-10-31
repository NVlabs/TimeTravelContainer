// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cgroup2fs

import (
	"gvisor.dev/gvisor/pkg/state"
)

// How this package's types load from what builds before TTC's merge of
// upstream gVisor through 2026-10-28 (TTC's dailies through 20261023) saved;
// see pkg/state/migrate.go, and runsc/ttcd/schemas for their shapes.
func init() {
	// Older builds had no cgroup.freeze: no cgroup was frozen, and no task held
	// a freeze credit.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/sentry/fsimpl/cgroup2fs.cgroup",
		Fields: []string{"InodeAttrs", "InodeDirectoryNoNewChildren", "InodeNoopRefCount", "InodeNotAnonymous", "InodeNotSymlink", "InodeWatches", "OrderedChildren", "InodeFSOwned", "implStatFS", "locks", "fs", "parent", "eventsFile", "path", "level", "deleted", "children", "subtreeCtrls", "tasks", "tasksCount", "ctrls", "closestCtrls", "nrPopulatedChildren", "maxDescendants", "maxDepth", "nrDescendants", "killSeq", "xattrs", "bpf"},
		Zero:   []string{"freezeRequested", "nrFreezeCredits", "nrChildrenWithCredits"},
	})
}
