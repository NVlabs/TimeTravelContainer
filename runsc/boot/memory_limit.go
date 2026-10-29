// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package boot

import (
	"math"
	"os"

	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
)

// limitSandboxMemory has the sandbox's Kernels keep their processes within
// the memory and swap limit of the sandbox's cgroup, if Config.SentryOOMKill
// asks for it and the cgroup has such a limit; see kernel.SetMemoryLimit.
func limitSandboxMemory(args *Args) {
	if !args.Conf.SentryOOMKill || args.Conf.Platform != "systrap" {
		return
	}
	// Donated descriptors are above stdio's; 0 is an Args that set none.
	files := map[string]int{
		"memory.current":      args.MemoryCurrentFD,
		"memory.stat":         args.MemoryStatFD,
		"memory.swap.current": args.MemorySwapCurrentFD,
		"meminfo":             args.HostMeminfoFD,
	}
	opened := make(map[string]*os.File)
	for name, fd := range files {
		if fd > 0 {
			opened[name] = os.NewFile(uintptr(fd), name)
		}
	}
	limit, ok := sandboxMemoryLimit(args, opened)
	if !ok {
		for _, f := range opened {
			_ = f.Close()
		}
		return
	}
	if opened["memory.swap.current"] != nil && limit.SwapCurrent == nil {
		_ = opened["memory.swap.current"].Close()
	}
	if opened["meminfo"] != nil && limit.HostMeminfo == nil {
		_ = opened["meminfo"].Close()
	}
	kernel.SetMemoryLimit(limit)
	switch {
	case limit.SwapCurrent == nil:
		log.Infof("Keeping the sandbox's processes within its cgroup's limit of %d MiB of memory", limit.Memory>>20)
	case limit.Total == math.MaxUint64:
		log.Infof("Keeping the sandbox's processes within its cgroup's limit of %d MiB of memory and the host's swap", limit.Memory>>20)
	default:
		log.Infof("Keeping the sandbox's processes within its cgroup's limit of %d MiB of memory and swap, %d MiB of it memory, as far as the host has swap", limit.Total>>20, limit.Memory>>20)
	}
}

// sandboxMemoryLimit returns the limit of the sandbox's cgroup, which the
// spec gives, read through files, or false if the Sentry has none to keep:
// the cgroup's memory is unlimited, or the Sentry cannot read what the
// cgroup holds, or, if it may swap, the swap it holds or, for unlimited
// swap, the host's free swap.
func sandboxMemoryLimit(args *Args, files map[string]*os.File) (kernel.MemoryLimit, bool) {
	if args.Spec == nil || args.Spec.Linux == nil || args.Spec.Linux.Resources == nil || args.Spec.Linux.Resources.Memory == nil {
		return kernel.MemoryLimit{}, false
	}
	memory := args.Spec.Linux.Resources.Memory
	if memory.Limit == nil || *memory.Limit <= 0 {
		return kernel.MemoryLimit{}, false
	}
	limit := kernel.MemoryLimit{
		Total:   uint64(*memory.Limit),
		Memory:  uint64(*memory.Limit),
		Current: files["memory.current"],
		Stat:    files["memory.stat"],
	}
	if limit.Current == nil || limit.Stat == nil {
		return kernel.MemoryLimit{}, false
	}
	// A cgroup's swap is unlimited unless the spec limits it, and with no
	// memory.swap.current its kernel does not account it.
	unlimitedSwap := memory.Swap == nil || *memory.Swap < 0
	if unlimitedSwap || *memory.Swap > *memory.Limit {
		limit.SwapCurrent = files["memory.swap.current"]
		limit.HostMeminfo = files["meminfo"]
		if limit.SwapCurrent == nil {
			return kernel.MemoryLimit{}, false
		}
		if unlimitedSwap {
			// The cgroup holds as much as the host has swap for.
			if limit.HostMeminfo == nil {
				return kernel.MemoryLimit{}, false
			}
			limit.Total = math.MaxUint64
		} else {
			limit.Total = uint64(*memory.Swap)
		}
	}
	return limit, true
}
