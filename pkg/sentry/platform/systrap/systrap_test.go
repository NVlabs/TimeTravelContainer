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

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package systrap

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	pkgcontext "gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/cpuid"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/platform/systrap/sysmsg"
	"gvisor.dev/gvisor/pkg/sentry/platform/systrap/testutil"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/usermem"
)

const childEnv = "SYSTRAP_TEST_CHILD"

func TestMain(m *testing.M) {
	switch mode := os.Getenv(childEnv); mode {
	case "":
		cpuid.Initialize()
		os.Exit(m.Run())
	case "stuck":
		for {
			unix.Pause()
		}
	case "hoststall":
		if err := hostStallChild(); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown %s: %q\n", childEnv, mode)
		os.Exit(1)
	}
}

// testMemoryManager implements the parts of platform.MemoryManager that
// platformContext.Switch touches on the syscall path. Syscall patching is
// disabled in the tests, so MMap and friends are never reached.
type testMemoryManager struct {
	usermem.IO // Never used.
	as         platform.AddressSpace
}

func (*testMemoryManager) MMap(pkgcontext.Context, memmap.MMapOpts) (hostarch.Addr, error) {
	panic("unexpected MMap call")
}

func (mm *testMemoryManager) AddressSpace() platform.AddressSpace { return mm.as }

func (*testMemoryManager) FindVMAByName(hostarch.AddrRange, string) (hostarch.Addr, uint64, error) {
	panic("unexpected FindVMAByName call")
}

const userCodeAddr = hostarch.Addr(0x100000)

// newSystrapTest creates a real Systrap instance, maps userCode into a new
// address space, and returns a context ready to Switch into it.
func newSystrapTest(t *testing.T) (*Systrap, platform.AddressSpace, *platformContext, *arch.Context64) {
	t.Helper()

	s, err := New(platform.Options{
		DisableSyscallPatching: true,
		// Only slow-path for now in order to not have to use a real
		// memory manager.
		DisableFastPath: true,
	})
	if err != nil {
		t.Fatalf("failed to create Systrap platform: %v", err)
	}

	as, err := s.NewAddressSpace()
	if err != nil {
		t.Fatalf("failed to create address space: %v", err)
	}
	t.Cleanup(as.Release)

	fr, err := s.memoryFile.Allocate(hostarch.PageSize, pgalloc.AllocOpts{Kind: usage.System})
	if err != nil {
		t.Fatalf("failed to allocate user code page: %v", err)
	}
	bs, err := s.memoryFile.MapInternal(fr, hostarch.Write)
	if err != nil {
		t.Fatalf("failed to map user code page: %v", err)
	}
	if _, err := safemem.CopySeq(bs, safemem.BlockSeqOf(safemem.BlockFromSafeSlice(testutil.UserCode))); err != nil {
		t.Fatalf("failed to write user code: %v", err)
	}
	if err := as.MapFile(userCodeAddr, s.memoryFile, fr, hostarch.AccessType{Read: true, Execute: true}, false); err != nil {
		t.Fatalf("failed to map user code into the stub address space: %v", err)
	}

	pctx := s.NewContext(pkgcontext.Background()).(*platformContext)
	t.Cleanup(pctx.Release)

	ac := arch.New(arch.Host)
	ac.SetIP(uintptr(userCodeAddr))
	return s, as, pctx, ac
}

// TestSwitchThreadIDVisibility runs a real Systrap instance and verifies
// that there is no window during which the ContextState indicates it's being
// worked on AND there is no thread currently working on it.
func TestSwitchThreadIDVisibility(t *testing.T) {
	_, as, pctx, ac := newSystrapTest(t)
	mm := &testMemoryManager{as: as}
	ctx := pkgcontext.Background()
	violations := atomic.Uint64{}
	stop := atomic.Bool{}
	samplerDone := make(chan struct{})
	startSampler := func(shared *sysmsg.ThreadContext) {
		go func() {
			defer close(samplerDone)
			for !stop.Load() {
				a1 := atomic.LoadUint64(&shared.AckedTime)

				// The stub ThreadID must always be set first,
				// and cleared last.
				tid := atomic.LoadUint32(&shared.ThreadID)
				state := shared.State.Get()

				a2 := atomic.LoadUint64(&shared.AckedTime)
				if a1 != 0 && a1 == a2 && state == sysmsg.ContextStateNone && tid == invalidThreadID {
					violations.Add(1)
				}
			}
		}()
	}

	for i := 0; i < 4000; i++ {
		si, _, err := pctx.Switch(ctx, mm, ac, 0)
		if err != nil {
			t.Fatalf("Switch failed at iteration %d: %v (si: %v)", i, err, si)
		}
		if sysno := ac.SyscallNo(); sysno != unix.SYS_GETPID {
			t.Fatalf("unexpected syscall %d at iteration %d", sysno, i)
		}
		ac.SetReturn(0) // Emulate the syscall return value.

		if i == 0 {
			startSampler(pctx.sharedContext.Load().shared)
		}
	}

	stop.Store(true)
	<-samplerDone
	if n := violations.Load(); n != 0 {
		t.Errorf("sampler observed %d acked contexts with an invalid ThreadID; "+
			"the stub published acked_time before ThreadID or cleared ThreadID "+
			"before publishing the context state", n)
	}
}

