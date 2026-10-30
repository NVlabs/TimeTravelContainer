// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mm

import (
	"testing"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/usermem"
)

// pressureAddressSpace is an AddressSpace whose platform is short of host
// mappings.
type pressureAddressSpace struct {
	platform.AddressSpace
}

// MappingPressure implements platform.MappingPressure.MappingPressure.
func (pressureAddressSpace) MappingPressure() bool {
	return true
}

// A fork that writes scattered pages of 2 MiB that it shares with its parent
// leaves its copies and the shared pages interleaved, a pma and a host
// mapping each. Under mapping pressure, once more than 16 pmas map the 2 MiB,
// a write copies all of it: one pma, which the host maps with one mapping.
// The fork reads what it wrote and what it shared, and its parent what it
// had.
func TestDensifyUnderMappingPressure(t *testing.T) {
	for _, pressure := range []bool{false, true} {
		t.Run(map[bool]string{false: "no pressure", true: "pressure"}[pressure], func(t *testing.T) {
			ctx, parent := homeTestMemoryManager(t, false)
			defer parent.DecUsers(ctx)
			addr := mapPrivate(ctx, t, parent, 2*hostarch.HugePageSize)
			block := hostarch.AddrRange{Start: (addr + hostarch.HugePageSize - 1).HugeRoundDown()}
			block.End = block.Start + hostarch.HugePageSize
			const pages = hostarch.HugePageSize / hostarch.PageSize
			writePagesInRandomOrder(ctx, t, parent, block.Start, pages, 1)
			child, err := cloneMemoryManagerMetadata(ctx, parent)
			if err != nil {
				t.Fatalf("cloneMemoryManagerMetadata: %v", err)
			}
			defer child.DecUsers(ctx)
			as := child.as
			if pressure {
				child.as = pressureAddressSpace{as}
			}
			before := densifiedBlocks.Load()
			// Every fourth page, which leaves the copies apart.
			for page := 0; page < pages; page += 4 {
				if _, err := child.CopyOut(ctx, block.Start+hostarch.Addr(page)*hostarch.PageSize, []byte{2}, usermem.IOOpts{}); err != nil {
					t.Fatalf("writing page %d: %v", page, err)
				}
			}
			child.as = as
			n := countPMAs(child, block)
			densified := densifiedBlocks.Load() - before
			if pressure && (n != 1 || densified != 1) {
				t.Errorf("under mapping pressure, %d pmas map the 2 MiB, and %d blocks were copied whole; want 1 and 1", n, densified)
			}
			if !pressure && (n < densifyMinPMAs || densified != 0) {
				t.Errorf("without mapping pressure, %d pmas map the 2 MiB, and %d blocks were copied whole; want copies apart and none", n, densified)
			}
			if pressure {
				checkAddressOrdered(t, child, block.Start, pages)
			}
			for page := 0; page < pages; page++ {
				pageAddr := block.Start + hostarch.Addr(page)*hostarch.PageSize
				want := byte(1)
				if page%4 == 0 {
					want = 2
				}
				var b [1]byte
				if _, err := child.CopyIn(ctx, pageAddr, b[:], usermem.IOOpts{}); err != nil || b[0] != want {
					t.Fatalf("the fork reads %d from page %d (%v), want %d", b[0], page, err, want)
				}
				if _, err := parent.CopyIn(ctx, pageAddr, b[:], usermem.IOOpts{}); err != nil || b[0] != 1 {
					t.Fatalf("the parent reads %d from page %d (%v), want 1", b[0], page, err)
				}
			}
			// Its pages are its own: a write takes them over in place.
			if _, err := child.CopyOut(ctx, block.Start+hostarch.PageSize, []byte{3}, usermem.IOOpts{}); err != nil {
				t.Fatal(err)
			}
			if pressure {
				checkAddressOrdered(t, child, block.Start, pages)
			}
		})
	}
}
