// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mm

import (
	"gvisor.dev/gvisor/pkg/state"
)

// How this package's types load from what builds before TTC's merge of
// upstream gVisor through 2026-10-28 (TTC's dailies through 20261023) saved;
// see pkg/state/migrate.go, and runsc/ttcd/schemas for their shapes.
func init() {
	// Older builds never set hasPinned, and fork treats their pages as it did
	// until pages are pinned again. kernel.Task's migration sets userNS from the
	// tasks using the MemoryManager.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/sentry/mm.MemoryManager",
		Fields: []string{"layout", "users", "vmas", "brk", "usageAS", "lockedAS", "dataAS", "defMLockMode", "liveForkPMAKey", "pmas", "curRSS", "maxRSS", "dumpability", "argv", "envv", "auxv", "executable", "aioManager", "vdsoSigReturnAddr", "membarrierPrivateEnabled", "membarrierRSeqEnabled"},
		Zero:   []string{"hasPinned"},
		Set:    []string{"userNS"},
	})
	// The usertrap table is sealed and copied eagerly on fork, as it is mapped
	// now.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/sentry/mm.vma",
		Fields: []string{"mappable", "off", "realPerms", "dontfork", "mlockMode", "numaPolicy", "numaNodemask", "id", "name", "nameMut", "lastFault"},
		Set:    []string{"eagerForkCopy", "sealed"},
		Fix: func(obj any, old map[string]any) {
			v := obj.(*vma)
			if v.name == "[usertrap]" {
				v.eagerForkCopy = true
				v.sealed = true
			}
		},
	})
}
