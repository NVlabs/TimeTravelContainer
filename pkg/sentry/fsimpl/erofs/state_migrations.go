// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package erofs

import (
	"gvisor.dev/gvisor/pkg/state"
)

// How this package's types load from what builds before TTC's merge of
// upstream gVisor through 2026-10-28 (TTC's dailies through 20261023) saved;
// see pkg/state/migrate.go, and runsc/ttcd/schemas for their shapes.
func init() {
	// TTC never mounts EROFS images, so its images hold none of these.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/sentry/fsimpl/erofs.dentry",
		Fields: []string{"vfsd", "dentryRefs", "parent", "name", "inode", "childMap"},
		Refuse: "TTC mounts no EROFS image",
	})
	// As for dentry.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/sentry/fsimpl/erofs.filesystem",
		Fields: []string{"vfsfs", "mopts", "iopts", "devMinor", "root", "image", "mf", "inodeBuckets"},
		Refuse: "TTC mounts no EROFS image",
	})
	// As for dentry.
	state.RegisterMigration(state.Migration{
		OldName: "pkg/sentry/fsimpl/erofs.dentryRefs",
		Refuse:  "TTC mounts no EROFS image",
	})
}
