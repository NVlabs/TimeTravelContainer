// Copyright 2018 The gVisor Authors.
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

package kernel

import (
	"context"
	"io"

	"gvisor.dev/gvisor/pkg/sentry/inet"
	"gvisor.dev/gvisor/pkg/state"
	"gvisor.dev/gvisor/pkg/tcpip"
)

// afterLoad is invoked by stateify.
func (ts *TaskSet) afterLoad(_ context.Context) {
	ts.zeroLiveTasksCond.L = &ts.mu
}

// saveStateGraph serializes exactly one Kernel. A Sentry process may contain
// multiple TTC branches, while a few upstream registries remain process-wide.
// Filter branch-owned registry entries and reject any future path that reaches
// a peer Kernel instead of silently combining both branches in one image.
//
// Preconditions: k.extMu must be locked.
func (k *Kernel) saveStateGraph(ctx context.Context, w io.Writer) (state.Stats, error) {
	k.filteringBranchDanglingEndpoints = true
	defer func() {
		k.filteringBranchDanglingEndpoints = false
	}()
	return state.SaveWithOptions(ctx, w, k, state.SaveOptions{
		RejectMultipleRootObjects: true,
	})
}

// saveDanglingEndpoints is invoked by stateify.
func (k *Kernel) saveDanglingEndpoints() []tcpip.Endpoint {
	endpoints := tcpip.GetDanglingEndpoints()
	if !k.filteringBranchDanglingEndpoints {
		return endpoints
	}
	if k.rootNetworkNamespace == nil {
		return nil
	}
	var owners []inet.LiveForkDanglingEndpointOwner
	for _, ns := range k.rootNetworkNamespace.RelatedNamespaces() {
		if ns.Stack() == nil {
			continue
		}
		owner, ok := ns.Stack().(inet.LiveForkDanglingEndpointOwner)
		if !ok {
			// Network live-fork validation must reject stacks that cannot identify
			// branch ownership before serialization reaches this point.
			continue
		}
		owners = append(owners, owner)
	}
	owned := make([]tcpip.Endpoint, 0, len(endpoints))
	for _, endpoint := range endpoints {
		for _, owner := range owners {
			if owner.OwnsLiveForkDanglingEndpoint(endpoint) {
				owned = append(owned, endpoint)
				break
			}
		}
	}
	return owned
}

// loadDanglingEndpoints is invoked by stateify.
func (k *Kernel) loadDanglingEndpoints(_ context.Context, es []tcpip.Endpoint) {
	for _, e := range es {
		tcpip.AddDanglingEndpoint(e)
	}
}

// saveVforkParent is invoked by stateify.
func (t *Task) saveVforkParent() *Task {
	return t.vforkParent.Load()
}

// loadVforkParent is invoked by stateify.
func (t *Task) loadVforkParent(_ context.Context, vforkParent *Task) {
	t.vforkParent.Store(vforkParent)
}

// savePtraceTracer is invoked by stateify.
func (t *Task) savePtraceTracer() *Task {
	return t.ptraceTracer.Load()
}

// loadPtraceTracer is invoked by stateify.
func (t *Task) loadPtraceTracer(_ context.Context, tracer *Task) {
	t.ptraceTracer.Store(tracer)
}

// saveSeccomp is invoked by stateify.
func (t *Task) saveSeccomp() *taskSeccomp {
	return t.seccomp.Load()
}

// loadSeccomp is invoked by stateify.
func (t *Task) loadSeccomp(_ context.Context, seccompData *taskSeccomp) {
	t.seccomp.Store(seccompData)
}

// saveFsContext is invoked by stateify.
func (t *Task) saveFsContext() *FSContext {
	return t.fsContext.Load()
}

// loadFsContext is invoked by stateify.
func (t *Task) loadFsContext(_ context.Context, fsContext *FSContext) {
	t.fsContext.Store(fsContext)
}

// saveAppCPUClockLast is invoked by stateify.
func (tg *ThreadGroup) saveAppCPUClockLast() *Task {
	return tg.appCPUClockLast.Load()
}

// loadAppCPUClockLast is invoked by stateify.
func (tg *ThreadGroup) loadAppCPUClockLast(_ context.Context, task *Task) {
	tg.appCPUClockLast.Store(task)
}

// saveAppSysCPUClockLast is invoked by stateify.
func (tg *ThreadGroup) saveAppSysCPUClockLast() *Task {
	return tg.appSysCPUClockLast.Load()
}

// loadAppSysCPUClockLast is invoked by stateify.
func (tg *ThreadGroup) loadAppSysCPUClockLast(_ context.Context, task *Task) {
	tg.appSysCPUClockLast.Store(task)
}

// saveOldRSeqCritical is invoked by stateify.
func (tg *ThreadGroup) saveOldRSeqCritical() *OldRSeqCriticalRegion {
	return tg.oldRSeqCritical.Load()
}

// loadOldRSeqCritical is invoked by stateify.
func (tg *ThreadGroup) loadOldRSeqCritical(_ context.Context, r *OldRSeqCriticalRegion) {
	tg.oldRSeqCritical.Store(r)
}

// saveT is invoked by stateify.
func (p *pid) saveT() *Task {
	return p.t.Load()
}

// loadT is invoked by stateify.
func (p *pid) loadT(_ context.Context, t *Task) {
	p.t.Store(t)
}
