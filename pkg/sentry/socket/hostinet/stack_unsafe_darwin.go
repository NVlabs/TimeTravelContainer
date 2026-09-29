// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package hostinet

import (
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/sentry/inet"
)

func queryInterfaceFeatures(interfaces map[int32]inet.Interface) error {
	return unix.EOPNOTSUPP
}
