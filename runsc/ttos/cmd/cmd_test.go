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
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSelector(t *testing.T) {
	tests := []struct {
		input string
		want  selector
		err   bool
	}{
		{input: "web", want: selector{container: "web", branch: "main"}},
		{input: "web@test-a", want: selector{container: "web", branch: "test-a"}},
		{input: "", err: true},
		{input: "@child", err: true},
		{input: "web@", err: true},
		{input: "web@child@again", err: true},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			got, err := parseSelector(test.input)
			if (err != nil) != test.err {
				t.Fatalf("parseSelector(%q) error: %v, want error=%t", test.input, err, test.err)
			}
			if got != test.want {
				t.Errorf("parseSelector(%q) = %+v, want %+v", test.input, got, test.want)
			}
		})
	}
}

func TestWriteTree(t *testing.T) {
	families := []Family{{
		ID:    "web",
		State: "running",
		Branches: []Branch{
			{ID: "retry", ParentID: "test-a", State: "running", ForkTotalNS: 2_000_000, SourceDowntimeNS: 1_000_000},
			{ID: "test-b", ParentID: "main", State: "paused"},
			{ID: "main", State: "running"},
			{ID: "test-a", ParentID: "main", State: "paused"},
			{ID: "orphan", ParentID: "deleted", State: "running"},
		},
	}}
	var out bytes.Buffer
	writeTree(&out, families)
	want := "web (running)\n" +
		"├── main running\n" +
		"│   ├── test-a paused\n" +
		"│   │   └── retry running fork=2ms downtime=1ms\n" +
		"│   └── test-b paused\n" +
		"└── orphan running parent=deleted (deleted)\n"
	if got := out.String(); got != want {
		t.Errorf("writeTree output:\n%s\nwant:\n%s", got, want)
	}
}

