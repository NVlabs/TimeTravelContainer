// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package stateio

import (
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/aio"
	"gvisor.dev/gvisor/pkg/log"
)

func newFDWriterQueue(fd int32, maxParallel int) (aio.Queue, bool) {
	statusFlags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		log.Warningf("stateio.FDWriter: failed to get file status flags for FD %d: %v", fd, err)
		return aio.NewSerialGoQueue(maxParallel), false
	}
	if statusFlags&unix.O_DIRECT == 0 {
		return aio.NewSerialGoQueue(maxParallel), false
	}
	q, err := aio.NewLinuxQueue(maxParallel)
	if err != nil {
		log.Warningf("stateio.FDWriter: failed to create aio.LinuxQueue with capacity %d: %v; falling back to serial aio.GoQueue", maxParallel, err)
		return aio.NewSerialGoQueue(maxParallel), false
	}
	return q, true
}
