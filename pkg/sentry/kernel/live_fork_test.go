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

package kernel_test

import (
	"errors"
	"testing"

	"gvisor.dev/gvisor/pkg/sentry/fsimpl/testutil"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	sentrytime "gvisor.dev/gvisor/pkg/sentry/time"
)

var errInjectedLiveForkFailure = errors.New("injected live-fork failure")

func TestLiveForkFailureCleanup(t *testing.T) {
	stages := []kernel.LiveForkStage{
		kernel.LiveForkStageValidated,
		kernel.LiveForkStageSharedMemoryPrepared,
		kernel.LiveForkStageMappingsInvalidated,
		kernel.LiveForkStageVFSPrepared,
		kernel.LiveForkStageMemoryPrepared,
		kernel.LiveForkStageOwnerAcquired,
		kernel.LiveForkStageChildInitialized,
		kernel.LiveForkStageMetadataLoaded,
		kernel.LiveForkStageVDSOPrivatized,
		kernel.LiveForkStageNetworkRestored,
		kernel.LiveForkStageVFSRestored,
		kernel.LiveForkStageChildPaused,
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

			child, err := parent.LiveFork(parent.SupervisorContext(), kernel.LiveForkOptions{
				Clocks: sentrytime.NewCalibratedClocks(),
				TestOnlyFailureInjector: func(completed kernel.LiveForkStage) error {
					if completed == stage {
						return errInjectedLiveForkFailure
					}
					return nil
				},
			})
			if child != nil || !errors.Is(err, errInjectedLiveForkFailure) {
				t.Fatalf("LiveFork failure at %s: got child=%p err=%v", stage, child, err)
			}
			if !parent.IsPaused() {
				t.Fatalf("parent resumed after failure at %s", stage)
			}

			// A successful retry proves that save hooks, CoW transitions, owner
			// references, and partial child state were all left reusable.
			child, err = parent.LiveFork(parent.SupervisorContext(), kernel.LiveForkOptions{
				Clocks: sentrytime.NewCalibratedClocks(),
			})
			if err != nil {
				t.Fatalf("LiveFork retry after %s: %v", stage, err)
			}
			if err := parent.StartPaused(); err != nil {
				t.Fatalf("parent.StartPaused after %s: %v", stage, err)
			}
			if err := child.StartPaused(); err != nil {
				t.Fatalf("child.StartPaused after %s: %v", stage, err)
			}
			parent.Unpause()
			child.Unpause()
			parent.Pause()
			child.Pause()
			child.Release()
			parent.Release()
		})
	}
}

func TestLiveForkTwoKernelsSharePlatformAndMemoryFile(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	parent.Pause()

	var metrics kernel.LiveForkMetrics
	child, err := parent.LiveFork(parent.SupervisorContext(), kernel.LiveForkOptions{
		Clocks:  sentrytime.NewCalibratedClocks(),
		Metrics: &metrics,
	})
	if err != nil {
		t.Fatalf("LiveFork: %v", err)
	}

	if parent.Platform != child.Platform {
		t.Error("parent and child do not share a platform")
	}
	if parent.MemoryFile() != child.MemoryFile() {
		t.Error("parent and child do not share a MemoryFile")
	}
	if parent.RootPIDNamespace() == child.RootPIDNamespace() {
		t.Error("parent and child share a PID namespace object")
	}
	if metrics.Total <= 0 || metrics.MetadataStream <= 0 || metrics.ChildRestore <= 0 {
		t.Errorf("incomplete live-fork metrics: %+v", metrics)
	}
	parentInitID := parent.RootPIDNamespace().IDOfThreadGroup(parent.GlobalInit())
	childInitID := child.RootPIDNamespace().IDOfThreadGroup(child.GlobalInit())
	if parentInitID != childInitID {
		t.Errorf("init PID changed across live fork: parent=%d child=%d", parentInitID, childInitID)
	}

	if err := parent.StartPaused(); err != nil {
		t.Fatalf("parent.StartPaused: %v", err)
	}
	if err := child.StartPaused(); err != nil {
		t.Fatalf("child.StartPaused: %v", err)
	}
	parent.Unpause()
	child.Unpause()

	parent.Pause()
	child.Pause()
	parent.Release()
	child.Release()
}

func TestLiveForkClonesPIDFDTarget(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	parentTask := parent.GlobalInit().Leader()
	parentTID := parent.RootPIDNamespace().IDOfTask(parentTask)
	pidfd, err := parentTask.PIDFDOpen(parentTID, false /* isThread */, false /* nonBlock */)
	if err != nil {
		t.Fatalf("PIDFDOpen: %v", err)
	}
	const pidfdNumber = int32(10)
	if _, err := parentTask.FDTable().NewFDAt(parentTask, pidfdNumber, pidfd, kernel.FDFlags{}); err != nil {
		pidfd.DecRef(parentTask)
		t.Fatalf("installing pidfd: %v", err)
	}
	pidfd.DecRef(parentTask)
	parent.Pause()

	child, err := parent.LiveFork(parent.SupervisorContext(), kernel.LiveForkOptions{
		Clocks: sentrytime.NewCalibratedClocks(),
	})
	if err != nil {
		t.Fatalf("LiveFork with pidfd: %v", err)
	}
	childTask := child.GlobalInit().Leader()
	for name, check := range map[string]struct {
		task *kernel.Task
		want *kernel.Task
	}{
		"parent": {parentTask, parentTask},
		"child":  {childTask, childTask},
	} {
		file := check.task.GetFile(pidfdNumber)
		if file == nil {
			t.Fatalf("%s pidfd is missing", name)
		}
		got, err := kernel.PIDFDTask(file)
		file.DecRef(check.task)
		if err != nil {
			t.Fatalf("resolving %s pidfd: %v", name, err)
		}
		if got != check.want {
			t.Errorf("%s pidfd target=%p, want branch-local task %p", name, got, check.want)
		}
	}

	child.Release()
	parent.Release()
}

// TestLiveForkRunningKernelRepeatedly verifies that restarting the parent after
// one fork leaves all save-time services quiesceable for a later fork.
func TestLiveForkRunningKernelRepeatedly(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	parent.Pause()

	first, err := parent.LiveFork(parent.SupervisorContext(), kernel.LiveForkOptions{
		Clocks: sentrytime.NewCalibratedClocks(),
	})
	if err != nil {
		t.Fatalf("first LiveFork: %v", err)
	}
	if err := parent.StartPaused(); err != nil {
		t.Fatalf("parent.StartPaused: %v", err)
	}
	if err := first.StartPaused(); err != nil {
		t.Fatalf("first.StartPaused: %v", err)
	}
	parent.Unpause()
	first.Unpause()

	parent.Pause()
	second, err := parent.LiveFork(parent.SupervisorContext(), kernel.LiveForkOptions{
		Clocks: sentrytime.NewCalibratedClocks(),
	})
	if err != nil {
		t.Fatalf("second LiveFork: %v", err)
	}
	if err := second.StartPaused(); err != nil {
		t.Fatalf("second.StartPaused: %v", err)
	}
	parent.Unpause()
	second.Unpause()

	parent.Pause()
	first.Pause()
	second.Pause()
	second.Release()
	first.Release()
	parent.Release()
}
