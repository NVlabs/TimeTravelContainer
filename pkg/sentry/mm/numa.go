// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mm

import (
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
)

// placementOf returns where the pages of an allocation for addr in vma go,
// under a host CPU topology: by vma's memory policy, or else by that of the
// task that allocates them, whose own node they go to unless a policy says
// otherwise.
func placementOf(ctx context.Context, vma *vma, addr hostarch.Addr) pgalloc.NUMAPlacement {
	policy, ok := ctx.Value(pgalloc.CtxMemoryPolicy).(pgalloc.MemoryPolicy)
	if !ok {
		return pgalloc.NUMAPlacement{}
	}
	return policy.Placement(vma.numaPolicy, vma.numaNodemask, uint64(addr)>>hostarch.HugePageShift)
}

// taskPlacement returns where the pages of addr go by the memory policy of
// the task of ctx alone, under a host CPU topology.
func taskPlacement(ctx context.Context, addr hostarch.Addr) pgalloc.NUMAPlacement {
	policy, ok := ctx.Value(pgalloc.CtxMemoryPolicy).(pgalloc.MemoryPolicy)
	if !ok {
		return pgalloc.NUMAPlacement{}
	}
	return policy.Placement(linux.MPOL_DEFAULT, 0, uint64(addr)>>hostarch.HugePageShift)
}

// PageNode returns the host memory node of the page at addr. If the page has
// no memory and allocate is true, it allocates it as a read would, as
// get_mempolicy(2) does with MPOL_F_NODE|MPOL_F_ADDR; otherwise it fails
// with ENOENT, as move_pages(2) does.
func (mm *MemoryManager) PageNode(ctx context.Context, addr hostarch.Addr, allocate bool) (int, error) {
	ar, ok := addr.RoundDown().ToRange(hostarch.PageSize)
	if !ok {
		return 0, linuxerr.EFAULT
	}
	mm.mappingMu.RLock()
	defer mm.mappingMu.RUnlock()
	vseg := mm.vmas.FindSegment(ar.Start)
	if !vseg.Ok() {
		return 0, linuxerr.EFAULT
	}
	mm.activeMu.Lock()
	defer mm.activeMu.Unlock()
	pseg := mm.pmas.FindSegment(ar.Start)
	if !pseg.Ok() {
		if !allocate {
			return 0, linuxerr.ENOENT
		}
		var err error
		if pseg, _, err = mm.getPMAsLocked(ctx, vseg, ar, hostarch.Read, false /* callerIndirectCommit */, false /* forPin */); err != nil {
			return 0, err
		}
	}
	if err := pseg.ensureInternalMappingsLocked(); err != nil {
		return 0, err
	}
	ims := mm.internalMappingsLocked(pseg, ar)
	if ims.IsEmpty() {
		return 0, linuxerr.EFAULT
	}
	return pgalloc.PageNode(ims.Head().Addr())
}
