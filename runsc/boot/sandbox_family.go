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

package boot

import (
	"fmt"
	"sort"
	"sync"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/sentry/control"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	sentrytime "gvisor.dev/gvisor/pkg/sentry/time"
)

// DefaultBranchID is the branch used by existing, branch-unaware runsc RPCs.
const DefaultBranchID = "main"

// BranchState describes a live branch's lifecycle state.
type BranchState string

const (
	BranchCreating   BranchState = "creating"
	BranchPaused     BranchState = "paused"
	BranchRunning    BranchState = "running"
	BranchDestroying BranchState = "destroying"
	BranchFailed     BranchState = "failed"
)

// BranchInfo is the branch metadata exposed over the control socket.
type BranchInfo struct {
	ID       string
	ParentID string
	State    BranchState
}

// ForkBranchArgs selects the source and destination of an in-memory fork.
type ForkBranchArgs struct {
	// Source defaults to DefaultBranchID.
	Source string
	// ID is the new branch unique identifier.
	ID string
	// Paused leaves the new branch stopped after creation.
	Paused bool
}

// BranchArgs selects an existing branch. An empty ID selects the default.
type BranchArgs struct {
	ID string
}

type sandboxBranch struct {
	opMu sync.Mutex

	id       string
	parentID string
	k        *kernel.Kernel
	state    BranchState
}

// SandboxFamily owns all in-memory branches in one sentry process.
//
// The default branch is Loader.k. Child branches share immutable and COW
// backing with their ancestors, but each branch owns its Kernel lifecycle.
type SandboxFamily struct {
	mu sync.Mutex

	branches map[string]*sandboxBranch
}

// NewSandboxFamily creates a family containing k as its default branch.
func NewSandboxFamily(k *kernel.Kernel) *SandboxFamily {
	return &SandboxFamily{
		branches: map[string]*sandboxBranch{
			DefaultBranchID: {
				id:    DefaultBranchID,
				k:     k,
				state: kernelBranchState(k),
			},
		},
	}
}

func kernelBranchState(k *kernel.Kernel) BranchState {
	if k.IsPaused() {
		return BranchPaused
	}
	return BranchRunning
}

// ReplaceDefaultKernel replaces the default branch during checkpoint restore.
// No child branches may exist because checkpoint restore cannot preserve them.
func (f *SandboxFamily) ReplaceDefaultKernel(k *kernel.Kernel) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.branches) != 1 {
		return fmt.Errorf("cannot replace default kernel with %d live child branches", len(f.branches)-1)
	}
	b := f.branches[DefaultBranchID]
	b.k = k
	b.state = kernelBranchState(k)
	return nil
}

// HasChildren reports whether a checkpoint-incompatible child branch exists.
func (f *SandboxFamily) HasChildren() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.branches) != 1
}

// MarkDefaultStarted records the default kernel state after Loader.Run.
func (f *SandboxFamily) MarkDefaultStarted() {
	f.mu.Lock()
	b := f.branches[DefaultBranchID]
	b.state = kernelBranchState(b.k)
	f.mu.Unlock()
}

