// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package fdchannel

import "golang.org/x/sys/unix"

func (*Endpoint) initIov() {}

func sendFDFlags() uintptr { return 0 }

func newConnectedSockets() ([2]int, error) {
	return unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
}
