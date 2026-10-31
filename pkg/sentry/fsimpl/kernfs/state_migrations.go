// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kernfs

import (
	"gvisor.dev/gvisor/pkg/state"
)

// How this package's types load from what builds before TTC's merge of
// upstream gVisor through 2026-10-28 (TTC's dailies through 20261023) saved;
// see pkg/state/migrate.go, and runsc/ttcd/schemas for their shapes.
func init() {
	// A procfs mounted before the upgrade keeps the older behavior of failed
	// lookups as files are created.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/sentry/fsimpl/kernfs.Filesystem",
		Fields: []string{"vfsfs", "deferredDecRefs", "nextInoMinusOne", "cachedDentries", "cachedDentriesLen", "MaxCachedDentries", "root"},
		Zero:   []string{"NoNegativeDentries"},
	})
}
