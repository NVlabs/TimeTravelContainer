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

package sandbox

import (
	"os/exec"
	"testing"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/runsc/specutils"
)

func TestSandboxProcessArgv0(t *testing.T) {
	spec := &specs.Spec{Annotations: map[string]string{
		specutils.ContainerdContainerNameAnnotation: "web app",
	}}
	if got, want := sandboxProcessArgv0(spec, "ignored"), "ttc-sentry[web_app@family]"; got != want {
		t.Fatalf("sandboxProcessArgv0() = %q, want %q", got, want)
	}
}

func TestIsProcessRunningRejectsZombie(t *testing.T) {
	command := exec.Command("/bin/true")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer command.Wait()
	pid := command.Process.Pid
	deadline := time.Now().Add(5 * time.Second)
	for IsProcessRunning(pid) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if IsProcessRunning(pid) {
		t.Fatalf("PID %d remained running after exit", pid)
	}
}
