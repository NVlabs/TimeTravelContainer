// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package boot

import (
	"math"
	"os"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// The Sentry keeps a sandbox within its cgroup's memory limit, and its swap:
// as far as the host has swap when the cgroup may swap more than the memory
// limit, all of the host's swap when its swap is unlimited, and none when the
// swap limit is the memory limit. It keeps none it cannot read the cgroup for.
func TestSandboxMemoryLimit(t *testing.T) {
	file := func() *os.File {
		f, err := os.CreateTemp(t.TempDir(), "cgroup-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		return f
	}
	const gib = 1 << 30
	all := map[string]*os.File{"memory.current": file(), "memory.stat": file(), "memory.swap.current": file(), "meminfo": file()}
	noMeminfo := map[string]*os.File{"memory.current": all["memory.current"], "memory.stat": all["memory.stat"], "memory.swap.current": all["memory.swap.current"]}
	noSwap := map[string]*os.File{"memory.current": all["memory.current"], "memory.stat": all["memory.stat"]}
	limit := func(v int64) *int64 { return &v }
	for _, test := range []struct {
		name    string
		memory  *specs.LinuxMemory
		files   map[string]*os.File
		ok      bool
		total   uint64
		swap    bool
		meminfo bool
	}{
		{name: "no memory limit", memory: &specs.LinuxMemory{Swap: limit(2 * gib)}, files: all},
		{name: "swap twice memory", memory: &specs.LinuxMemory{Limit: limit(gib), Swap: limit(2 * gib)}, files: all, ok: true, total: 2 * gib, swap: true, meminfo: true},
		{name: "swap twice memory, no meminfo", memory: &specs.LinuxMemory{Limit: limit(gib), Swap: limit(2 * gib)}, files: noMeminfo, ok: true, total: 2 * gib, swap: true},
		{name: "swap twice memory, swap not accounted", memory: &specs.LinuxMemory{Limit: limit(gib), Swap: limit(2 * gib)}, files: noSwap},
		{name: "no swap", memory: &specs.LinuxMemory{Limit: limit(gib), Swap: limit(gib)}, files: all, ok: true, total: gib},
		{name: "unlimited swap", memory: &specs.LinuxMemory{Limit: limit(gib), Swap: limit(-1)}, files: all, ok: true, total: math.MaxUint64, swap: true, meminfo: true},
		{name: "swap unset", memory: &specs.LinuxMemory{Limit: limit(gib)}, files: all, ok: true, total: math.MaxUint64, swap: true, meminfo: true},
		{name: "unlimited swap, no meminfo", memory: &specs.LinuxMemory{Limit: limit(gib), Swap: limit(-1)}, files: noMeminfo},
		{name: "no cgroup files", memory: &specs.LinuxMemory{Limit: limit(gib), Swap: limit(gib)}, files: map[string]*os.File{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := &Args{Spec: &specs.Spec{Linux: &specs.Linux{Resources: &specs.LinuxResources{Memory: test.memory}}}}
			got, ok := sandboxMemoryLimit(args, test.files)
			if ok != test.ok {
				t.Fatalf("sandboxMemoryLimit() = %t; want %t", ok, test.ok)
			}
			if !ok {
				return
			}
			if got.Total != test.total || got.Memory != gib || (got.SwapCurrent != nil) != test.swap || (got.HostMeminfo != nil) != test.meminfo {
				t.Errorf("sandboxMemoryLimit() = total %d, memory %d, swap file %t, meminfo %t; want total %d, memory %d, swap file %t, meminfo %t",
					got.Total, got.Memory, got.SwapCurrent != nil, got.HostMeminfo != nil, test.total, uint64(gib), test.swap, test.meminfo)
			}
		})
	}
}