func TestCommandsUseQualifiedBranches(t *testing.T) {
	b := &fakeBackend{families: []Family{{ID: "web", State: "running", Branches: []Branch{{ID: "main", State: "running"}}}}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"fork", "--paused", "web@main", "web@test-a"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("fork status=%d, stderr=%q", status, stderr.String())
	}
	if got := stdout.String(); got != "web@test-a\n" {
		t.Errorf("fork output=%q, want %q", got, "web@test-a\n")
	}
	if b.forkSource != (selector{container: "web", branch: "main"}) || b.forkDestination != "test-a" || !b.forkPaused {
		t.Errorf("fork call = source %+v destination %q paused=%t", b.forkSource, b.forkDestination, b.forkPaused)
	}

	stdout.Reset()
	if status := runCommand([]string{"pause", "web@test-a"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("pause status=%d, stderr=%q", status, stderr.String())
	}
	if b.paused != (selector{container: "web", branch: "test-a"}) {
		t.Errorf("pause target=%+v", b.paused)
	}

	stdout.Reset()
	if status := runCommand([]string{"resume", "web@test-a"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("resume status=%d, stderr=%q", status, stderr.String())
	}
	if b.resumed != (selector{container: "web", branch: "test-a"}) {
		t.Errorf("resume target=%+v", b.resumed)
	}

	stdout.Reset()
	if status := runCommand([]string{"branch", "rm", "web@test-a"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("branch rm status=%d, stderr=%q", status, stderr.String())
	}
	if b.destroyed != (selector{container: "web", branch: "test-a"}) {
		t.Errorf("destroy target=%+v", b.destroyed)
	}
}

func TestForkGeneratesDestination(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"fork", "web"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("fork status=%d, stderr=%q", status, stderr.String())
	}
	if !strings.HasPrefix(b.forkDestination, "fork-") {
		t.Errorf("generated destination %q does not start with fork-", b.forkDestination)
	}
	if got := stdout.String(); got != "web@"+b.forkDestination+"\n" {
		t.Errorf("fork output=%q, want %q", got, "web@"+b.forkDestination+"\n")
	}
}

func TestForkRejectsCrossFamilyDestination(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	status := runCommand([]string{"fork", "web", "other@child"}, &stdout, &stderr, b)
	if status != 1 {
		t.Fatalf("status=%d, want 1", status)
	}
	if !strings.Contains(stderr.String(), "must remain in the source container family") {
		t.Errorf("stderr=%q", stderr.String())
	}
}

func TestBranchRMRejectsMain(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	status := runCommand([]string{"branch", "rm", "web"}, &stdout, &stderr, b)
	if status != 1 {
		t.Fatalf("status=%d, want 1", status)
	}
	if b.destroyed.container != "" {
		t.Errorf("destroy unexpectedly called for %+v", b.destroyed)
	}
}

func TestListJSON(t *testing.T) {
	b := &fakeBackend{families: []Family{{ID: "web", State: "running", Branches: []Branch{{ID: "main", State: "running"}}}}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"ls", "--json"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("status=%d, stderr=%q", status, stderr.String())
	}
	want := `"id": "web"`
	if !strings.Contains(stdout.String(), want) {
		t.Errorf("JSON output %q does not contain %q", stdout.String(), want)
	}
}

func TestRuntimeBackendUsesRunscCLI(t *testing.T) {
	dir := t.TempDir()
	runsc := filepath.Join(dir, "runsc")
	script := `#!/bin/sh
if [ "$1" != "--root" ] || [ "$2" != "/runtime" ]; then
	echo "unexpected globals: $*" >&2
	exit 2
fi
if [ "$3" = "list" ]; then
	echo '[{"id":"web","status":"running"}]'
	exit 0
fi
if [ "$3" = "timetravel" ] && [ "$4" = "web" ] && [ "$5" = "branches" ]; then
	echo '[{"ID":"main","ParentID":"","State":"running","ForkMetrics":{"SourceDowntime":0,"Kernel":{"Total":0},"Total":0}}]'
	exit 0
fi
if [ "$3" = "timetravel" ] && [ "$4" = "--source" ] && [ "$5" = "main" ] && [ "$6" = "--paused" ] && [ "$7" = "web" ] && [ "$8" = "fork" ] && [ "$9" = "child" ]; then
	echo '{"ID":"child","ParentID":"main","State":"paused","ForkMetrics":{"SourceDowntime":1000000,"Kernel":{"Total":2000000},"Total":3000000}}'
	exit 0
fi
echo "unexpected command: $*" >&2
exit 2
`
	if err := os.WriteFile(runsc, []byte(script), 0755); err != nil {
		t.Fatalf("writing fake runsc: %v", err)
	}
	b := &runtimeBackend{root: "/runtime", runsc: runsc}
	families, err := b.listFamilies()
	if err != nil {
		t.Fatalf("listFamilies: %v", err)
	}
	if len(families) != 1 || families[0].ID != "web" || len(families[0].Branches) != 1 || families[0].Branches[0].ID != "main" {
		t.Fatalf("listFamilies returned %+v", families)
	}
	child, err := b.fork(selector{container: "web", branch: "main"}, "child", true)
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if child.ID != "child" || child.ParentID != "main" || child.State != "paused" || child.ForkTotalNS != 3_000_000 || child.KernelForkNS != 2_000_000 {
		t.Errorf("fork returned %+v", child)
	}
}

type fakeBackend struct {
	families        []Family
	err             error
	forkSource      selector
	forkDestination string
	forkPaused      bool
	paused          selector
	resumed         selector
	destroyed       selector
}

func (b *fakeBackend) listFamilies() ([]Family, error) {
	return b.families, b.err
}

func (b *fakeBackend) fork(source selector, destination string, paused bool) (Branch, error) {
	b.forkSource = source
	b.forkDestination = destination
	b.forkPaused = paused
	if b.err != nil {
		return Branch{}, b.err
	}
	return Branch{ID: destination, ParentID: source.branch, State: "paused"}, nil
}

func (b *fakeBackend) pause(target selector) error {
	b.paused = target
	return b.err
}

func (b *fakeBackend) resume(target selector) error {
	b.resumed = target
	return b.err
}

func (b *fakeBackend) destroy(target selector) error {
	b.destroyed = target
	return b.err
}
