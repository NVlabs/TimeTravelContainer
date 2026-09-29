// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin && arm64

package safecopy

import (
	"testing"
	"unsafe"

	"gvisor.dev/gvisor/pkg/safecopy/machexc"
)

// TestFaultRecoveryReleasesPortRights checks that recovering from a fault
// releases the port rights that the Mach exception message carries.
func TestFaultRecoveryReleasesPortRights(t *testing.T) {
	const faults = 100
	withSegvErrorTestMapping(t, func(mapping []byte) {
		before, err := machexc.TaskPortRefs()
		if err != nil {
			t.Fatal(err)
		}
		dst := make([]byte, 1)
		for i := 0; i < faults; i++ {
			if _, err := CopyIn(dst, unsafe.Pointer(&mapping[pageSize])); err != (SegvError{uintptr(unsafe.Pointer(&mapping[pageSize]))}) {
				t.Fatalf("CopyIn from a PROT_NONE page returned %v, want SegvError", err)
			}
		}
		after, err := machexc.TaskPortRefs()
		if err != nil {
			t.Fatal(err)
		}
		if after > before {
			t.Errorf("task port send references grew from %d to %d over %d recovered faults", before, after, faults)
		}
	})
}
