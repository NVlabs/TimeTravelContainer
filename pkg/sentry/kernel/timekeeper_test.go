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
	"bytes"
	"context"
	"os"
	"testing"

	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/memutil"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	sentrytime "gvisor.dev/gvisor/pkg/sentry/time"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/state"
)

// mockClocks is a sentrytime.Clocks that simply returns the times in the
// struct.
type mockClocks struct {
	monotonic int64
	realtime  int64
}

// Update implements sentrytime.Clocks.Update. It does nothing.
func (*mockClocks) Update(parked bool) (monotonicParams sentrytime.Parameters, monotonicOk bool, realtimeParam sentrytime.Parameters, realtimeOk bool) {
	return
}

// GetTime implements sentrytime.Clocks.GetTime.
func (c *mockClocks) GetTime(id sentrytime.ClockID) (int64, error) {
	switch id {
	case sentrytime.Monotonic:
		return c.monotonic, nil
	case sentrytime.Realtime:
		return c.realtime, nil
	default:
		return 0, linuxerr.EINVAL
	}
}

// stateTestClocklessTimekeeper returns a test Timekeeper which has not had
// SetClocks called.
func stateTestClocklessTimekeeper(tb testing.TB) (*Timekeeper, *VDSOParamPage) {
	const memfileName = "timekeeper-test-memory"
	memfd, err := memutil.CreateMemFD(memfileName, 0)
	if err != nil {
		tb.Fatalf("creating application memory file: %v", err)
	}
	mf, err := pgalloc.NewMemoryFile(os.NewFile(uintptr(memfd), memfileName), pgalloc.MemoryFileOpts{
		DisableMemoryAccounting: true,
	})
	if err != nil {
		tb.Fatalf("initializing application memory file: %v", err)
	}
	fr, err := mf.Allocate(hostarch.PageSize, pgalloc.AllocOpts{Kind: usage.Anonymous})
	if err != nil {
		tb.Fatalf("failed to allocate memory: %v", err)
	}
	params := NewVDSOParamPage(mf, fr)
	return &Timekeeper{}, params
}

func stateTestTimekeeper(tb testing.TB) *Timekeeper {
	t, params := stateTestClocklessTimekeeper(tb)
	t.SetClocks(sentrytime.NewCalibratedClocks(), params)
	return t
}

func TestTimekeeperLoadClearsExecutionState(t *testing.T) {
	source := stateTestTimekeeper(t)
	source.PauseUpdates()
	defer source.Destroy()
	var image bytes.Buffer
	if _, err := state.Save(context.Background(), &image, source); err != nil {
		t.Fatalf("saving Timekeeper: %v", err)
	}

	target, params := stateTestClocklessTimekeeper(t)
	target.SetClocks(sentrytime.NewCalibratedClocks(), params)
	target.PauseUpdates()
	if _, err := state.Load(context.Background(), bytes.NewReader(image.Bytes()), target); err != nil {
		t.Fatalf("loading Timekeeper over initialized target: %v", err)
	}
	if target.clocks != nil {
		t.Fatal("restored Timekeeper retained its previous clock source")
	}
	if target.stop != nil || target.timer != nil || target.params != nil {
		t.Fatal("restored Timekeeper retained updater state")
	}
	target.SetClocks(sentrytime.NewCalibratedClocks(), params)
	target.Destroy()
}

func TestTimekeeperClockStateCanonicalizesKernel(t *testing.T) {
	canonical := &Timekeeper{}
	foreign := stateTestTimekeeper(t)
	defer foreign.Destroy()
	k := &Kernel{timekeeper: canonical}
	ctx := context.WithValue(context.Background(), CtxKernel, k)

	source := &timekeeperClock{tk: foreign, c: sentrytime.Monotonic}
	var image bytes.Buffer
	if _, err := state.Save(ctx, &image, source); err != nil {
		t.Fatalf("saving clock with active foreign Timekeeper: %v", err)
	}
	var restored timekeeperClock
	if _, err := state.Load(ctx, bytes.NewReader(image.Bytes()), &restored); err != nil {
		t.Fatalf("loading clock: %v", err)
	}
	if restored.tk != canonical {
		t.Fatalf("restored clock Timekeeper=%p, want current Kernel Timekeeper %p", restored.tk, canonical)
	}
	if restored.c != sentrytime.Monotonic {
		t.Fatalf("restored clock ID=%v, want monotonic", restored.c)
	}
}

