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

package proc

import (
	"context"

	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/state"
)

// branchKernelRef is used by cached procfs data objects. A live-fork child may
// inherit one of these objects after the source has populated /proc/net. Its
// raw pointer must be rebound to the Kernel represented by the state stream;
// otherwise a later save follows the source Kernel into the child's object
// graph and reaches its actively updating Timekeeper.
//
// +stateify type
type branchKernelRef struct {
	k *kernel.Kernel
}

func newBranchKernelRef(k *kernel.Kernel) branchKernelRef {
	return branchKernelRef{k: k}
}

// StateSave serializes the branch-local Kernel, not a stale source pointer.
func (r *branchKernelRef) StateSave(s state.Sink) {
	k := r.k
	if current, ok := s.Context().Value(kernel.CtxKernel).(*kernel.Kernel); ok && current != nil {
		k = current
	}
	s.Save(0, &k)
}

// StateLoad restores the stable field format and canonicalizes after the root
// Kernel has been decoded.
func (r *branchKernelRef) StateLoad(ctx context.Context, s state.Source) {
	s.Load(0, &r.k)
	s.AfterLoad(func() {
		if current, ok := ctx.Value(kernel.CtxKernel).(*kernel.Kernel); ok && current != nil {
			r.k = current
		}
	})
}
