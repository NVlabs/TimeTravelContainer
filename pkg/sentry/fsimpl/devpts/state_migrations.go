// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package devpts

import (
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/state"
)

// How this package's types load from what builds before TTC's merge of
// upstream gVisor through 2026-10-28 (TTC's dailies through 20261023) saved;
// see pkg/state/migrate.go, and runsc/ttcd/schemas for their shapes.
func init() {
	// Older builds kept the mount options as attributes: the root directory had
	// the mode (0555 when none was given) and the owners, and ptmx the ptmx
	// mode. Owners other than root were given as options.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/sentry/fsimpl/devpts.rootInode",
		Fields: []string{"implStatFS", "InodeAlwaysValid", "InodeAttrs", "InodeDirectoryNoNewChildren", "InodeNotAnonymous", "InodeNotSymlink", "InodeTemporary", "InodeWatches", "OrderedChildren", "InodeFSOwned", "rootInodeRefs", "locks", "master", "replicas", "nextIdx"},
		Set:    []string{"opts"},
		FixAfterLoad: func(obj any, old map[string]any) {
			i := obj.(*rootInode)
			mode := i.InodeAttrs.Mode() & 0777
			if mode == 0555 {
				mode = defaultReplicaMode
			}
			ptmxMode := linux.FileMode(0666)
			if i.master != nil {
				ptmxMode = i.master.InodeAttrs.Mode() & 0777
			}
			uid, gid := i.InodeAttrs.UID(), i.InodeAttrs.GID()
			i.opts = fileSystemOpts{mode: mode, ptmxMode: ptmxMode, uid: uid, gid: gid, setuid: uid != 0, setgid: gid != 0}
		},
	})
}
