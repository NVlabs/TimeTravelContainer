// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package linux

import "testing"

// Every capability has a name that CapabilityFromString accepts: runsc
// refuses a spec that names one "UNKNOWN", which Docker's --cap-add ALL made
// of the newest.
func TestCapabilityNames(t *testing.T) {
	for _, cp := range AllCapabilities() {
		name := cp.String()
		if name == "UNKNOWN" {
			t.Errorf("capability %d has no name", cp)
			continue
		}
		if got, ok := CapabilityFromString(name); !ok || got != cp {
			t.Errorf("CapabilityFromString(%q) = %d, %t; want %d", name, got, ok, cp)
		}
	}
	if n := len(AllCapabilities()); n != int(CAP_LAST_CAP)+1 {
		t.Errorf("AllCapabilities has %d capabilities, want %d", n, CAP_LAST_CAP+1)
	}
}
