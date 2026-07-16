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
	"fmt"
	"testing"
	"time"

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
	if child.ForkMetrics.Total <= 0 || child.ForkMetrics.Kernel.MetadataStream <= 0 || child.ForkMetrics.ChildStart <= 0 {
		t.Fatalf("incomplete fork metrics: %+v", child.ForkMetrics)
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

func TestSandboxFamilyRepeatedBranchTreesDoNotLeakPages(t *testing.T) {
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
	// Exclude one-time source-side lazy initialization from the leak baseline.
	// A retained branch reference would continue growing after this warm-up.
	if _, err := family.Fork(&ForkBranchArgs{ID: "warm-up", Paused: true}); err != nil {
		t.Fatalf("warm-up fork: %v", err)
	}
	if err := family.Destroy("warm-up"); err != nil {
		t.Fatalf("warm-up destroy: %v", err)
	}
	baselineUsage, err := parent.MemoryFile().TotalUsage()
	if err != nil {
		t.Fatalf("initial MemoryFile.TotalUsage: %v", err)
	}

	for round := 0; round < 20; round++ {
		childID := fmt.Sprintf("child-%d", round)
		siblingID := fmt.Sprintf("sibling-%d", round)
		grandchildID := fmt.Sprintf("grandchild-%d", round)
		greatGrandchildID := fmt.Sprintf("great-grandchild-%d", round)
		if _, err := family.Fork(&ForkBranchArgs{ID: childID, Paused: true}); err != nil {
			t.Fatalf("round %d child fork: %v", round, err)
		}
		if _, err := family.Fork(&ForkBranchArgs{ID: siblingID, Paused: true}); err != nil {
			t.Fatalf("round %d sibling fork: %v", round, err)
		}
		if _, err := family.Fork(&ForkBranchArgs{Source: childID, ID: grandchildID, Paused: true}); err != nil {
			t.Fatalf("round %d grandchild fork: %v", round, err)
		}
		if _, err := family.Fork(&ForkBranchArgs{Source: grandchildID, ID: greatGrandchildID, Paused: true}); err != nil {
			t.Fatalf("round %d great-grandchild fork: %v", round, err)
		}

		// Destroy two ancestors while their descendants remain, then remove
		// the surviving leaves in a different lineage order.
		for _, id := range []string{childID, grandchildID, siblingID, greatGrandchildID} {
			if err := family.Destroy(id); err != nil {
				t.Fatalf("round %d destroy %q: %v", round, id, err)
			}
		}
		if got := family.List(); len(got) != 1 || got[0].ID != DefaultBranchID {
			t.Fatalf("round %d branches after destroy: %+v", round, got)
		}
	}

	finalUsage, err := parent.MemoryFile().TotalUsage()
	if err != nil {
		t.Fatalf("final MemoryFile.TotalUsage: %v", err)
	}
	if finalUsage != baselineUsage {
		t.Errorf("live branch page usage leaked: before=%d after=%d", baselineUsage, finalUsage)
	}
	parent.Pause()
	parent.Release()
}

func BenchmarkSandboxFamilyFork(b *testing.B) {
	parent, err := testutil.Boot()
	if err != nil {
		b.Fatalf("Boot: %v", err)
	}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	parent.Pause()
	if err := parent.StartPaused(); err != nil {
		b.Fatalf("StartPaused: %v", err)
	}
	parent.Unpause()
	family := NewSandboxFamily(parent, SandboxFamilyOptions{})

	var totals SandboxForkMetrics
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := fmt.Sprintf("bench-%d", i)
		child, err := family.Fork(&ForkBranchArgs{ID: id, Paused: true})
		if err != nil {
			b.Fatalf("Fork %d: %v", i, err)
		}
		b.StopTimer()
		totals.SourcePause += child.ForkMetrics.SourcePause
		totals.SourceDowntime += child.ForkMetrics.SourceDowntime
		totals.Kernel.Validation += child.ForkMetrics.Kernel.Validation
		totals.Kernel.ParentPreparation += child.ForkMetrics.Kernel.ParentPreparation
		totals.Kernel.MetadataStream += child.ForkMetrics.Kernel.MetadataStream
		totals.Kernel.ChildRestore += child.ForkMetrics.Kernel.ChildRestore
		totals.ChildStart += child.ForkMetrics.ChildStart
		totals.SourceResume += child.ForkMetrics.SourceResume
		totals.Total += child.ForkMetrics.Total
		if err := family.Destroy(id); err != nil {
			b.Fatalf("Destroy %d: %v", i, err)
		}
		b.StartTimer()
	}
	b.StopTimer()
	perOp := func(d time.Duration) float64 { return float64(d.Nanoseconds()) / float64(b.N) }
	b.ReportMetric(perOp(totals.SourcePause), "source-pause-ns/op")
	b.ReportMetric(perOp(totals.SourceDowntime), "source-downtime-ns/op")
	b.ReportMetric(perOp(totals.Kernel.Validation), "validation-ns/op")
	b.ReportMetric(perOp(totals.Kernel.ParentPreparation), "preparation-ns/op")
	b.ReportMetric(perOp(totals.Kernel.MetadataStream), "metadata-ns/op")
	b.ReportMetric(perOp(totals.Kernel.ChildRestore), "restore-ns/op")
	b.ReportMetric(perOp(totals.ChildStart), "child-start-ns/op")
	b.ReportMetric(perOp(totals.SourceResume), "source-resume-ns/op")
	b.ReportMetric(perOp(totals.Total), "api-ns/op")

	parent.Pause()
	parent.Release()
}
