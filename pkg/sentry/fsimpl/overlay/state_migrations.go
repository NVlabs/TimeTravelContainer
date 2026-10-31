// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package overlay

import (
	"gvisor.dev/gvisor/pkg/state"
)

// How this package's types load from what builds before TTC's merge of
// upstream gVisor through 2026-10-28 (TTC's dailies through 20261023) saved;
// see pkg/state/migrate.go, and runsc/ttcd/schemas for their shapes.
func init() {
	// Older builds had no ACLs on overlays. Writers open across the upgrade are
	// not counted: a file that they hold open for writing can be executed
	// meanwhile.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/sentry/fsimpl/overlay.dentry",
		Fields: []string{"vfsd", "refs", "fs", "mode", "uid", "gid", "copiedUp", "parent", "name", "children", "dirents", "upperVD", "lowerVDs", "inlineLowerVDs", "devMajor", "devMinor", "ino", "lowerMappings", "wrappedMappable", "isMappable", "locks", "watches", "dirInoHash"},
		Zero:   []string{"accessACL", "defaultACL", "writeCount"},
	})
}