func TestTimekeeperTCPIPClockStateCanonicalizesKernel(t *testing.T) {
	canonical := &Timekeeper{}
	foreign := stateTestTimekeeper(t)
	defer foreign.Destroy()
	k := &Kernel{timekeeper: canonical}
	ctx := context.WithValue(context.Background(), CtxKernel, k)

	source := &timekeeperTCPIPClock{tk: foreign}
	var image bytes.Buffer
	if _, err := state.Save(ctx, &image, source); err != nil {
		t.Fatalf("saving network clock with active foreign Timekeeper: %v", err)
	}
	var restored timekeeperTCPIPClock
	if _, err := state.Load(ctx, bytes.NewReader(image.Bytes()), &restored); err != nil {
		t.Fatalf("loading network clock: %v", err)
	}
	if restored.tk != canonical {
		t.Fatalf("restored network clock Timekeeper=%p, want current Kernel Timekeeper %p", restored.tk, canonical)
	}
}

// TestTimekeeperMonotonicZero tests that monotonic time starts at zero.
func TestTimekeeperMonotonicZero(t *testing.T) {
	c := &mockClocks{
		monotonic: 100000,
	}

	tk, params := stateTestClocklessTimekeeper(t)
	tk.SetClocks(c, params)
	defer tk.Destroy()

	now, err := tk.GetTime(sentrytime.Monotonic)
	if err != nil {
		t.Errorf("GetTime err got %v want nil", err)
	}
	if now != 0 {
		t.Errorf("GetTime got %d want 0", now)
	}

	c.monotonic += 10

	now, err = tk.GetTime(sentrytime.Monotonic)
	if err != nil {
		t.Errorf("GetTime err got %v want nil", err)
	}
	if now != 10 {
		t.Errorf("GetTime got %d want 10", now)
	}
}

// TestTimekeeperMonotonicJumpForward tests that monotonic time jumps forward
// after restore.
func TestTimekeeperMonotonicForward(t *testing.T) {
	c := &mockClocks{
		monotonic: 900000,
		realtime:  600000,
	}

	tk, params := stateTestClocklessTimekeeper(t)
	tk.restored = make(chan struct{})
	tk.saveMonotonic = 100000
	tk.saveRealtime = 400000
	tk.SetClocks(c, params)
	defer tk.Destroy()

	// The monotonic clock should jump ahead by 200000 to 300000.
	//
	// The new system monotonic time (900000) is irrelevant to what the app
	// sees.
	now, err := tk.GetTime(sentrytime.Monotonic)
	if err != nil {
		t.Errorf("GetTime err got %v want nil", err)
	}
	if now != 300000 {
		t.Errorf("GetTime got %d want 300000", now)
	}
}

// TestTimekeeperMonotonicJumpBackwards tests that monotonic time does not jump
// backwards when realtime goes backwards.
func TestTimekeeperMonotonicJumpBackwards(t *testing.T) {
	c := &mockClocks{
		monotonic: 900000,
		realtime:  400000,
	}

	tk, params := stateTestClocklessTimekeeper(t)
	tk.restored = make(chan struct{})
	tk.saveMonotonic = 100000
	tk.saveRealtime = 600000
	tk.SetClocks(c, params)
	defer tk.Destroy()

	// The monotonic clock should remain at 100000.
	//
	// The new system monotonic time (900000) is irrelevant to what the app
	// sees and we don't want to jump the monotonic clock backwards like
	// realtime did.
	now, err := tk.GetTime(sentrytime.Monotonic)
	if err != nil {
		t.Errorf("GetTime err got %v want nil", err)
	}
	if now != 100000 {
		t.Errorf("GetTime got %d want 100000", now)
	}
}
