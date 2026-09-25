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

package cmd

import (
	"reflect"
	"testing"
)

func TestRunscCommandUsesEmbeddedRuntime(t *testing.T) {
	command := runscCommand("/opt/ttc/ttcd", "--root", "/tmp/root", "list")
	want := []string{"/opt/ttc/ttcd", "runsc", "--root", "/tmp/root", "list"}
	if !reflect.DeepEqual(command.Args, want) {
		t.Fatalf("runscCommand(ttcd).Args = %q, want %q", command.Args, want)
	}
}

func TestRunscCommandKeepsExplicitDevelopmentRuntime(t *testing.T) {
	command := runscCommand("/tmp/fake-runsc", "list")
	want := []string{"/tmp/fake-runsc", "list"}
	if !reflect.DeepEqual(command.Args, want) {
		t.Fatalf("runscCommand(fake-runsc).Args = %q, want %q", command.Args, want)
	}
}

func TestEmbeddedRuntimeQueryAnswersOnlyReadOnlyQueries(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][]string{
		{"list"},
		{"list", "--format=json"},
		{"state", "family"},
		{"delete", "--force", "family"},
		{"timetravel", "family", "pause", "main"},
		{"timetravel", "--signal", "15", "family", "signal", "main"},
		{"timetravel", "family", "ps"},
		{"timetravel", "family", "branches", "main"},
	} {
		if output, answered, err := embeddedRuntimeQuery(root, args); answered {
			t.Errorf("embeddedRuntimeQuery(%q) = %q, %v, answered; want it left to the runtime", args, output, err)
		}
	}
	output, answered, err := embeddedRuntimeQuery(root, []string{"list", "--format=json", "--sandbox"})
	if !answered || err != nil || string(output) != "null" {
		t.Fatalf("listing an empty root = %q, %t, %v; want null, answered", output, answered, err)
	}
	if _, answered, err := embeddedRuntimeQuery(root, []string{"timetravel", "missing", "branches"}); !answered || err == nil {
		t.Fatalf("branches of a missing sandbox: answered %t, err %v; want an error", answered, err)
	}
}