// List returns a stable snapshot sorted by branch ID.
func (f *SandboxFamily) List() []BranchInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]BranchInfo, 0, len(f.branches))
	for _, b := range f.branches {
		out = append(out, BranchInfo{ID: b.id, ParentID: b.parentID, State: b.state})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Fork creates a child from args.Source. Both branches remain paused unless
// their requested/prior state says otherwise.
func (f *SandboxFamily) Fork(args *ForkBranchArgs) (*BranchInfo, error) {
	if args == nil {
		return nil, fmt.Errorf("missing live-fork arguments")
	}
	if args.ID == "" {
		return nil, fmt.Errorf("branch ID must not be empty")
	}
	sourceID := args.Source
	if sourceID == "" {
		sourceID = DefaultBranchID
	}

	f.mu.Lock()
	source := f.branches[sourceID]
	if source == nil {
		f.mu.Unlock()
		return nil, fmt.Errorf("source branch %q does not exist", sourceID)
	}
	if _, ok := f.branches[args.ID]; ok {
		f.mu.Unlock()
		return nil, fmt.Errorf("branch %q already exists", args.ID)
	}
	childBranch := &sandboxBranch{id: args.ID, parentID: sourceID, state: BranchCreating}
	f.branches[args.ID] = childBranch
	f.mu.Unlock()

	fail := func(err error) (*BranchInfo, error) {
		f.mu.Lock()
		childBranch.state = BranchFailed
		delete(f.branches, args.ID)
		f.mu.Unlock()
		return nil, err
	}

	source.opMu.Lock()
	defer source.opMu.Unlock()
	f.mu.Lock()
	if source.state != BranchRunning && source.state != BranchPaused {
		state := source.state
		f.mu.Unlock()
		return fail(fmt.Errorf("source branch %q is %s", sourceID, state))
	}
	sourceWasPaused := source.k.IsPaused()
	f.mu.Unlock()

	if !sourceWasPaused {
		source.k.Pause()
	}
	resumeSource := func() error {
		if sourceWasPaused {
			return nil
		}
		source.k.Unpause()
		return control.PostResume(source.k, nil)
	}

	child, err := source.k.LiveFork(source.k.SupervisorContext(), kernel.LiveForkOptions{
		Clocks: sentrytime.NewCalibratedClocks(),
	})
	if err != nil {
		resumeErr := resumeSource()
		if resumeErr != nil {
			err = fmt.Errorf("%w; resuming source: %v", err, resumeErr)
		}
		return fail(fmt.Errorf("forking branch %q from %q: %w", args.ID, sourceID, err))
	}
	if err := child.StartPaused(); err != nil {
		// StartPaused failure is an invariant violation, but ensure ownership of
		// the child's shared MemoryFile is still released.
		child.Release()
		_ = resumeSource()
		return fail(fmt.Errorf("starting branch %q paused: %w", args.ID, err))
	}

	f.mu.Lock()
	childBranch.k = child
	childBranch.state = BranchPaused
	f.mu.Unlock()
	if !args.Paused {
		child.Unpause()
		if err := control.PostResume(child, nil); err != nil {
			child.Pause()
			child.Kill(linux.WaitStatusTerminationSignal(linux.SIGKILL))
			child.Unpause()
			child.WaitExited()
			child.Release()
			_ = resumeSource()
			return fail(fmt.Errorf("resuming branch %q: %w", args.ID, err))
		}
		f.mu.Lock()
		childBranch.state = kernelBranchState(child)
		f.mu.Unlock()
	}
	if err := resumeSource(); err != nil {
		child.Kill(linux.WaitStatusTerminationSignal(linux.SIGKILL))
		if child.IsPaused() {
			child.Unpause()
		}
		child.WaitExited()
		child.Release()
		return fail(fmt.Errorf("resuming source branch %q: %w", sourceID, err))
	}

	f.mu.Lock()
	source.state = kernelBranchState(source.k)
	info := BranchInfo{ID: childBranch.id, ParentID: childBranch.parentID, State: childBranch.state}
	f.mu.Unlock()
	return &info, nil
}

// Pause pauses a branch.
func (f *SandboxFamily) Pause(id string) error {
	b, err := f.branch(id)
	if err != nil {
		return err
	}
	b.opMu.Lock()
	defer b.opMu.Unlock()
	f.mu.Lock()
	if b.state != BranchRunning {
		state := b.state
		f.mu.Unlock()
		return fmt.Errorf("branch %q is %s, not running", id, state)
	}
	f.mu.Unlock()
	b.k.Pause()
	f.mu.Lock()
	b.state = BranchPaused
	f.mu.Unlock()
	return nil
}

// Resume resumes a paused branch.
func (f *SandboxFamily) Resume(id string) error {
	b, err := f.branch(id)
	if err != nil {
		return err
	}
	b.opMu.Lock()
	defer b.opMu.Unlock()
	f.mu.Lock()
	if b.state != BranchPaused {
		state := b.state
		f.mu.Unlock()
		return fmt.Errorf("branch %q is %s, not paused", id, state)
	}
	f.mu.Unlock()
	b.k.Unpause()
	if err := control.PostResume(b.k, nil); err != nil {
		b.k.Pause()
		return err
	}
	f.mu.Lock()
	b.state = kernelBranchState(b.k)
	f.mu.Unlock()
	return nil
}

// Destroy destroys a non-default branch and removes it from the family.
func (f *SandboxFamily) Destroy(id string) error {
	if id == DefaultBranchID {
		return fmt.Errorf("the default branch is owned by the loader")
	}
	b, err := f.branch(id)
	if err != nil {
		return err
	}
	b.opMu.Lock()
	defer b.opMu.Unlock()
	f.mu.Lock()
	if b.state == BranchCreating || b.state == BranchDestroying {
		state := b.state
		f.mu.Unlock()
		return fmt.Errorf("branch %q is %s", id, state)
	}
	for _, candidate := range f.branches {
		if candidate.parentID == id {
			f.mu.Unlock()
			return fmt.Errorf("branch %q still has child %q", id, candidate.id)
		}
	}
	b.state = BranchDestroying
	paused := b.k.IsPaused()
	f.mu.Unlock()

	b.k.Kill(linux.WaitStatusTerminationSignal(linux.SIGKILL))
	if paused {
		b.k.Unpause()
	}
	b.k.WaitExited()
	b.k.Release()

	f.mu.Lock()
	delete(f.branches, id)
	f.mu.Unlock()
	return nil
}

// DestroyChildren destroys all non-default branches during loader teardown.
func (f *SandboxFamily) DestroyChildren() {
	for {
		f.mu.Lock()
		var id string
		remaining := len(f.branches) - 1
		for candidateID := range f.branches {
			if candidateID == DefaultBranchID {
				continue
			}
			hasChild := false
			for _, possibleChild := range f.branches {
				if possibleChild.parentID == candidateID {
					hasChild = true
					break
				}
			}
			if !hasChild {
				id = candidateID
				break
			}
		}
		f.mu.Unlock()
		if id == "" {
			if remaining == 0 {
				return
			}
			panic("cycle in live branch family")
		}
		if err := f.Destroy(id); err != nil {
			// Creating branches are serialized with RPC teardown by the control
			// server. Treat any other state mismatch as a programming error.
			panic(fmt.Sprintf("destroying live branch %q: %v", id, err))
		}
	}
}

func (f *SandboxFamily) branch(id string) (*sandboxBranch, error) {
	if id == "" {
		id = DefaultBranchID
	}
	f.mu.Lock()
	b := f.branches[id]
	f.mu.Unlock()
	if b == nil {
		return nil, fmt.Errorf("branch %q does not exist", id)
	}
	return b, nil
}
