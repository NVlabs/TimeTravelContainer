// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"gvisor.dev/gvisor/pkg/state"
)

// How this package's types load from what builds before TTC's merge of
// upstream gVisor through 2026-10-28 (TTC's dailies through 20261023) saved;
// see pkg/state/migrate.go, and runsc/ttcd/schemas for their shapes.
func init() {
	// Upstream 6d32647a9 renamed these types and fields, without changing what
	// they do.
	state.RegisterMigration(state.Migration{
		Type:    "pkg/sentry/socket/unix/transport.queueSender",
		OldName: "pkg/sentry/socket/unix/transport.connectedEndpoint",
		Fields:  []string{"endpoint", "writeQueue"},
	})
	// As queueSender.
	state.RegisterMigration(state.Migration{
		Type:    "pkg/sentry/socket/unix/transport.HostSenderRefs",
		OldName: "pkg/sentry/socket/unix/transport.HostConnectedEndpointRefs",
		Fields:  []string{"refCount"},
	})
	// As queueSender.
	state.RegisterMigration(state.Migration{
		Type:    "pkg/sentry/socket/unix/transport.HostSender",
		OldName: "pkg/sentry/socket/unix/transport.HostConnectedEndpoint",
		Fields:  []string{"HostConnectedEndpointRefs", "fd", "addr", "stype", "rdShutdown", "wrShutdown", "passcred"},
		Renamed: map[string]string{"HostConnectedEndpointRefs": "HostSenderRefs", "rdShutdown": "recvShutdown", "wrShutdown": "sendShutdown"},
	})
	// As queueSender.
	state.RegisterMigration(state.Migration{
		Type:    "pkg/sentry/socket/unix/transport.SCMSender",
		OldName: "pkg/sentry/socket/unix/transport.SCMConnectedEndpoint",
		Fields:  []string{"HostConnectedEndpoint", "queue"},
		Renamed: map[string]string{"HostConnectedEndpoint": "HostSender"},
	})
	// As queueSender.
	state.RegisterMigration(state.Migration{
		Type:    "pkg/sentry/socket/unix/transport.baseEndpoint",
		Fields:  []string{"Queue", "DefaultSocketOptionsHandler", "receiver", "connected", "path", "writeShutdown", "ops", "lastError"},
		Renamed: map[string]string{"Queue": "WaitQueue", "connected": "peer", "writeShutdown": "sendShutdown"},
	})
	// As queueSender.
	state.RegisterMigration(state.Migration{
		Type:    "pkg/sentry/socket/unix/transport.queue",
		Fields:  []string{"queueRefs", "ReaderQueue", "WriterQueue", "closed", "unread", "used", "limit", "dataList"},
		Renamed: map[string]string{"ReaderQueue": "readerWaiters", "WriterQueue": "writerWaiters"},
	})
}
