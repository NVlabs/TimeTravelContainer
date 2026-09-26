// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

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
