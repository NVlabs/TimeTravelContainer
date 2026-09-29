// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package boot

import "golang.org/x/sys/unix"

import "gvisor.dev/gvisor/runsc/flag"

func configureHostTestFlags(*flag.FlagSet) error { return nil }

func createHostEventFDForTest() (int, error) {
	return unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
}
