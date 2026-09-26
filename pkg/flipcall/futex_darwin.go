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

//go:build darwin

package flipcall

import (
	"fmt"
	"runtime"
	"syscall"
)

const (
	ulCompareAndWaitShared = 3
	ulWakeAll              = 0x00000100
	sysUlockWait           = 515
	sysUlockWake           = 516
)

func (ep *Endpoint) futexSetPeerActive() error {
	if ep.connState().CompareAndSwap(ep.activeState, ep.inactiveState) {
		return nil
	}
	switch cs := ep.connState().Load(); cs {
	case csShutdown:
		return ShutdownError{}
	default:
		return fmt.Errorf("unexpected connection state before peer wake: %v", cs)
	}
}

func (ep *Endpoint) futexWakePeer() error {
	return ep.futexWakeConnState(1)
}

func (ep *Endpoint) futexWaitUntilActive() error {
	for {
		switch cs := ep.connState().Load(); cs {
		case ep.activeState:
			return nil
		case ep.inactiveState:
			if ep.isShutdownLocally() {
				return ShutdownError{}
			}
			if err := ep.futexWaitConnState(ep.inactiveState); err != nil {
				return err
			}
		case csShutdown:
			return ShutdownError{}
		default:
			return fmt.Errorf("unexpected connection state while waiting for peer: %v", cs)
		}
	}
}

func (ep *Endpoint) futexWakeConnState(numThreads int32) error {
	operation := uintptr(ulCompareAndWaitShared)
	if numThreads != 1 {
		operation |= ulWakeAll
	}
	_, _, errno := syscall.Syscall6(sysUlockWake, operation, ep.packet, 0, 0, 0, 0)
	// It is valid for the peer to observe the changed state before it enters
	// the kernel, in which case there is no waiter to wake.
	if errno == 0 || errno == syscall.ENOENT {
		return nil
	}
	return fmt.Errorf("__ulock_wake: %w", errno)
}

func (ep *Endpoint) futexWaitConnState(curState uint32) error {
	_, _, errno := syscall.Syscall6(
		sysUlockWait,
		ulCompareAndWaitShared,
		ep.packet,
		uintptr(curState),
		0, // No timeout.
		0,
		0,
	)
	// EAGAIN means the peer changed the state before the kernel committed the
	// wait. EINTR is retried by futexWaitUntilActive after it rechecks state.
	if errno == 0 || errno == syscall.EAGAIN || errno == syscall.EINTR {
		return nil
	}
	return fmt.Errorf("__ulock_wait: %w", errno)
}

func yieldThread() {
	runtime.Gosched()
}
