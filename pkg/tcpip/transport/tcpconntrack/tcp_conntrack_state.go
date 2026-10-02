// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tcpconntrack

import (
	"context"
)

// afterLoad is invoked by stateify. A TCB's state handlers are functions,
// which are not saved: a restored TCB that tracks a connection, as a live
// fork's child's iptables do, reinstalls them, so that the connection's next
// segment does not call a nil handler.
func (t *TCB) afterLoad(context.Context) {
	if !t.IsEmpty() {
		t.setHandlers(t.synSent)
	}
}
