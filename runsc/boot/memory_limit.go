// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package boot

import (
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
	kernel.SetMemoryLimit(limit)
	log.Infof("Keeping the sandbox's processes within its cgroup's limit of %d MiB of memory and swap", limit.Total>>20)
}

// sandboxMemoryLimit returns the limit of the sandbox's cgroup, which the
// spec gives, read through files, or false if the Sentry has none to keep:
// the cgroup's memory, or its swap, is unlimited, or the Sentry cannot read
// what the cgroup holds.
func sandboxMemoryLimit(args *Args, files map[string]*os.File) (kernel.MemoryLimit, bool) {
	if args.Spec == nil || args.Spec.Linux == nil || args.Spec.Linux.Resources == nil || args.Spec.Linux.Resources.Memory == nil {
		return kernel.MemoryLimit{}, false
	}
	memory := args.Spec.Linux.Resources.Memory
	if memory.Limit == nil || *memory.Limit <= 0 || memory.Swap == nil || *memory.Swap <= 0 {
		// Unlimited swap is a cgroup's default: the host never runs it out
		// of memory, but with the rest of the host.
		return kernel.MemoryLimit{}, false
	}
	limit := kernel.MemoryLimit{
		Total:   uint64(max(*memory.Limit, *memory.Swap)),
		Current: files["memory.current"],
		Stat:    files["memory.stat"],
	}
	if limit.Current == nil || limit.Stat == nil {
		return kernel.MemoryLimit{}, false
	}
	if *memory.Swap > *memory.Limit {
		// Swap that its cgroup does not account is unlimited.
		limit.SwapCurrent = files["memory.swap.current"]
		if limit.SwapCurrent == nil {
			return kernel.MemoryLimit{}, false
		}
	}
	return limit, true
}
