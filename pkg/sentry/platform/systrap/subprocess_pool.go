// Copyright 2023 The gVisor Authors.
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

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package systrap

import (
	"sync"
)

// subprocessPool exists to solve these distinct problems:
//
// 1) Subprocesses can't always be killed properly (see subprocess.Release).
// In general it's helpful to be able to reuse subprocesses, but we must observe
// the subprocess lifecycle before we can do so (e.g. should wait for all
// contexts to be released).
//
// 2) Any seccomp filters that have been installed will apply to subprocesses
// created here. Therefore we use the intermediary (source), which is created
// on initialization of the platform.
type subprocessPool struct {
	mu     sync.Mutex
	source *subprocess
	// available stores all subprocesses that are available for reuse.
	// +checklocks:mu
	available []*subprocess
}

// maxAvailableSubprocesses is the most subprocesses that the pool keeps for
// reuse. Each is a host process, with memory and mappings of the Sentry's, and
// an application that once ran thousands of processes at a time otherwise left
// as many for good.
const maxAvailableSubprocesses = 512

// markAvailable makes s available for reuse, unless the pool is full. It
// returns whether it did.
func (p *subprocessPool) markAvailable(s *subprocess) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.available) >= maxAvailableSubprocesses {
		return false
	}
	p.available = append(p.available, s)
	return true
}

// shrink destroys up to n of the subprocesses that the pool has kept longest,
// and returns how many it destroyed: each holds a host file descriptor, its
// syscall thread's seccomp notifications, which the Sentry needs when it nears
// its limit on them (see fdbudget.AddShrinker).
func (p *subprocessPool) shrink(n int) int {
	if !p.mu.TryLock() {
		return 0
	}
	n = min(n, len(p.available))
	victims := make([]*subprocess, n)
	copy(victims, p.available)
	kept := copy(p.available, p.available[n:])
	clear(p.available[kept:])
	p.available = p.available[:kept]
	p.mu.Unlock()
	for _, s := range victims {
		s.destroy()
		// The reaper closes it once it has reaped the stub; the budget
		// needs it now, and the killed stub has no use for it.
		if t := s.syscallThread; t != nil && t.seccompNotify != nil {
			t.seccompNotify.Close()
		}
	}
	return n
}

func (p *subprocessPool) fetchAvailable() *subprocess {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.available) > 0 {
		s := p.available[len(p.available)-1]
		p.available = p.available[:len(p.available)-1]

		return s
	}
	return nil
}
