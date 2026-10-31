// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mq

import (
	"gvisor.dev/gvisor/pkg/state"
)

// How this package's types load from what builds before TTC's merge of
// upstream gVisor through 2026-10-28 (TTC's dailies through 20261023) saved;
// see pkg/state/migrate.go, and runsc/ttcd/schemas for their shapes.
func init() {
	// Duplicates of the queue inode's owner and mode, which access checks use
	// now.
	state.RegisterMigration(state.Migration{
		Type:    "pkg/sentry/kernel/mq.Queue",
		Fields:  []string{"ownerUID", "ownerGID", "mode", "queue", "messages", "subscriber", "messageCount", "maxMessageCount", "maxMessageSize", "byteCount"},
		Dropped: []string{"ownerUID", "ownerGID", "mode"},
	})
	// Older builds did not implement mq_notify: no queue of theirs has a
	// subscriber to notify.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/sentry/kernel/mq.Subscriber",
		Fields: []string{"pid"},
		Zero:   []string{"target", "userNS", "method", "signal", "value"},
	})
}
