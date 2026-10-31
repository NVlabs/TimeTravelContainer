// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package fsutil

import (
	"gvisor.dev/gvisor/pkg/state"
)

// How this package's types load from what builds before TTC's merge of
// upstream gVisor through 2026-10-28 (TTC's dailies through 20261023) saved;
// see pkg/state/migrate.go, and runsc/ttcd/schemas for their shapes.
func init() {
	// Not saved since upstream 93a2e47f5: the file is mapped again as it is
	// used.
	state.RegisterMigration(state.Migration{
		Type:    "pkg/sentry/fsutil.MmapPreciseFile",
		Fields:  []string{"NoBufferedIOFallback", "fd", "memType", "addrMustEqualFileOffset", "mappableReleased", "refs", "mappings"},
		Dropped: []string{"mappings"},
	})
}
