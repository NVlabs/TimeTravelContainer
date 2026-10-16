// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kernel

import (
	"fmt"
	"math"
	"os"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

// cgroupFile stands in for a cgroup's file.
func cgroupFile(t *testing.T) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "cgroup-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func setCgroupFile(t *testing.T, f *os.File, content string) {
	t.Helper()
	if err := f.Truncate(0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte(content), 0); err != nil {
		t.Fatal(err)
	}
}

// What counts against a cgroup's limit is its memory and swap but the file
// cache and slab its reclaim may take. A family may allocate what its cgroup
// may still hold, less the slack, before it checks again, and is over its
// limit once it holds more than the limit less the slack.
func TestMemoryLimiterCheck(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "memory-")
	if err != nil {
		t.Fatal(err)
	}
	mf, err := pgalloc.NewMemoryFile(file, pgalloc.MemoryFileOpts{
		DelayedEviction:         pgalloc.DelayedEvictionDisabled,
		DisableIMAWorkAround:    true,
		DisableMemoryAccounting: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mf.Destroy()
	const mib = 1 << 20
	current, stat, swap := cgroupFile(t), cgroupFile(t), cgroupFile(t)
	l := &memoryLimiter{
		mf:      mf,
		limit:   &MemoryLimit{Total: 256 * mib, Current: current, Stat: stat, SwapCurrent: swap},
		slack:   32 * mib,
		kernels: make(map[*Kernel]struct{}),
	}
	// 150 MiB of memory, 30 MiB of it file cache and slab, and 50 MiB of
	// swap: 170 MiB counts, 54 MiB short of the limit less the slack.
	setCgroupFile(t, current, fmt.Sprintf("%d\n", 150*mib))
	setCgroupFile(t, stat, fmt.Sprintf("anon %d\nfile %d\nactive_file %d\ninactive_file %d\nshmem %d\nslab_reclaimable %d\n", 60*mib, 80*mib, 16*mib, 8*mib, 50*mib, 6*mib))
	setCgroupFile(t, swap, fmt.Sprintf("%d\n", 50*mib))
	if used, err := l.used(); err != nil || used != 170*mib {
		t.Fatalf("used() = %d MiB, %v; want 170 MiB", used/mib, err)
	}
	if !l.check() {
		t.Fatal("check() says a family 54 MiB short of its limit is over it")
	}
	if mf.OverCommitLimit() {
		t.Fatal("a family that has allocated nothing since its check may not allocate")
	}
	if _, err := mf.Allocate(50*mib, pgalloc.AllocOpts{Kind: usage.Anonymous}); err != nil {
		t.Fatal(err)
	}
	if mf.OverCommitLimit() {
		t.Error("allocating 50 of the 54 MiB its cgroup may still hold has a family checked")
	}
	if _, err := mf.Allocate(8*mib, pgalloc.AllocOpts{Kind: usage.Anonymous}); err != nil {
		t.Fatal(err)
	}
	if !mf.OverCommitLimit() {
		t.Error("allocating past what its cgroup may still hold does not have a family checked")
	}
	// The cgroup now holds 228 MiB, which counts.
	setCgroupFile(t, current, fmt.Sprintf("%d\n", 208*mib))
	if l.check() {
		t.Error("check() says a family 4 MiB over its limit less the slack is within it")
	}
}

// A family over its limit kills a process only once nothing frees memory any
// more: not while what was freed is being released, nor while the processes it
// killed exit, which took 0.2 s with the Sentry swapped out, nor before what
// its cgroup holds had time to show their memory gone. A family whose last
// victim took most of that time to exit killed its init.
func TestMemoryReclaimStalled(t *testing.T) {
	const mib = 1 << 20
	t0 := time.Now()
	killed := memoryReclaim{killed: true, least: 250 * mib, progressAt: t0}
	type step struct {
		ms      int
		usedMiB uint64
		exited  bool
		waste   bool
		kill    bool
	}
	for _, test := range []struct {
		name  string
		r     memoryReclaim
		steps []step
	}{
		{
			name:  "nothing killed, nothing freeing memory",
			r:     memoryReclaim{least: math.MaxUint64},
			steps: []step{{0, 240, true, false, true}},
		},
		{
			name: "nothing killed, what was freed being released",
			r:    memoryReclaim{least: math.MaxUint64},
			steps: []step{
				{0, 240, true, true, false},
				{10, 240, true, false, true},
			},
		},
		{
			name: "nothing killed, a release freeing too little",
			r:    memoryReclaim{least: math.MaxUint64},
			steps: []step{
				{0, 240, true, true, false},
				{1000, 200, true, true, false},
				{2999, 200, true, true, false},
				{3000, 200, true, true, true},
			},
		},
		{
			name: "a killed process slow to exit",
			r:    killed,
			steps: []step{
				{240, 250, false, false, false},
				{242, 250, true, true, false},
				{260, 250, true, false, false},
				{509, 250, true, false, false},
				{510, 250, true, false, true},
			},
		},
		{
			name: "a killed process that does not exit",
			r:    killed,
			steps: []step{
				{1999, 250, false, false, false},
				{2000, 250, false, false, true},
			},
		},
		{
			name: "a killed process shrinking as it exits",
			r:    killed,
			steps: []step{
				{1500, 240, false, false, false},
				{3499, 240, false, false, false},
				{3500, 240, false, false, true},
			},
		},
		{
			name: "what the cgroup holds shrinking after the exit",
			r:    killed,
			steps: []step{
				{10, 250, true, false, false},
				{200, 200, true, false, false},
				{449, 200, true, false, false},
				{450, 200, true, false, true},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := test.r
			for _, s := range test.steps {
				now := t0.Add(time.Duration(s.ms) * time.Millisecond)
				if got := r.stalled(now, s.usedMiB*mib, s.exited, s.waste); got != s.kill {
					t.Fatalf("at %d ms, holding %d MiB, exited %t, waste %t: stalled() = %t, want %t", s.ms, s.usedMiB, s.exited, s.waste, got, s.kill)
				}
			}
		})
	}
}

// A family over its limit again soon after it killed processes for it, as
// after what its cgroup holds dipped under its limit for a moment, goes on
// waiting for their memory rather than killing another: it killed its init
// while the process it killed first still exited. Once they have exited and a
// while has passed, it kills at once.
func TestMemoryReclaimWaitsForEarlierVictims(t *testing.T) {
	const mib = 1 << 20
	t0 := time.Now()
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	victim := &ThreadGroup{threadGroupNode: threadGroupNode{pidns: &PIDNamespace{owner: &TaskSet{}}, liveTasks: 1}}
	l := &memoryLimiter{
		victims:   []*ThreadGroup{victim},
		reclaimed: memoryReclaim{killed: true, killedAt: t0, least: 250 * mib, progressAt: t0},
	}
	victims, r := l.startReclaim(at(300))
	if len(victims) != 1 || !r.killed {
		t.Fatalf("startReclaim() = %d processes, killed %t; want the process that exits, killed", len(victims), r.killed)
	}
	if r.stalled(at(300), 250*mib, allExited(victims), false) {
		t.Error("killed another process while the one killed before exits")
	}
	victim.liveTasks = 0
	victims, r = l.startReclaim(at(1000))
	if len(victims) != 1 {
		t.Fatalf("startReclaim() = %d processes; want the process that exited 1 s after it was killed", len(victims))
	}
	if r.stalled(at(1000), 250*mib, allExited(victims), false) {
		t.Error("killed another process before what the cgroup holds had time to show the memory freed")
	}
	if !r.stalled(at(1250), 250*mib, allExited(victims), false) {
		t.Error("did not kill another process once the cgroup had time to show the memory freed")
	}
	if victims, r := l.startReclaim(at(2000)); len(victims) != 0 || r.killed {
		t.Errorf("startReclaim() = %d processes, killed %t; want none, 2 s after the process killed before", len(victims), r.killed)
	}
}
