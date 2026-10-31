// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tcp

import (
	"gvisor.dev/gvisor/pkg/state"
)

// How this package's types load from what builds before TTC's merge of
// upstream gVisor through 2026-10-28 (TTC's dailies through 20261023) saved;
// see pkg/state/migrate.go, and runsc/ttcd/schemas for their shapes.
func init() {
	// No congestion control was chosen explicitly that accepted endpoints must
	// inherit.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/tcpip/transport/tcp.Endpoint",
		Fields: []string{"TCPEndpointStateInner", "liveForkExternal", "TransportEndpointInfo", "DefaultSocketOptionsHandler", "stack", "protocol", "waiterQueue", "hardError", "lastError", "TCPRcvBufState", "rcvMemUsed", "ownedByUser", "rcvQueue", "state", "connectionDirectionState", "isPortReserved", "isRegistered", "boundNICID", "ipv4TTL", "ipv6HopLimit", "isConnectNotified", "h", "portFlags", "boundBindToDevice", "boundPortFlags", "boundDest", "effectiveNetProtos", "recentTSTime", "shutdownFlags", "tcpRecovery", "sack", "delay", "scoreboard", "segmentQueue", "userMSS", "maxSynRetries", "windowClamp", "sndQueueInfo", "cc", "keepalive", "userTimeout", "deferAccept", "acceptQueue", "rcv", "snd", "connectingAddress", "amss", "sendTOS", "gso", "stats", "tcpLingerTimeout", "closed", "txHash", "owner", "ops", "lastOutOfWindowAckTime", "pmtud", "alsoBindToV4", "terminateAtRestore"},
		Zero:   []string{"ccSetBySockOpt"},
	})
	// As older builds did, the epoch starts at no window.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/tcpip/transport/tcp.cubicState",
		Fields: []string{"TCPCubicState", "numCongestionEvents", "s"},
		Zero:   []string{"cwndEpoch"},
	})
	// No small segment is held back for an ACK.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/tcpip/transport/tcp.sender",
		Fields: []string{"TCPSenderState", "ep", "finSent", "lr", "firstRetransmittedSegXmitTime", "writeNext", "writeList", "rtt", "minRTO", "maxRTO", "maxRetries", "gso", "state", "cc", "rc", "spuriousRecovery", "retransmitTS", "startCork"},
		Zero:   []string{"smallSegmentEnd"},
	})
}
