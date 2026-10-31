// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package stack

import (
	"gvisor.dev/gvisor/pkg/state"
)

// How this package's types load from what builds before TTC's merge of
// upstream gVisor through 2026-10-28 (TTC's dailies through 20261023) saved;
// see pkg/state/migrate.go, and runsc/ttcd/schemas for their shapes.
func init() {
	// Older builds did not save addresses: TTC adds every NIC and its addresses
	// again as it restores a branch's network.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/tcpip/stack.AddressableEndpointState",
		Fields: []string{"networkEndpoint", "options"},
		Zero:   []string{"endpoints", "primary"},
	})
	// The default range.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/tcpip/stack.MasqueradeTarget",
		Fields: []string{"NetworkProtocol"},
		Zero:   []string{"Ports"},
	})
	// No preserved NICs; RACK accounts for no clock resolution, as older builds
	// did.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/tcpip/stack.Stack",
		Fields: []string{"transportProtocols", "networkProtocols", "rawFactory", "packetEndpointWriteSupported", "demux", "stats", "defaultForwardingEnabled", "savedConfig", "cleanupEndpoints", "PortManager", "clock", "handleLocal", "nftablesConfigured", "restoredEndpoints", "resumableEndpoints", "icmpRateLimiter", "seed", "nudConfigs", "nudDisp", "sendBufferSize", "receiveBufferSize", "tcpInvalidRateLimit", "tsOffsetSecret", "externalNetworkingDisabled", "allowConnectedOnSave"},
		Zero:   []string{"preservedNICs", "preservedRoutes", "clockResolution"},
	})
	// As older builds did: probes from the unspecified address, and no packets
	// queued.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/tcpip/stack.neighborEntryMu",
		Fields: []string{"neigh", "isRouter", "timer"},
		Zero:   []string{"localAddr", "pendingPackets"},
	})
	// Zero bounds are unrestricted.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/tcpip/stack.nic",
		Fields: []string{"NetworkLinkEndpoint", "stack", "id", "name", "kind", "context", "stats", "networkEndpoints", "linkAddrResolvers", "duplicateAddressDetectors", "enabled", "spoofing", "promiscuous", "linkResQueue", "packetEPs", "qDisc", "deliverLinkPackets", "Primary", "experimentIPOptionEnabled"},
		Zero:   []string{"minMTU", "maxMTU"},
	})
}
