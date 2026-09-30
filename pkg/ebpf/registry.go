// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ebpf

import (
	"math"

	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
)

// MemoryLimit bounds the memory of a kernel's eBPF objects, in bytes. Linux
// charges them to memory cgroups; the sentry's own memory has none, so a
// kernel has a budget.
const MemoryLimit = 512 << 20

// objectOverhead is the memory charged for an object, besides its data.
const objectOverhead = 256

// Registry is a kernel's eBPF state: the IDs of its objects, the memory
// they use, and the sysctls that govern them.
//
// +stateify savable
type Registry struct {
	// nextProgID and nextMapID are the last IDs that programs and maps got.
	// As Linux's, IDs are sequential, and wrap.
	nextProgID atomicbitops.Uint32
	nextMapID  atomicbitops.Uint32

	// memory is the memory charged for the kernel's objects, in bytes.
	memory atomicbitops.Int64

	// UnprivilegedDisabled is the kernel.unprivileged_bpf_disabled sysctl:
	// unless 0, only privileged users may create maps and load programs. 1
	// can't be undone.
	UnprivilegedDisabled atomicbitops.Int32

	// JITEnable, JITKallsyms, and JITHarden are the net.core.bpf_jit_enable,
	// bpf_jit_kallsyms, and bpf_jit_harden sysctls. Programs never run, so
	// they have no effect.
	JITEnable   atomicbitops.Int32
	JITKallsyms atomicbitops.Int32
	JITHarden   atomicbitops.Int32
}

// Init initializes r with Linux's defaults: unprivileged eBPF is disabled,
// but may be enabled (CONFIG_BPF_UNPRIV_DEFAULT_OFF), and the JIT is on.
func (r *Registry) Init() {
	r.UnprivilegedDisabled.Store(2)
	r.JITEnable.Store(1)
}

// newID returns the next ID of next, which is in [1, INT_MAX).
func (r *Registry) newID(next *atomicbitops.Uint32) BPFID {
	for {
		id := next.Add(1)
		if id != 0 && id < math.MaxInt32 {
			return BPFID(id)
		}
		next.CompareAndSwap(id, 0)
	}
}

// charge charges n bytes to r, and fails with ENOMEM if that exceeds its
// budget.
func (r *Registry) charge(n int64) error {
	if n < 0 || n > MemoryLimit || r.memory.Add(n) > MemoryLimit {
		if n >= 0 && n <= MemoryLimit {
			r.memory.Add(-n)
		}
		return linuxerr.ENOMEM
	}
	return nil
}

// uncharge returns n bytes, that charge charged, to r.
func (r *Registry) uncharge(n int64) {
	r.memory.Add(-n)
}

// Memory returns the memory charged for r's objects, in bytes.
func (r *Registry) Memory() int64 {
	return r.memory.Load()
}
