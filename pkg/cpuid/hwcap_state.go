// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cpuid

import (
	"context"

	"gvisor.dev/gvisor/pkg/state"
)

// hwCap has an explicit state implementation because it is declared in a
// host-independent file but populated by mutually exclusive host-specific
// files. Keeping the contract here prevents state generation from assigning
// it a host build constraint and makes the guest CPU feature contract portable.
func (*hwCap) StateTypeName() string { return "pkg/cpuid.hwCap" }

func (*hwCap) StateFields() []string { return []string{"hwCap1", "hwCap2"} }

// +checklocksignore
func (h *hwCap) StateSave(sink state.Sink) {
	sink.Save(0, &h.hwCap1)
	sink.Save(1, &h.hwCap2)
}

// +checklocksignore
func (h *hwCap) StateLoad(_ context.Context, source state.Source) {
	source.Load(0, &h.hwCap1)
	source.Load(1, &h.hwCap2)
}

func init() {
	state.Register((*hwCap)(nil))
}
