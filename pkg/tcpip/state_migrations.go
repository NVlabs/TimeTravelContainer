// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tcpip

import (
	"gvisor.dev/gvisor/pkg/state"
)

// How this package's types load from what builds before TTC's merge of
// upstream gVisor through 2026-10-28 (TTC's dailies through 20261023) saved;
// see pkg/state/migrate.go, and runsc/ttcd/schemas for their shapes.
func init() {
	// Older builds did not schedule jobs again after a restore, and nor are
	// theirs: a job with no done is never scheduled.
	state.RegisterMigration(state.Migration{
		Type:    "pkg/tcpip.jobInstance",
		Fields:  []string{"earlyReturn"},
		Dropped: []string{"earlyReturn"},
		Zero:    []string{"deadline", "done"},
	})
}
