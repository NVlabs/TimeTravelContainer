// Copyright 2018 The gVisor Authors.
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

package kernel

import (
	"context"

	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/sentry/ktime"
	"gvisor.dev/gvisor/pkg/sentry/time"
	"gvisor.dev/gvisor/pkg/sync"
)

// beforeSave is invoked by stateify.
func (t *Timekeeper) beforeSave() {
	if t.stop != nil {
		panic("pauseUpdates must be called before Save")
	}

	if t.clocks == nil {
		t.restored = nil
		return
	}

	// N.B. we want the *offset* monotonic time.
	var err error
	if t.saveMonotonic, err = t.GetTime(time.Monotonic); err != nil {
		panic("unable to get current monotonic time: " + err.Error())
	}

	if t.saveRealtime, err = t.GetTime(time.Realtime); err != nil {
		panic("unable to get current realtime: " + err.Error())
	}
}

// afterLoad is invoked by stateify.
func (t *Timekeeper) afterLoad(context.Context) {
	// StateLoad is allowed to decode into an existing root object. More
	// importantly, execution-only fields must never survive a second state
	// cycle (for example dehydrate -> rehydrate -> live fork). Reconstruct the
	// complete unsaved portion of Timekeeper here instead of relying on the
	// decoder having allocated zeroed storage.
	t.clocks = nil
	t.monotonicOffset = 0
	t.monotonicLowerBound = atomicbitops.Int64{}
	t.mu = sync.Mutex{}
	t.stop = nil
	t.wg = sync.WaitGroup{}
	t.updaterState = atomicbitops.Int32{}
	t.refs = atomicbitops.Int64{}
	t.updateMu = sync.Mutex{}
	t.timer = nil
	t.params = nil
	t.restored = make(chan struct{})
	t.pausableClock = &ktime.SyntheticClock{}
	t.afterFuncSchedulerWakeCh = make(chan struct{}, 1)
}
