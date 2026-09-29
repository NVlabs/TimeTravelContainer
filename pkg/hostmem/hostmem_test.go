// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hostmem

import (
	"math"
	"testing"
)

func TestAllocationGeometry(t *testing.T) {
	g := AllocationGranule()
	if g == 0 || g&(g-1) != 0 {
		t.Fatalf("AllocationGranule() = %d, want a power of two", g)
	}
	if got := AllocationRoundDown(g + 1); got != g {
		t.Errorf("AllocationRoundDown(%d) = %d, want %d", g+1, got, g)
	}
	if got, err := AllocationRoundUp(g + 1); err != nil || got != 2*g {
		t.Errorf("AllocationRoundUp(%d) = (%d, %v), want (%d, nil)", g+1, got, err, 2*g)
	}
	if _, err := AllocationRoundUp(math.MaxUint64); err == nil {
		t.Error("AllocationRoundUp(MaxUint64) unexpectedly succeeded")
	}
	if err := ValidateAllocationRange(g, g); err != nil {
		t.Errorf("ValidateAllocationRange(%d, %d): %v", g, g, err)
	}
	if err := ValidateAllocationRange(g+1, g); err == nil {
		t.Error("ValidateAllocationRange accepted an unaligned address")
	}
}
