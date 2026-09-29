// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package boot

import "net"

func timeTravelHostLoopbackAddress(last byte) net.IP {
	return net.IPv4(127, 0, 0, last)
}
