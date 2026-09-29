// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package boot

import "net"

// Darwin normally configures only 127.0.0.1 on lo0, unlike Linux where the
// complete 127/8 block is locally routed. Preserve TTC's host-alias facility
// by collapsing aliases onto the configured loopback address.
func timeTravelHostLoopbackAddress(byte) net.IP {
	return net.IPv4(127, 0, 0, 1)
}
