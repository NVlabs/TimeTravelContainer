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
	"testing"

	"gvisor.dev/gvisor/pkg/sentry/fsimpl/testutil"
)

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

	family := NewSandboxFamily(parent)
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
	if err := family.Destroy("child"); err == nil {
		t.Fatal("Destroy child succeeded while it still had a child")
	}
	family.DestroyChildren()
	if got := family.List(); len(got) != 1 || got[0].ID != DefaultBranchID {
		t.Fatalf("List after destroy: %+v", got)
	}

	parent.Pause()
	parent.Release()
}
