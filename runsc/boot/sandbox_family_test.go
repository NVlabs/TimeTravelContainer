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
	"errors"
	"testing"

	"gvisor.dev/gvisor/pkg/sentry/fsimpl/testutil"
)

func TestSandboxFamilyForkFailureCleanup(t *testing.T) {
	stages := []SandboxForkStage{
		SandboxForkStagePlaceholderRegistered,
		SandboxForkStageSourcePaused,
		SandboxForkStageKernelCloned,
		SandboxForkStageChildStarted,
		SandboxForkStageBranchReady,
	}
	for _, stage := range stages {
		t.Run(string(stage), func(t *testing.T) {
			parent, err := testutil.Boot()
			if err != nil {
				t.Fatalf("Boot: %v", err)
			}
			parent.PipeMount().IncRef()
			parent.SetHostMount(parent.PipeMount())
			parent.Pause()
			if err := parent.StartPaused(); err != nil {
				t.Fatalf("StartPaused: %v", err)
			}
			parent.Unpause()

			injected := errors.New("injected sandbox-family fork failure")
			failOnce := true
			family := NewSandboxFamily(parent, SandboxFamilyOptions{
				TestOnlyForkFailureInjector: func(completed SandboxForkStage) error {
					if failOnce && completed == stage {
						failOnce = false
						return injected
					}
					return nil
				},
			})
			if child, err := family.Fork(&ForkBranchArgs{ID: "failed", Paused: true}); child != nil || !errors.Is(err, injected) {
				t.Fatalf("Fork failure at %s: got child=%+v err=%v", stage, child, err)
			}
			if got := family.List(); len(got) != 1 || got[0].ID != DefaultBranchID || got[0].State != BranchRunning {
				t.Fatalf("family after failure at %s: %+v", stage, got)
			}
			child, err := family.Fork(&ForkBranchArgs{ID: "retry", Paused: true})
			if err != nil {
				t.Fatalf("Fork retry after %s: %v", stage, err)
			}
			if child.State != BranchPaused {
				t.Fatalf("retry child after %s: %+v", stage, child)
			}
			if err := family.Destroy(child.ID); err != nil {
				t.Fatalf("Destroy retry after %s: %v", stage, err)
			}
			parent.Pause()
			parent.Release()
		})
	}
}

func TestSandboxFamilyBranchLifecycle(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	parent.Pause()
	if err := parent.StartPaused(); err != nil {
		t.Fatalf("StartPaused: %v", err)
	}
	parent.Unpause()

	family := NewSandboxFamily(parent, SandboxFamilyOptions{})
	child, err := family.Fork(&ForkBranchArgs{ID: "child", Paused: true})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if child.ID != "child" || child.ParentID != DefaultBranchID || child.State != BranchPaused {
		t.Fatalf("Fork returned %+v", child)
	}
	branches := family.List()
	if len(branches) != 2 || branches[0].ID != "child" || branches[1].ID != DefaultBranchID {
		t.Fatalf("List after fork: %+v", branches)
	}
	if branches[1].State != BranchRunning {
		t.Fatalf("source state after fork: %s", branches[1].State)
	}

	if err := family.Resume("child"); err != nil {
		t.Fatalf("Resume child: %v", err)
	}
	if err := family.Pause("child"); err != nil {
		t.Fatalf("Pause child: %v", err)
	}
	grandchild, err := family.Fork(&ForkBranchArgs{Source: "child", ID: "grandchild", Paused: true})
	if err != nil {
		t.Fatalf("Fork grandchild: %v", err)
	}
	if grandchild.ParentID != "child" {
		t.Fatalf("grandchild parent: %q", grandchild.ParentID)
	}
	// Descendants own all references needed by their cloned Kernels. Destroying
	// an ancestor must not constrain their lifetime; ParentID remains historical
	// creation lineage even after that source no longer exists.
	if err := family.Destroy("child"); err != nil {
		t.Fatalf("Destroy child before grandchild: %v", err)
	}
	branches = family.List()
	if len(branches) != 2 || branches[0].ID != "grandchild" || branches[0].ParentID != "child" {
		t.Fatalf("List after ancestor destroy: %+v", branches)
	}
	if err := family.Resume("grandchild"); err != nil {
		t.Fatalf("Resume grandchild after ancestor destroy: %v", err)
	}
	if err := family.Pause("grandchild"); err != nil {
		t.Fatalf("Pause grandchild after ancestor destroy: %v", err)
	}
	greatGrandchild, err := family.Fork(&ForkBranchArgs{Source: "grandchild", ID: "great-grandchild", Paused: true})
	if err != nil {
		t.Fatalf("Fork from grandchild after ancestor destroy: %v", err)
	}
	if greatGrandchild.ParentID != "grandchild" {
		t.Fatalf("great-grandchild parent: %q", greatGrandchild.ParentID)
	}
	if err := family.Destroy("grandchild"); err != nil {
		t.Fatalf("Destroy grandchild before great-grandchild: %v", err)
	}
	if err := family.Resume("great-grandchild"); err != nil {
		t.Fatalf("Resume great-grandchild after ancestor destroy: %v", err)
	}
	family.DestroyChildren()
	if got := family.List(); len(got) != 1 || got[0].ID != DefaultBranchID {
		t.Fatalf("List after destroy: %+v", got)
	}

	parent.Pause()
	parent.Release()
}
