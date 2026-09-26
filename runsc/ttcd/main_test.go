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

package main

import (
	"os/exec"
	"slices"
	"testing"
)

func TestEmbeddedRuntimeArgv0(t *testing.T) {
	for _, test := range []struct {
		argv0 string
		want  bool
	}{
		{"/usr/local/bin/runsc", true},
		{"runsc-gofer", true},
		{"runsc-exec", true},
		{"ttc-sentry[demo@family]", true},
		{"/usr/local/bin/ttcd", false},
		{"ttcd-runsc", false},
		{"unrelated-runsc-gofer", false},
	} {
		if got := embeddedRuntimeArgv0(test.argv0); got != test.want {
			t.Errorf("embeddedRuntimeArgv0(%q) = %t, want %t", test.argv0, got, test.want)
		}
	}
}

func TestInheritedChildren(t *testing.T) {
	child := exec.Command("sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatalf("starting a child: %v", err)
	}
	defer func() {
		child.Process.Kill()
		child.Wait()
	}()
	if children := inheritedChildren(); !slices.Contains(children, child.Process.Pid) {
		t.Errorf("inheritedChildren() = %v, want it to hold %d", children, child.Process.Pid)
	}
}
