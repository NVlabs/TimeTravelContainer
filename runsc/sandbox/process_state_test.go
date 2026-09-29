// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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
	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(truePath)
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
