// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package stopfd

import (
	"gvisor.dev/gvisor/pkg/state"
)

// How this package's types load from what builds before TTC's merge of
// upstream gVisor through 2026-10-28 (TTC's dailies through 20261023) saved;
// see pkg/state/migrate.go, and runsc/ttcd/schemas for their shapes.
func init() {
	// The eventfd is not saved now.
	state.RegisterMigration(state.Migration{
		Type:    "pkg/tcpip/link/stopfd.StopFD",
		Fields:  []string{"EFD"},
		Dropped: []string{"EFD"},
	})
}
