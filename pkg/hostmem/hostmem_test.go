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
