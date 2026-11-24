// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ipv6

import "gvisor.dev/gvisor/pkg/tcpip/stack"

var _ stack.ReplacedNetworkEndpoint = (*endpoint)(nil)

// ReleaseAddresses implements stack.ReplacedNetworkEndpoint.
func (e *endpoint) ReleaseAddresses() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.mu.addressableEndpointState.Cleanup()
}
