// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tmpfs

import (
	"gvisor.dev/gvisor/pkg/state"
)

// How this package's types load from what builds before TTC's merge of
// upstream gVisor through 2026-10-28 (TTC's dailies through 20261023) saved;
// see pkg/state/migrate.go, and runsc/ttcd/schemas for their shapes.
func init() {
	// Older builds had no huge mount option.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/sentry/fsimpl/tmpfs.filesystem",
		Fields: []string{"vfsfs", "mf", "clock", "devMinor", "mopts", "usage", "nextInoMinusOne", "root", "maxFilenameLen", "maxSizeInPages", "maxInodes", "pagesUsed", "diskBytes", "inodesUsed", "allowXattrPrefix", "ovlWhiteout", "diskVolume"},
		Zero:   []string{"huge"},
	})
	// Writers open across the upgrade are not counted: a file that they hold
	// open for writing can be executed meanwhile.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/sentry/fsimpl/tmpfs.inode",
		Fields: []string{"fs", "refs", "xattrs", "mode", "accessACL", "defaultACL", "nlink", "uid", "gid", "ino", "atime", "btime", "ctime", "mtime", "locks", "watches", "impl"},
		Zero:   []string{"writeCount"},
	})
}
