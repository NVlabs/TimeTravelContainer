// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvproxy

import (
	"gvisor.dev/gvisor/pkg/state"
)

// How this package's types load from what builds before TTC's merge of
// upstream gVisor through 2026-10-28 (TTC's dailies through 20261023) saved;
// see pkg/state/migrate.go, and runsc/ttcd/schemas for their shapes.
func init() {
	// Older builds offered neither nvidia-uvm-tools nor the MNCC management
	// capability.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/sentry/devices/nvproxy.DeviceInfo",
		Fields: []string{"CapsDevMajor", "HaveFabricIMEXManagement", "FabricIMEXManagementDevMinor", "CapsIMEXChannelsDevMajor", "UVMDevMajor"},
		Zero:   []string{"HaveMNCCManagement", "MNCCManagementDevMinor", "HaveUVMTools"},
	})
}