func TestSpinningQueueLayout(t *testing.T) {
	newSystrapTest(t)
	queueStart := stubContextQueueRegion + stubContextQueueRegionLen
	if stubSpinningThreadQueueAddr < queueStart || stubSpinningThreadQueueAddr+sysmsg.SpinningQueueMemSize > stubContextRegion {
		t.Errorf("spinning queue %#x not in [%#x, %#x)", stubSpinningThreadQueueAddr, queueStart, stubContextRegion)
	}
}

// children returns the number of this process's children, and how many of
// them are zombies.
func children(t *testing.T) (int, int) {
	t.Helper()
	stats, err := filepath.Glob("/proc/[0-9]*/stat")
	if err != nil {
		t.Fatal(err)
	}
	self := strconv.Itoa(os.Getpid())
	var n, zombies int
	for _, path := range stats {
		data, err := os.ReadFile(path)
		if err != nil {
			continue // Exited meanwhile.
		}
		// The fields after the parenthesized command: state, then ppid.
		fields := strings.Fields(string(data[bytes.LastIndexByte(data, ')')+1:]))
		if len(fields) < 2 || fields[1] != self {
			continue
		}
		n++
		if fields[0] == "Z" {
			zombies++
		}
	}
	return n, zombies
}

// mappedBytes returns the size of this process's memory mappings.
func mappedBytes(t *testing.T) uint64 {
	t.Helper()
	data, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		t.Fatal(err)
	}
	var total uint64
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var start, end uint64
		if _, err := fmt.Sscanf(line, "%x-%x", &start, &end); err != nil {
			t.Fatalf("parsing %q: %v", line, err)
		}
		total += end - start
	}
	return total
}

// The pool keeps a bounded number of released subprocesses: the stubs of the
// others are killed and reaped, and the Sentry's mappings of their memory go.
func TestPoolDestroysExcessSubprocesses(t *testing.T) {
	s, err := New(platform.Options{DisableSyscallPatching: true, DisableFastPath: true})
	if err != nil {
		t.Fatalf("failed to create Systrap platform: %v", err)
	}
	const excess = 16
	ases := make([]platform.AddressSpace, maxAvailableSubprocesses+excess)
	for i := range ases {
		if ases[i], err = s.NewAddressSpace(); err != nil {
			t.Fatalf("NewAddressSpace %d: %v", i, err)
		}
	}
	stubs, _ := children(t)
	mapped := mappedBytes(t)
	for _, as := range ases {
		as.Release()
	}
	globalPool.mu.Lock()
	pooled := len(globalPool.available)
	globalPool.mu.Unlock()
	if pooled != maxAvailableSubprocesses {
		t.Errorf("pooled %d subprocesses, want %d", pooled, maxAvailableSubprocesses)
	}
	var left, zombies int
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if left, zombies = children(t); left <= stubs-excess && zombies == 0 {
			break
		}
	}
	if left > stubs-excess || zombies != 0 {
		t.Errorf("%d children, %d of them zombies, after destroying %d of %d stubs", left, zombies, excess, stubs)
	}
	// Each destroyed subprocess had its thread contexts mapped, at least.
	if got := mappedBytes(t); got > mapped-excess*uint64(stubContextRegionLen) {
		t.Errorf("%d bytes mapped after destroying %d subprocesses, from %d", got, excess, mapped)
	}
}
