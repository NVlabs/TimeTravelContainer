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
	"time"
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

// Darwin has no supported process-shared futex API. The state word is already
// in coherent MAP_SHARED memory, so v1 uses bounded polling. This preserves
// protocol correctness and process topology while leaving an efficient Mach
// semaphore/ulock transport as a later performance optimization.
func (ep *Endpoint) futexWakePeer() error {
	return nil
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

func (ep *Endpoint) futexWakeConnState(_ int32) error {
	return nil
}

func (ep *Endpoint) futexWaitConnState(curState uint32) error {
	for i := 0; i < 64; i++ {
		if ep.connState().Load() != curState || ep.isShutdownLocally() {
			return nil
		}
		runtime.Gosched()
	}
	time.Sleep(50 * time.Microsecond)
	return nil
}

func yieldThread() {
	runtime.Gosched()
}
