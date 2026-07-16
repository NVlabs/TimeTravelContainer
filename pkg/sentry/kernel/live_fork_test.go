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
	"testing"

	"gvisor.dev/gvisor/pkg/sentry/fsimpl/testutil"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	sentrytime "gvisor.dev/gvisor/pkg/sentry/time"
)

func TestLiveForkTwoKernelsSharePlatformAndMemoryFile(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	parent.Pause()

	child, err := parent.LiveFork(parent.SupervisorContext(), kernel.LiveForkOptions{
		Clocks: sentrytime.NewCalibratedClocks(),
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
