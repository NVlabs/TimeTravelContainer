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

package hosttid

import (
	"os"
	"runtime"
	"testing"

	"gvisor.dev/gvisor/pkg/sync"
)

// runOnMain is used to send functions to run on the main (initial) thread.
var runOnMain = make(chan func(), 10)

func checkProcid(t *testing.T, start *sync.WaitGroup, done *sync.WaitGroup) {
	defer done.Done()

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	start.Done()
	start.Wait()

	procID := Current()
	tid, exact := nativeTIDForTest()
	if exact && procID != tid {
		t.Logf("Bad procid: expected %v, got %v", tid, procID)
		t.Fail()
	} else if procID == 0 {
		t.Error("Bad procid: got zero")
	}
}

func TestRuntimePointers(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	g, m, p, mcache, g0, g0StackLo, g0StackHi, gsignal, gsignalStackLo, gsignalStackHi := RuntimePointers()
	for name, pointer := range map[string]uintptr{
		"g": g, "m": m, "p": p, "mcache": mcache, "g0": g0,
	} {
		if pointer == 0 {
			t.Errorf("RuntimePointers returned a nil %s", name)
		}
	}
	if g0StackLo == 0 || g0StackLo >= g0StackHi {
		t.Errorf("RuntimePointers returned invalid g0 stack [%#x, %#x)", g0StackLo, g0StackHi)
	}
	if gsignal != 0 && (gsignalStackLo == 0 || gsignalStackLo >= gsignalStackHi) {
		t.Errorf("RuntimePointers returned invalid gsignal stack [%#x, %#x)", gsignalStackLo, gsignalStackHi)
	}
}

func TestProcidInitialized(t *testing.T) {
	var start sync.WaitGroup
	var done sync.WaitGroup

	count := 100
	start.Add(count + 1)
	done.Add(count + 1)

	// Run the check on the main thread.
	//
	// When cgo is not included, the only case when procid isn't initialized
	// is in the main (initial) thread, so we have to test this case
	// specifically.
	runOnMain <- func() {
		checkProcid(t, &start, &done)
	}

	// Run the check on a number of different threads.
	for i := 0; i < count; i++ {
		go checkProcid(t, &start, &done)
	}

	done.Wait()
}

func TestMain(m *testing.M) {
	// Make sure we remain at the main (initial) thread.
	runtime.LockOSThread()

	// Start running tests in a different goroutine.
	go func() {
		os.Exit(m.Run())
	}()

	// Execute any functions that have been sent for execution in the main
	// thread.
	for f := range runOnMain {
		f()
	}
}
