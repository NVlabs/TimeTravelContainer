// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

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
