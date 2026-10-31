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
	// Older builds read only EROFS inodes whose blocks are laid out flat, whose
	// chunk format is zero.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/erofs.Inode",
		Fields: []string{"image", "dataOff", "idataOff", "blocks", "format", "mode", "nid", "size", "mtime", "mtimeNsec", "uid", "gid", "nlink"},
		Zero:   []string{"chunkFormat"},
	})
}
