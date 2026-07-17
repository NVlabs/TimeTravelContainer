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
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	ttosbundle "gvisor.dev/gvisor/runsc/ttos/bundle"
	ttosconsole "gvisor.dev/gvisor/runsc/ttos/console"
	ttosimages "gvisor.dev/gvisor/runsc/ttos/images"
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

func TestPsFiltersAndUsesQualifiedSelectors(t *testing.T) {
	b := &fakeBackend{families: []Family{
		{ID: "web", State: "running", Branches: []Branch{
			{ID: "main", State: "running"},
			{ID: "child", ParentID: "main", State: "paused"},
			{ID: "old", ParentID: "main", State: "exited"},
		}},
		{ID: "prepared", State: "created", Branches: []Branch{{ID: "main", State: "created"}}},
	}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"ps", "--quiet"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("ps status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := stdout.String(), "web@main\nweb@child\n"; got != want {
		t.Errorf("ps --quiet=%q, want %q", got, want)
	}
	stdout.Reset()
	if status := runCommand([]string{"ps", "--all", "--quiet"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("ps --all status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := stdout.String(), "web@main\nweb@child\nweb@old\nprepared@main\n"; got != want {
		t.Errorf("ps --all --quiet=%q, want %q", got, want)
	}
}

func TestPsDockerFiltersLatestAndFormatting(t *testing.T) {
	base := time.Date(2026, time.July, 17, 0, 0, 0, 0, time.UTC)
	digest := "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	b := &fakeBackend{families: []Family{
		{
			ID: "web", Image: "docker.io/library/alpine:3.20", Digest: digest,
			CreatedAt: base, Volumes: []string{"cache"},
			Branches: []Branch{
				{ID: "main", State: "running", CreatedAt: base},
				{ID: "child", ParentID: "main", State: "paused", CreatedAt: base.Add(2 * time.Hour)},
				{ID: "old", ParentID: "main", State: "exited", CreatedAt: base.Add(time.Hour)},
			},
		},
		{
			ID: "newest", Image: "docker.io/library/busybox:1.36",
			CreatedAt: base.Add(3 * time.Hour),
			Branches:  []Branch{{ID: "main", State: "created", CreatedAt: base.Add(3 * time.Hour)}},
		},
	}}
	var stdout, stderr bytes.Buffer
	args := []string{
		"ps", "--all",
		"--filter", "status=paused",
		"--filter", "ancestor=alpine:3.20",
		"--filter", "volume=cache",
		"--filter", "name=web@child",
		"--format", "{{.ID}}|{{.Image}}|{{.State}}|{{.ImageID}}",
	}
	if status := runCommand(args, &stdout, &stderr, b); status != 0 {
		t.Fatalf("filtered ps status=%d, stderr=%q", status, stderr.String())
	}
	want := "web@child|alpine:3.20|paused|0123456789ab\n"
	if got := stdout.String(); got != want {
		t.Errorf("filtered ps=%q, want %q", got, want)
	}

	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"ps", "--latest", "--quiet"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("ps --latest status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := stdout.String(), "newest@main\n"; got != want {
		t.Errorf("ps --latest=%q, want %q", got, want)
	}

	stdout.Reset()
	if status := runCommand([]string{"ps", "--last", "2", "--quiet"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("ps --last status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := stdout.String(), "newest@main\nweb@child\n"; got != want {
		t.Errorf("ps --last 2=%q, want %q", got, want)
	}

	stdout.Reset()
	if status := runCommand([]string{"ps", "--all", "--filter", "branch=child", "--format", "table {{.Names}}\t{{.Status}}"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("formatted ps status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := stdout.String(), "NAMES\tSTATUS\nweb@child\tPaused\n"; got != want {
		t.Errorf("formatted ps=%q, want %q", got, want)
	}
}

func TestPsJSONAndNoTruncImageID(t *testing.T) {
	containerID := strings.Repeat("d", 64)
	created := time.Date(2026, time.July, 17, 1, 2, 3, 0, time.UTC)
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	b := &fakeBackend{families: []Family{{
		ID: containerID, Name: "demo", Image: "docker.io/example/demo:v1", Digest: digest, CreatedAt: created,
		Branches: []Branch{{ID: "main", State: "running", CreatedAt: created}},
	}}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"ps", "--format", "json", "--no-trunc"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("ps json status=%d, stderr=%q", status, stderr.String())
	}
	var row map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &row); err != nil {
		t.Fatalf("decoding ps JSON %q: %v", stdout.String(), err)
	}
	if row["id"] != containerID+"@main" || row["names"] != "demo@main" || row["container"] != "demo" || row["image"] != "example/demo:v1" || row["image_id"] != digest {
		t.Errorf("ps JSON row=%+v", row)
	}
	if row["created_at"] != created.Format(time.RFC3339Nano) {
		t.Errorf("created_at=%v, want %s", row["created_at"], created.Format(time.RFC3339Nano))
	}
}

func TestPsRejectsInvalidFiltersAndSelection(t *testing.T) {
	tests := [][]string{
		{"ps", "--filter", "missing-equals"},
		{"ps", "--filter", "unknown=value"},
		{"ps", "--filter", "name=["},
		{"ps", "--last", "-2"},
		{"ps", "--latest", "--last", "2"},
	}
	for _, args := range tests {
		b := &fakeBackend{}
		var stdout, stderr bytes.Buffer
		if status := runCommand(args, &stdout, &stderr, b); status != 1 {
			t.Errorf("%v status=%d, want 1", args, status)
		}
		if b.families == nil && stdout.Len() != 0 {
			t.Errorf("%v unexpectedly wrote %q", args, stdout.String())
		}
	}
}

func TestTopTableAndJSON(t *testing.T) {
	b := &fakeBackend{processList: []ProcessInfo{{
		UID: 1000, PID: 7, PPID: 1, PGID: 7, Threads: []int32{7, 8},
		C: 3, TTY: "?", STime: "12:34", Time: "9ms", Cmd: "worker",
	}}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"top", "web@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("top status=%d, stderr=%q", status, stderr.String())
	}
	if b.processTarget != (selector{container: "web", branch: "child"}) {
		t.Errorf("top target=%+v", b.processTarget)
	}
	for _, text := range []string{"UID", "PID", "CMD", "1000", "worker"} {
		if !strings.Contains(stdout.String(), text) {
			t.Errorf("top output %q does not contain %q", stdout.String(), text)
		}
	}
	stdout.Reset()
	if status := runCommand([]string{"top", "--json", "web@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("top --json status=%d, stderr=%q", status, stderr.String())
	}
	var got []ProcessInfo
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil || !reflect.DeepEqual(got, b.processList) {
		t.Errorf("top JSON=%+v err=%v, want %+v", got, err, b.processList)
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

func TestForkCheckReportsEligibilityWithoutForking(t *testing.T) {
	b := &fakeBackend{checkResult: ForkEligibility{Eligible: true}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"fork", "--check", "web@main"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("eligible check status=%d, stderr=%q", status, stderr.String())
	}
	if b.checkTarget != (selector{container: "web", branch: "main"}) || b.forkDestination != "" {
		t.Errorf("check target=%+v fork destination=%q", b.checkTarget, b.forkDestination)
	}
	if got, want := stdout.String(), "web@main eligible\n"; got != want {
		t.Errorf("eligible output=%q, want %q", got, want)
	}

	b.checkResult = ForkEligibility{
		Reason: "live fork resource validation failed",
		Issues: []ForkResourceIssue{{PID: 1, FD: -1, Resource: "aioMappable", Reason: "unsupported mapping"}},
	}
	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"fork", "--check", "web"}, &stdout, &stderr, b); status != 1 {
		t.Fatalf("ineligible check status=%d, stderr=%q", status, stderr.String())
	}
	for _, text := range []string{"web@main ineligible", "reason:", "mapping", "aioMappable", "unsupported mapping"} {
		if !strings.Contains(stdout.String(), text) {
			t.Errorf("ineligible output %q does not contain %q", stdout.String(), text)
		}
	}

	stdout.Reset()
	if status := runCommand([]string{"fork", "--check", "--json", "web"}, &stdout, &stderr, b); status != 1 {
		t.Fatalf("JSON ineligible check status=%d, stderr=%q", status, stderr.String())
	}
	var got ForkEligibility
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil || !reflect.DeepEqual(got, b.checkResult) {
		t.Errorf("eligibility JSON=%+v err=%v, want %+v", got, err, b.checkResult)
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

func TestPullNormalizesDockerReference(t *testing.T) {
	b := &fakeBackend{pullInfo: ttosimages.Info{
		Reference: "docker.io/library/ubuntu:latest",
		Digest:    "sha256:1234",
		Platform:  "linux/amd64",
	}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"pull", "ubuntu"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("pull status=%d, stderr=%q", status, stderr.String())
	}
	if b.pullReference != "docker.io/library/ubuntu:latest" || b.pullSnapshotter != "native" || b.pullUnpack {
		t.Errorf("pull call: reference=%q snapshotter=%q unpack=%t", b.pullReference, b.pullSnapshotter, b.pullUnpack)
	}
	want := "Pulled docker.io/library/ubuntu:latest\nDigest: sha256:1234\nPlatform: linux/amd64\n"
	if got := stdout.String(); got != want {
		t.Errorf("pull output=%q, want %q", got, want)
	}
}

func TestPullOptionsAndQuietOutput(t *testing.T) {
	b := &fakeBackend{pullInfo: ttosimages.Info{Digest: "sha256:5678"}}
	var stdout, stderr bytes.Buffer
	args := []string{"pull", "--platform", "linux/arm64", "--snapshotter", "overlayfs", "--unpack=false", "-q", "example/app:v1"}
	if status := runCommand(args, &stdout, &stderr, b); status != 0 {
		t.Fatalf("pull status=%d, stderr=%q", status, stderr.String())
	}
	if b.pullReference != "docker.io/example/app:v1" || b.pullPlatform != "linux/arm64" || b.pullSnapshotter != "overlayfs" || b.pullUnpack {
		t.Errorf("pull call: reference=%q platform=%q snapshotter=%q unpack=%t", b.pullReference, b.pullPlatform, b.pullSnapshotter, b.pullUnpack)
	}
	if got := stdout.String(); got != "sha256:5678\n" {
		t.Errorf("quiet pull output=%q", got)
	}
}

func TestImagesDockerStyleTableAndQuietAliases(t *testing.T) {
	digest := "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	b := &fakeBackend{imageList: []ImageInfo{
		{
			Reference:    "docker.io/library/busybox:latest",
			Digest:       digest,
			Size:         2_150_000,
			Platform:     "linux/amd64",
			Materialized: true,
			CreatedAt:    time.Now().Add(-2 * time.Hour),
		},
		{
			Reference: "docker.io/library/busybox:stable",
			Digest:    digest,
			Size:      2_150_000,
			Platform:  "linux/amd64",
			CreatedAt: time.Now().Add(-2 * time.Hour),
		},
	}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"images"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("images status=%d, stderr=%q", status, stderr.String())
	}
	for _, want := range []string{"REPOSITORY", "TAG", "IMAGE ID", "PLATFORM", "ROOTFS", "UNPACKED", "busybox", "latest", "stable", "0123456789ab", "linux/amd64", "2.15MB", "2 hours ago"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("images output %q does not contain %q", stdout.String(), want)
		}
	}
	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"image", "ls", "-q"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("image ls -q status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := stdout.String(), "0123456789ab\n"; got != want {
		t.Errorf("image ls -q=%q, want %q", got, want)
	}
	stdout.Reset()
	if status := runCommand([]string{"images", "-q", "--no-trunc"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("images --no-trunc status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := stdout.String(), digest+"\n"; got != want {
		t.Errorf("images --no-trunc=%q, want %q", got, want)
	}
}

func TestImageInspectAggregatesAliasesAndTTOSState(t *testing.T) {
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	created := time.Date(2026, time.July, 1, 2, 3, 4, 0, time.UTC)
	infos := []ImageInfo{
		{
			Reference:  "docker.io/library/busybox:latest",
			Digest:     digest,
			MediaType:  "application/vnd.oci.image.index.v1+json",
			Size:       1234,
			Platform:   "linux/arm64/v8",
			Containers: []string{"zeta", "alpha"},
			CreatedAt:  created,
		},
		{
			Reference:    "docker.io/library/busybox:stable",
			Digest:       digest,
			Platform:     "linux/arm64/v8",
			Materialized: true,
			Unpacked:     true,
		},
	}
	got, err := inspectImageFromInventory(infos, "busybox")
	if err != nil {
		t.Fatalf("inspectImageFromInventory: %v", err)
	}
	if got.ID != digest || got.OS != "linux" || got.Architecture != "arm64" || got.Variant != "v8" || !got.Materialized || !got.Unpacked {
		t.Errorf("inspect info=%+v", got)
	}
	if want := []string{"busybox:latest", "busybox:stable"}; !reflect.DeepEqual(got.RepoTags, want) {
		t.Errorf("RepoTags=%q, want %q", got.RepoTags, want)
	}
	if want := []string{"busybox@" + digest}; !reflect.DeepEqual(got.RepoDigests, want) {
		t.Errorf("RepoDigests=%q, want %q", got.RepoDigests, want)
	}
	if want := []string{"alpha", "zeta"}; !reflect.DeepEqual(got.Containers, want) {
		t.Errorf("Containers=%q, want %q", got.Containers, want)
	}
}

func TestImageInspectAndRemoveCommandRouting(t *testing.T) {
	digest := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	b := &fakeBackend{
		imageInspectInfo: ImageInspectInfo{ID: digest, RepoTags: []string{"alpine:latest"}},
		imageRemoveResult: ImageRemoveResult{
			Untagged: []string{"alpine:latest"},
			Deleted:  digest,
		},
	}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"image", "inspect", "alpine"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("image inspect status=%d, stderr=%q", status, stderr.String())
	}
	if b.imageInspectID != "alpine" || !strings.Contains(stdout.String(), `"Id": "`+digest+`"`) {
		t.Errorf("image inspect id=%q output=%q", b.imageInspectID, stdout.String())
	}
	stdout.Reset()
	if status := runCommand([]string{"rmi", "--force", "alpine"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("rmi status=%d, stderr=%q", status, stderr.String())
	}
	if b.imageRemoveID != "alpine" || !b.imageRemoveForce {
		t.Errorf("remove call id=%q force=%t", b.imageRemoveID, b.imageRemoveForce)
	}
	want := "Untagged: alpine:latest\nDeleted: " + digest + "\n"
	if got := stdout.String(); got != want {
		t.Errorf("rmi output=%q, want %q", got, want)
	}
}

func TestValidateImageRemovalRequiresForceWhenInUse(t *testing.T) {
	digest := "sha256:abcd"
	users := []string{"alpha", "zeta"}
	if err := validateImageRemoval(digest, users, false); err == nil || !strings.Contains(err.Error(), "alpha, zeta") {
		t.Fatalf("validateImageRemoval without force=%v, want named conflict", err)
	}
	if err := validateImageRemoval(digest, users, true); err != nil {
		t.Errorf("validateImageRemoval with force: %v", err)
	}
	if err := validateImageRemoval(digest, nil, false); err != nil {
		t.Errorf("validateImageRemoval unused: %v", err)
	}
}

func TestRunParsesDockerStyleOptions(t *testing.T) {
	containerID := strings.Repeat("a", 64)
	b := &fakeBackend{containerID: containerID}
	var stdout, stderr bytes.Buffer
	args := []string{"run", "--name", "demo", "-dit", "--rm", "--read-only", "--stop-signal", "SIGUSR2", "--stop-timeout", "7", "--pids-limit", "23", "--pull", "never", "--network", "host", "-e", "A=1", "--workdir", "/work", "--user", "123:456", "--entrypoint", "/entry", "--log-opt", "max-size=1.5m", "--log-opt", "max-file=4", "-v", "/host/a:/host-data:ro", "--mount", "type=bind,src=/host/b,dst=/config,readonly", "-v", "cache:/cache", "--mount", "type=volume,source=data,target=/data,readonly", "--tmpfs", "/scratch:mode=0700,size=1m", "--mount", "type=tmpfs,target=/worktmp,tmpfs-size=2097152,tmpfs-mode=0750", "--mount", "type=proc,target=/alt-proc,readonly", "--mount", "type=sysfs,target=/alt-sys", "busybox:1.36", "echo", "ok"}
	if status := runCommand(args, &stdout, &stderr, b); status != 0 {
		t.Fatalf("run status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := stdout.String(), containerID+"\n"; got != want {
		t.Errorf("run output=%q, want %q", got, want)
	}
	if b.runRequest.name != "demo" || b.runRequest.reference != "busybox:1.36" || !b.runRequest.detach || !b.runRequest.interactive || !b.runRequest.terminal || !b.runRequest.autoRemove || !b.runRequest.readonlyRootfs || b.runRequest.cwd != "/work" || b.runRequest.user != "123:456" || b.runRequest.networkMode != "host" || b.runRequest.pullPolicy != "never" || b.runRequest.stopSignal != "SIGUSR2" || b.runRequest.stopTimeout != 7 {
		t.Errorf("run request=%+v", b.runRequest)
	}
	if b.runRequest.pidsLimit == nil || *b.runRequest.pidsLimit != 23 {
		t.Errorf("pids limit=%v, want 23", b.runRequest.pidsLimit)
	}
	if got, want := strings.Join(b.runRequest.args, " "), "echo ok"; got != want {
		t.Errorf("run args=%q, want %q", got, want)
	}
	if got, want := strings.Join(b.runRequest.entrypoint, " "), "/entry"; got != want {
		t.Errorf("entrypoint=%q, want %q", got, want)
	}
	wantMounts := []ttosbundle.HostMount{
		{Source: "/host/a", Destination: "/host-data", Readonly: true},
		{Source: "/host/b", Destination: "/config", Readonly: true},
	}
	if !reflect.DeepEqual(b.runRequest.hostMounts, wantMounts) {
		t.Errorf("host mounts=%+v, want %+v", b.runRequest.hostMounts, wantMounts)
	}
	wantVolumes := []ttosbundle.ManagedVolume{
		{Name: "cache", Destination: "/cache"},
		{Name: "data", Destination: "/data", Readonly: true},
	}
	if !reflect.DeepEqual(b.runRequest.managedVolumes, wantVolumes) {
		t.Errorf("managed volumes=%+v, want %+v", b.runRequest.managedVolumes, wantVolumes)
	}
	wantFilesystems := []ttosbundle.FilesystemMount{
		{Type: "tmpfs", Destination: "/scratch", Options: []string{"rw", "mode=0700", "size=1m"}, HostConfig: true, InspectOptions: "mode=0700,size=1m"},
		{Type: "tmpfs", Destination: "/worktmp", Options: []string{"rw", "size=2097152", "mode=0750"}},
		{Type: "proc", Destination: "/alt-proc", Readonly: true, Options: []string{"ro", "nosuid", "nodev", "noexec"}},
		{Type: "sysfs", Destination: "/alt-sys", Readonly: true, Options: []string{"ro", "nosuid", "nodev", "noexec"}},
	}
	if !reflect.DeepEqual(b.runRequest.filesystemMounts, wantFilesystems) {
		t.Errorf("filesystem mounts=%+v, want %+v", b.runRequest.filesystemMounts, wantFilesystems)
	}
	if b.runRequest.consoleLogMaxBytes != 1572864 || b.runRequest.consoleLogMaxFiles != 4 {
		t.Errorf("log retention=(%d, %d), want (1572864, 4)", b.runRequest.consoleLogMaxBytes, b.runRequest.consoleLogMaxFiles)
	}
}

func TestParseTmpfsMountDockerOptions(t *testing.T) {
	const value = "/data:ro,suid,dev,noexec,sync,dirsync,noatime,nodiratime,size=64m,mode=1770,uid=123,gid=456,nr_inodes=400k,nr_blocks=1024"
	got, err := parseTmpfsMount(value)
	if err != nil {
		t.Fatalf("parseTmpfsMount: %v", err)
	}
	want := ttosbundle.FilesystemMount{
		Type: "tmpfs", Destination: "/data", Readonly: true,
		Options:    []string{"ro", "suid", "dev", "noexec", "sync", "dirsync", "noatime", "nodiratime", "size=64m", "mode=1770", "uid=123", "gid=456", "nr_inodes=400k", "nr_blocks=1024"},
		HostConfig: true, InspectOptions: strings.TrimPrefix(value, "/data:"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mount=%+v, want %+v", got, want)
	}
	lastWins, err := parseTmpfsMount("/data:ro,rw")
	if err != nil {
		t.Fatalf("last access mode: %v", err)
	}
	if lastWins.Readonly || !reflect.DeepEqual(lastWins.Options, []string{"rw"}) {
		t.Errorf("last access mode did not win: %+v", lastWins)
	}
}

func TestRunAndCreateLoadEnvironmentAndLabels(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "container.env")
	const absent = "TTOS_TEST_ABSENT_ENV_7281"
	if _, ok := os.LookupEnv(absent); ok {
		t.Fatalf("test environment unexpectedly contains %s", absent)
	}
	if err := os.WriteFile(envFile, []byte("# from a file\nFROM_FILE=file\nHOST_IMPORT\n"+absent+"\n\n"), 0600); err != nil {
		t.Fatal(err)
	}
	labelFile := filepath.Join(dir, "container.labels")
	if err := os.WriteFile(labelFile, []byte("# metadata\nsuite=file\nfile-only=value\nempty\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOST_IMPORT", "host-value")
	t.Setenv("DIRECT_IMPORT", "direct-host")

	for _, command := range []string{"run", "create"} {
		t.Run(command, func(t *testing.T) {
			b := &fakeBackend{}
			var stdout, stderr bytes.Buffer
			args := []string{
				command,
				"--env-file", envFile,
				"--env", "FROM_FILE=direct",
				"-e", "DIRECT=value",
				"-e", "DIRECT_IMPORT",
				"-e", absent,
				"--label-file", labelFile,
				"--label", "suite=direct",
				"-l", "direct-only=value",
				"--name", "demo",
				"--pids-limit", "17",
				"busybox:1.36",
			}
			if status := runCommand(args, &stdout, &stderr, b); status != 0 {
				t.Fatalf("%s status=%d, stderr=%q", command, status, stderr.String())
			}
			wantEnv := []string{
				"FROM_FILE=file",
				"HOST_IMPORT=host-value",
				absent,
				"FROM_FILE=direct",
				"DIRECT=value",
				"DIRECT_IMPORT=direct-host",
				absent,
			}
			if !reflect.DeepEqual(b.runRequest.env, wantEnv) {
				t.Errorf("environment=%q, want %q", b.runRequest.env, wantEnv)
			}
			wantLabels := map[string]string{
				"suite":       "direct",
				"file-only":   "value",
				"empty":       "",
				"direct-only": "value",
			}
			if !reflect.DeepEqual(b.runRequest.labels, wantLabels) {
				t.Errorf("labels=%q, want %q", b.runRequest.labels, wantLabels)
			}
			if b.runRequest.pidsLimit == nil || *b.runRequest.pidsLimit != 17 {
				t.Errorf("pids limit=%v, want 17", b.runRequest.pidsLimit)
			}
			wantOutput := ""
			if command == "create" {
				wantOutput = "demo\n"
			}
			if got := stdout.String(); got != wantOutput {
				t.Errorf("output=%q, want %q", got, wantOutput)
			}
			if got := b.created; got != (command == "create") {
				t.Errorf("created=%t for %s", got, command)
			}
		})
	}
}

func TestRunAndCreateRejectInvalidEnvironmentAndLabelFiles(t *testing.T) {
	dir := t.TempDir()
	invalidEnv := filepath.Join(dir, "invalid.env")
	if err := os.WriteFile(invalidEnv, []byte(" =value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	invalidLabels := filepath.Join(dir, "invalid.labels")
	if err := os.WriteFile(invalidLabels, []byte("=value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"run", "--env-file", filepath.Join(dir, "missing"), "busybox"},
		{"run", "--env-file", invalidEnv, "busybox"},
		{"create", "--label-file", invalidLabels, "busybox"},
	} {
		b := &fakeBackend{}
		var stdout, stderr bytes.Buffer
		if status := runCommand(args, &stdout, &stderr, b); status != 1 {
			t.Errorf("runCommand(%q) status=%d, want 1", args, status)
		}
		if b.runRequest.reference != "" {
			t.Errorf("runCommand(%q) reached backend with %+v", args, b.runRequest)
		}
	}
}

func TestRunAndCreateRejectInvalidPullPolicy(t *testing.T) {
	for _, command := range []string{"run", "create"} {
		b := &fakeBackend{}
		var stdout, stderr bytes.Buffer
		if status := runCommand([]string{command, "--pull", "sometimes", "busybox"}, &stdout, &stderr, b); status != 1 {
			t.Errorf("%s invalid pull policy status=%d, want 1", command, status)
		}
		if b.runRequest.reference != "" {
			t.Errorf("%s invalid pull policy invoked backend with %+v", command, b.runRequest)
		}
	}
}

func TestRunAndCreateRejectInvalidStopPolicy(t *testing.T) {
	for _, args := range [][]string{
		{"run", "--stop-signal", "SIGINVALID", "busybox"},
		{"create", "--stop-timeout", "-2", "busybox"},
		{"run", "--pids-limit", "-2", "busybox"},
	} {
		b := &fakeBackend{}
		var stdout, stderr bytes.Buffer
		if status := runCommand(args, &stdout, &stderr, b); status != 1 {
			t.Errorf("runCommand(%q) status=%d, want 1", args, status)
		}
		if b.runRequest.reference != "" {
			t.Errorf("runCommand(%q) invoked backend with %+v", args, b.runRequest)
		}
	}
}

func TestRunAndCreateRejectUnsupportedNetwork(t *testing.T) {
	for _, args := range [][]string{
		{"run", "--network", "bridge", "busybox"},
		{"create", "--net", "sandbox", "busybox"},
	} {
		b := &fakeBackend{}
		var stdout, stderr bytes.Buffer
		if status := runCommand(args, &stdout, &stderr, b); status != 1 {
			t.Errorf("runCommand(%q) status=%d, want 1", args, status)
		}
		if b.runRequest.reference != "" {
			t.Errorf("runCommand(%q) invoked backend with %+v", args, b.runRequest)
		}
	}
}

func TestRunRejectsInvalidLogOptions(t *testing.T) {
	for _, option := range []string{"max-size=0", "max-size=nope", "max-file=0", "unknown=value", "missing"} {
		b := &fakeBackend{}
		var stdout, stderr bytes.Buffer
		if status := runCommand([]string{"run", "--log-opt", option, "busybox"}, &stdout, &stderr, b); status != 1 {
			t.Errorf("--log-opt %q status=%d, want 1", option, status)
		}
		if b.runRequest.reference != "" {
			t.Errorf("--log-opt %q invoked backend with %+v", option, b.runRequest)
		}
	}
}

func TestRunRejectsUnmanagedWritableMounts(t *testing.T) {
	for _, args := range [][]string{
		{"run", "-v", "/host:/data", "busybox"},
		{"run", "--mount", "type=bind,source=/host,target=/data", "busybox"},
	} {
		b := &fakeBackend{}
		var stdout, stderr bytes.Buffer
		if status := runCommand(args, &stdout, &stderr, b); status != 1 {
			t.Errorf("runCommand(%q) status=%d, want 1", args, status)
		}
		if b.runRequest.reference != "" {
			t.Errorf("runCommand(%q) invoked backend with %+v", args, b.runRequest)
		}
	}
}

func TestRunRejectsInvalidFilesystemMounts(t *testing.T) {
	for _, args := range [][]string{
		{"run", "--tmpfs", "relative", "busybox"},
		{"run", "--tmpfs", "/scratch:mode=0899", "busybox"},
		{"run", "--tmpfs", "/scratch:size=zero", "busybox"},
		{"run", "--tmpfs", "/scratch:unknown", "busybox"},
		{"run", "--mount", "type=tmpfs,target=/scratch,source=host", "busybox"},
		{"run", "--mount", "type=proc,target=relative", "busybox"},
		{"run", "--mount", "type=proc,target=/alt-proc,size=1m", "busybox"},
		{"run", "--mount", "type=tmpfs,target=/scratch,tmpfs-size=2m", "busybox"},
		{"run", "--tmpfs", "/scratch:uid=-1", "busybox"},
		{"run", "--tmpfs", "/scratch:nr_blocks=10%", "busybox"},
		{"run", "--mount", "type=sysfs,target=/alt-sys,readonly=maybe", "busybox"},
		{"run", "--mount", "type=tmpfs,target=/scratch,target=/again", "busybox"},
	} {
		b := &fakeBackend{}
		var stdout, stderr bytes.Buffer
		if status := runCommand(args, &stdout, &stderr, b); status != 1 {
			t.Errorf("runCommand(%q) status=%d, want 1", args, status)
		}
		if b.runRequest.reference != "" {
			t.Errorf("runCommand(%q) invoked backend with %+v", args, b.runRequest)
		}
	}
}

func TestRunForwardsContainerExitStatus(t *testing.T) {
	b := &fakeBackend{runStatus: 42}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"run", "busybox", "false"}, &stdout, &stderr, b); status != 42 {
		t.Fatalf("run status=%d, want 42; stderr=%q", status, stderr.String())
	}
}

func TestCreateAndStart(t *testing.T) {
	containerID := strings.Repeat("b", 64)
	b := &fakeBackend{containerID: containerID}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"create", "--name", "demo", "-it", "--rm", "--stop-signal", "SIGQUIT", "--stop-timeout", "4", "--net", "host", "busybox", "sleep", "30"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("create status=%d, stderr=%q", status, stderr.String())
	}
	if !b.created || b.runRequest.name != "demo" || b.runRequest.pullPolicy != "missing" || b.runRequest.networkMode != "host" || !b.runRequest.interactive || !b.runRequest.terminal || !b.runRequest.autoRemove || b.runRequest.stopSignal != "SIGQUIT" || b.runRequest.stopTimeout != 4 || strings.Join(b.runRequest.args, " ") != "sleep 30" {
		t.Errorf("create request=%+v created=%t", b.runRequest, b.created)
	}
	if got, want := stdout.String(), containerID+"\n"; got != want {
		t.Errorf("create output=%q, want %q", got, want)
	}
	stdout.Reset()
	if status := runCommand([]string{"start", "demo"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("start status=%d, stderr=%q", status, stderr.String())
	}
	if b.started != "demo" || !b.startDetach || b.startInteractive {
		t.Errorf("start name=%q detach=%t interactive=%t", b.started, b.startDetach, b.startInteractive)
	}
	if stdout.String() != "demo\n" {
		t.Errorf("start output=%q", stdout.String())
	}
}

func TestStartMultipleAndContainerAliases(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"container", "start", "one", "two"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("container start status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := stdout.String(), "one\ntwo\n"; got != want {
		t.Errorf("multi-start output=%q, want %q", got, want)
	}
	if got, want := b.startCalls, []startCall{{name: "one", detach: true}, {name: "two", detach: true}}; !reflect.DeepEqual(got, want) {
		t.Errorf("start calls=%+v, want %+v", got, want)
	}

	b = &fakeBackend{}
	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"start", "--attach", "one", "two"}, &stdout, &stderr, b); status == 0 {
		t.Fatal("multi-start accepted --attach")
	}
	if len(b.startCalls) != 0 {
		t.Errorf("rejected attached multi-start made calls %+v", b.startCalls)
	}

	b = &fakeBackend{inspectInfo: InspectInfo{ID: "demo", Name: "/demo"}}
	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"container", "inspect", "-f", "{{.Id}}|{{.ID}}|{{json .}}", "demo"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("container inspect status=%d, stderr=%q", status, stderr.String())
	}
	if got := stdout.String(); !strings.HasPrefix(got, "demo|demo|") || !strings.Contains(got, `"Id":"demo"`) || strings.Contains(got, `"ID":`) {
		t.Errorf("Docker and legacy ID template output=%q", got)
	}

	if status := runCommand([]string{"container", "unknown"}, &stdout, &stderr, b); status == 0 {
		t.Fatal("container namespace accepted an unknown command")
	}
}

func TestAttach(t *testing.T) {
	b := &fakeBackend{waitStatus: uint32(7 << 8)}
	var stdout, stderr bytes.Buffer
	status := runCommand([]string{"attach", "-i", "--exec", "exec-one", "--detach-keys", "ctrl-a,z", "demo@child"}, &stdout, &stderr, b)
	if status != 7 {
		t.Fatalf("attach status=%d, stderr=%q", status, stderr.String())
	}
	if b.attached.target != (selector{container: "demo", branch: "child"}) || b.attached.execID != "exec-one" || !b.attached.stdin || !b.attached.stdout || !b.attached.stderr {
		t.Errorf("attach request=%+v", b.attached)
	}
	if got, want := b.attached.detachKeys, []byte{1, 'z'}; !bytes.Equal(got, want) {
		t.Errorf("attach detach keys=%v, want %v", got, want)
	}
}

func TestAttachStreamSelection(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		stdin  bool
		stdout bool
		stderr bool
	}{
		{name: "output", args: []string{"--no-stdin"}, stdout: true, stderr: true},
		{name: "stdin-only", args: []string{"-i", "--no-stdout", "--no-stderr"}, stdin: true},
		{name: "stdout-only", args: []string{"--no-stderr"}, stdout: true},
		{name: "stderr-only", args: []string{"--no-stdout"}, stderr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			b := &fakeBackend{}
			var stdout, stderr bytes.Buffer
			args := append([]string{"attach"}, test.args...)
			args = append(args, "demo")
			if status := runCommand(args, &stdout, &stderr, b); status != 0 {
				t.Fatalf("attach status=%d, stderr=%q", status, stderr.String())
			}
			if b.attached.stdin != test.stdin || b.attached.stdout != test.stdout || b.attached.stderr != test.stderr {
				t.Errorf("attach streams=(stdin=%t stdout=%t stderr=%t), want (%t %t %t)",
					b.attached.stdin, b.attached.stdout, b.attached.stderr, test.stdin, test.stdout, test.stderr)
			}
		})
	}
	for _, args := range [][]string{
		{"attach", "--no-stdout", "--no-stderr", "demo"},
		{"attach", "-i", "--no-stdin", "demo"},
	} {
		b := &fakeBackend{}
		var stdout, stderr bytes.Buffer
		if status := runCommand(args, &stdout, &stderr, b); status != 1 {
			t.Errorf("runCommand(%q) status=%d, want 1", args, status)
		}
		if b.attached.target.container != "" {
			t.Errorf("rejected attach reached backend with %+v", b.attached)
		}
	}
}

func TestAttachDetachIsSuccess(t *testing.T) {
	b := &fakeBackend{err: errDetached}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"attach", "-i", "demo"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("attach status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := b.attached.detachKeys, defaultDetachKeys; !bytes.Equal(got, want) {
		t.Errorf("default detach keys=%v, want %v", got, want)
	}
}

func TestParseDetachKeys(t *testing.T) {
	for _, test := range []struct {
		value string
		want  []byte
	}{
		{value: "ctrl-p,ctrl-q", want: []byte{0x10, 0x11}},
		{value: "CTRL-@,ctrl-[,ctrl-\\,ctrl-_,x", want: []byte{0, 27, 28, 31, 'x'}},
	} {
		got, err := parseDetachKeys(test.value)
		if err != nil {
			t.Errorf("parseDetachKeys(%q): %v", test.value, err)
		} else if !bytes.Equal(got, test.want) {
			t.Errorf("parseDetachKeys(%q)=%v, want %v", test.value, got, test.want)
		}
	}
	for _, value := range []string{"", "ctrl-", "ctrl-1", "too-long"} {
		if _, err := parseDetachKeys(value); err == nil {
			t.Errorf("parseDetachKeys(%q) succeeded", value)
		}
	}
}

func TestDetachKeyFilterAcrossReads(t *testing.T) {
	filter := detachKeyFilter{sequence: append([]byte(nil), defaultDetachKeys...)}
	output, detached := filter.push([]byte("hello\x10"))
	if got, want := string(output), "hello"; got != want || detached {
		t.Fatalf("first push=(%q, %t), want (%q, false)", got, detached, want)
	}
	output, detached = filter.push([]byte("\x11ignored"))
	if len(output) != 0 || !detached {
		t.Fatalf("second push=(%q, %t), want (empty, true)", output, detached)
	}
	if pending := filter.flush(); len(pending) != 0 {
		t.Errorf("flush after detach=%q, want empty", pending)
	}
}

func TestDetachKeyFilterForwardsNearMatchAndFlushesPrefix(t *testing.T) {
	filter := detachKeyFilter{sequence: append([]byte(nil), defaultDetachKeys...)}
	output, detached := filter.push([]byte{'a', 0x10, 'x', 0x10})
	if got, want := output, []byte{'a', 0x10, 'x'}; !bytes.Equal(got, want) || detached {
		t.Fatalf("push=(%v, %t), want (%v, false)", got, detached, want)
	}
	if got, want := filter.flush(), []byte{0x10}; !bytes.Equal(got, want) {
		t.Errorf("flush=%v, want %v", got, want)
	}
}

func TestLogs(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"logs", "--follow", "--exec", "exec-one", "--tail", "7", "demo@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("logs status=%d, stderr=%q", status, stderr.String())
	}
	if b.logsTarget != (selector{container: "demo", branch: "child"}) || b.logsExecID != "exec-one" || !b.logsFollow || b.logsTail != 7 {
		t.Errorf("logs target=%+v exec=%q follow=%t tail=%d", b.logsTarget, b.logsExecID, b.logsFollow, b.logsTail)
	}
	if status := runCommand([]string{"logs", "--tail", "invalid", "demo"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("logs with Docker-compatible invalid tail status=%d, stderr=%q", status, stderr.String())
	}
	if b.logsTail != -1 {
		t.Errorf("invalid --tail selected %d, want all (-1)", b.logsTail)
	}
}

func TestConsoleLogTailPreservesGlobalLineOrderAndStreams(t *testing.T) {
	tail := &consoleLogTail{limit: 3}
	stdout := tail.writer(ttosconsole.StreamStdout)
	stderr := tail.writer(ttosconsole.StreamStderr)
	stdout.Write([]byte("drop\nkeep-out"))
	stderr.Write([]byte("keep-err-1\nkeep-err-2\n"))
	stdout.Write([]byte("-continued"))

	var gotStdout, gotStderr bytes.Buffer
	if err := tail.flush(&gotStdout, &gotStderr); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got, want := gotStdout.String(), "keep-out-continued"; got != want {
		t.Errorf("stdout=%q, want %q", got, want)
	}
	if got, want := gotStderr.String(), "keep-err-1\nkeep-err-2\n"; got != want {
		t.Errorf("stderr=%q, want %q", got, want)
	}
}

func TestConsoleLogFollowerTailsAcrossRetainedGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "main.log")
	w, err := ttosconsole.NewRotatingWriter(path, 34)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	for _, value := range []string{"drop\n", "keep-one\n", "keep-two\n"} {
		if err := ttosconsole.WriteFrame(w, ttosconsole.StreamStdout, []byte(value)); err != nil {
			t.Fatalf("WriteFrame(%q): %v", value, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	follower, err := openConsoleLogFollower(path)
	if err != nil {
		t.Fatalf("openConsoleLogFollower: %v", err)
	}
	defer follower.file.Close()
	tail := &consoleLogTail{limit: 2}
	if _, err := follower.drain(tail.writer(ttosconsole.StreamStdout), tail.writer(ttosconsole.StreamStderr)); err != nil {
		t.Fatalf("drain: %v", err)
	}
	var stdout bytes.Buffer
	if err := tail.flush(&stdout, nil); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got, want := stdout.String(), "keep-one\nkeep-two\n"; got != want {
		t.Errorf("stdout=%q, want %q", got, want)
	}
}

func TestConsoleLogFollowerHandlesPartialAppend(t *testing.T) {
	var encoded bytes.Buffer
	for _, frame := range []struct {
		stream byte
		data   string
	}{
		{stream: ttosconsole.StreamStdout, data: "hello"},
		{stream: ttosconsole.StreamStderr, data: "problem"},
		{stream: ttosconsole.StreamStdout},
		{stream: ttosconsole.StreamStderr},
	} {
		if err := ttosconsole.WriteFrame(&encoded, frame.stream, []byte(frame.data)); err != nil {
			t.Fatalf("WriteFrame: %v", err)
		}
	}
	path := filepath.Join(t.TempDir(), "main.log")
	data := encoded.Bytes()
	if err := os.WriteFile(path, data[:5], 0600); err != nil {
		t.Fatalf("writing partial log: %v", err)
	}
	follower, err := openConsoleLogFollower(path)
	if err != nil {
		t.Fatalf("openConsoleLogFollower: %v", err)
	}
	defer follower.file.Close()
	var stdout, stderr bytes.Buffer
	if progress, err := follower.drain(&stdout, &stderr); err != nil || !progress {
		t.Fatalf("draining partial header: progress=%t err=%v", progress, err)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("partial frame produced stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	appendFile, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatalf("opening append log: %v", err)
	}
	if _, err := appendFile.Write(data[5:]); err != nil {
		appendFile.Close()
		t.Fatalf("appending log: %v", err)
	}
	if err := appendFile.Close(); err != nil {
		t.Fatalf("closing append log: %v", err)
	}
	if progress, err := follower.drain(&stdout, &stderr); err != nil || !progress {
		t.Fatalf("draining completed frames: progress=%t err=%v", progress, err)
	}
	if got := stdout.String(); got != "hello" {
		t.Errorf("stdout=%q, want hello", got)
	}
	if got := stderr.String(); got != "problem" {
		t.Errorf("stderr=%q, want problem", got)
	}
	if !follower.stdoutEOF || !follower.stderrEOF || len(follower.pending) != 0 {
		t.Errorf("follower state: stdoutEOF=%t stderrEOF=%t pending=%d", follower.stdoutEOF, follower.stderrEOF, len(follower.pending))
	}
}

func TestConsoleLogFollowerFollowsLiveRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "main.log")
	w, err := ttosconsole.NewRotatingWriter(path, 28)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	for _, value := range []string{"first", "second"} {
		if err := ttosconsole.WriteFrame(w, ttosconsole.StreamStdout, []byte(value)); err != nil {
			t.Fatalf("WriteFrame(%q): %v", value, err)
		}
	}
	follower, err := openConsoleLogFollower(path)
	if err != nil {
		t.Fatalf("openConsoleLogFollower: %v", err)
	}
	defer follower.file.Close()
	if err := ttosconsole.WriteFrame(w, ttosconsole.StreamStdout, []byte("third")); err != nil {
		t.Fatalf("WriteFrame(third): %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if progress, err := follower.drain(&stdout, &stderr); err != nil || !progress {
		t.Fatalf("drain: progress=%t err=%v", progress, err)
	}
	if got, want := stdout.String(), "firstsecondthird"; got != want {
		t.Errorf("stdout=%q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr=%q, want empty", stderr.String())
	}
}

func TestConsoleLogFollowerReportsRetentionOverrun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "main.log")
	w, err := ttosconsole.NewRotatingWriterWithFiles(path, 10, 2)
	if err != nil {
		t.Fatalf("NewRotatingWriterWithFiles: %v", err)
	}
	if err := ttosconsole.WriteFrame(w, ttosconsole.StreamStdout, []byte("first")); err != nil {
		t.Fatalf("WriteFrame(first): %v", err)
	}
	follower, err := openConsoleLogFollower(path, 2)
	if err != nil {
		t.Fatalf("openConsoleLogFollower: %v", err)
	}
	defer follower.file.Close()
	for _, value := range []string{"second", "third"} {
		if err := ttosconsole.WriteFrame(w, ttosconsole.StreamStdout, []byte(value)); err != nil {
			t.Fatalf("WriteFrame(%q): %v", value, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	var stdout bytes.Buffer
	_, err = follower.drain(&stdout, nil)
	if err == nil || !strings.Contains(err.Error(), "fell behind retention") {
		t.Fatalf("drain error=%v, want retention overrun", err)
	}
	if got, want := stdout.String(), "first"; got != want {
		t.Errorf("stdout before overrun=%q, want %q", got, want)
	}
}
func TestConsoleLogFollowerReplaysRetainedGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "main.log")
	w, err := ttosconsole.NewRotatingWriter(path, 28)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	for _, value := range []string{"first", "second", "third"} {
		if err := ttosconsole.WriteFrame(w, ttosconsole.StreamStderr, []byte(value)); err != nil {
			t.Fatalf("WriteFrame(%q): %v", value, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	follower, err := openConsoleLogFollower(path)
	if err != nil {
		t.Fatalf("openConsoleLogFollower: %v", err)
	}
	defer follower.file.Close()
	var stdout, stderr bytes.Buffer
	if progress, err := follower.drain(&stdout, &stderr); err != nil || !progress {
		t.Fatalf("drain: progress=%t err=%v", progress, err)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout=%q, want empty", stdout.String())
	}
	if got, want := stderr.String(), "firstsecondthird"; got != want {
		t.Errorf("stderr=%q, want %q", got, want)
	}
}

func TestConsoleLogFollowerReplaysMultipleGenerations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "main.log")
	w, err := ttosconsole.NewRotatingWriterWithFiles(path, 10, 3)
	if err != nil {
		t.Fatalf("NewRotatingWriterWithFiles: %v", err)
	}
	for _, value := range []string{"drop", "two", "three", "four"} {
		if err := ttosconsole.WriteFrame(w, ttosconsole.StreamStdout, []byte(value)); err != nil {
			t.Fatalf("WriteFrame(%q): %v", value, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	follower, err := openConsoleLogFollower(path, 3)
	if err != nil {
		t.Fatalf("openConsoleLogFollower: %v", err)
	}
	defer follower.file.Close()
	var stdout bytes.Buffer
	if progress, err := follower.drain(&stdout, nil); err != nil || !progress {
		t.Fatalf("drain: progress=%t err=%v", progress, err)
	}
	if got, want := stdout.String(), "twothreefour"; got != want {
		t.Errorf("stdout=%q, want %q", got, want)
	}
}
func TestConsoleDrainHealth(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "console.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatalf("opening lock: %v", err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("locking: %v", err)
	}
	if active, err := consoleDrainActive(lockPath); err != nil || !active {
		t.Fatalf("active probe=(%t, %v), want (true, nil)", active, err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatalf("unlocking: %v", err)
	}
	if active, err := consoleDrainActive(lockPath); err != nil || active {
		t.Fatalf("inactive probe=(%t, %v), want (false, nil)", active, err)
	}
	errorPath := filepath.Join(dir, "console.error.log")
	if err := os.WriteFile(errorPath, []byte("low-level failure\n"), 0600); err != nil {
		t.Fatalf("writing diagnostic: %v", err)
	}
	if err := consoleDrainStoppedError(errorPath); err == nil || !strings.Contains(err.Error(), "low-level failure") {
		t.Fatalf("consoleDrainStoppedError=%v", err)
	}
}

func TestExecParsesBranchAndDockerStyleOptions(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	args := []string{"exec", "-it", "-e", "A=1", "--workdir", "/work", "--user", "123:456", "--additional-gids", "12", "--additional-gids", "34", "--cap", "CAP_CHOWN", "--cap", "CAP_SYS_PTRACE", "demo@child", "sh", "-c", "echo ok"}
	if status := runCommand(args, &stdout, &stderr, b); status != 0 {
		t.Fatalf("exec status=%d, stderr=%q", status, stderr.String())
	}
	request := b.execRequest
	if request.target != (selector{container: "demo", branch: "child"}) || !request.interactive || !request.terminal || request.cwd != "/work" || request.user != "123:456" {
		t.Errorf("exec request=%+v", request)
	}
	if got := strings.Join(request.env, " "); got != "A=1" {
		t.Errorf("exec env=%q", got)
	}
	if got, want := request.additionalGIDs, []string{"12", "34"}; !reflect.DeepEqual(got, want) {
		t.Errorf("exec additional GIDs=%v, want %v", got, want)
	}
	if got, want := request.capabilities, []string{"CAP_CHOWN", "CAP_SYS_PTRACE"}; !reflect.DeepEqual(got, want) {
		t.Errorf("exec capabilities=%v, want %v", got, want)
	}
	if got := strings.Join(request.args, " "); got != "sh -c echo ok" {
		t.Errorf("exec args=%q", got)
	}
}

func TestExecProcessFileControlsCompleteProcess(t *testing.T) {
	dir := t.TempDir()
	processPath := filepath.Join(dir, "process.json")
	data, err := json.Marshal(specs.Process{
		Terminal: true,
		Args:     []string{"sh", "-c", "echo process"},
		Cwd:      "/work",
		User:     specs.User{UID: 123, GID: 456, AdditionalGids: []uint32{12, 34}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(processPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"exec", "-di", "--process", processPath, "demo@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("exec --process status=%d, stderr=%q", status, stderr.String())
	}
	request := b.execRequest
	if request.processPath != processPath || !request.detach || !request.interactive || !request.terminal || len(request.args) != 0 {
		t.Fatalf("exec --process request=%+v", request)
	}
	b = &fakeBackend{}
	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"exec", "--process", processPath, "--env", "A=1", "demo"}, &stdout, &stderr, b); status == 0 {
		t.Fatal("exec --process accepted a CLI process override")
	}
	if b.execRequest.processPath != "" {
		t.Fatalf("rejected --process invocation reached backend: %+v", b.execRequest)
	}
}

func TestExecDetachReturnsQualifiedSessionID(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"exec", "-dit", "demo@child", "sleep", "30"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("detached exec status=%d, stderr=%q", status, stderr.String())
	}
	request := b.execRequest
	if !request.detach || !request.interactive || !request.terminal || !strings.HasPrefix(request.execID, "exec-") {
		t.Fatalf("detached exec request=%+v", request)
	}
	want := "demo@child:" + request.execID + "\n"
	if got := stdout.String(); got != want {
		t.Fatalf("detached exec output=%q, want %q", got, want)
	}
}

func TestExecSessionControls(t *testing.T) {
	created := time.Date(2026, time.July, 17, 2, 3, 4, 0, time.UTC)
	b := &fakeBackend{execSessionList: []runscExecSession{{
		ID: "exec-abc", PID: 7, State: "running", Terminal: true, CreatedAt: created,
	}}, waitStatus: uint32(7 << 8)}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"exec", "ls", "demo@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("exec ls status=%d, stderr=%q", status, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "exec-abc") || !strings.Contains(got, "running") {
		t.Fatalf("exec ls output=%q", got)
	}
	stdout.Reset()
	if status := runCommand([]string{"exec", "wait", "demo@child:exec-abc"}, &stdout, &stderr, b); status != 0 || stdout.String() != "7\n" {
		t.Fatalf("exec wait status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
	stdout.Reset()
	if status := runCommand([]string{"exec", "kill", "--signal", "TERM", "demo@child:exec-abc"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("exec kill status=%d, stderr=%q", status, stderr.String())
	}
	if b.execControlTarget != (selector{container: "demo", branch: "child"}) || b.execControlID != "exec-abc" || b.execControlSignal != 15 {
		t.Fatalf("exec control target=%+v id=%q signal=%d", b.execControlTarget, b.execControlID, b.execControlSignal)
	}
	if stdout.String() != "demo@child:exec-abc\n" {
		t.Fatalf("exec kill output=%q", stdout.String())
	}
}

func TestParseExecSelectorRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{"", "demo", "demo:", "demo:../bad", "demo@child:-bad"} {
		if _, err := parseExecSelector(value); err == nil {
			t.Errorf("parseExecSelector(%q) succeeded", value)
		}
	}
}

func TestExecForwardsProcessExitStatus(t *testing.T) {
	b := &fakeBackend{execStatus: 23}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"exec", "demo", "false"}, &stdout, &stderr, b); status != 23 {
		t.Fatalf("exec status=%d, want 23; stderr=%q", status, stderr.String())
	}
}

func TestInspect(t *testing.T) {
	b := &fakeBackend{inspectInfo: InspectInfo{
		ID: "demo", Name: "/demo", Image: "sha256:abc",
		State:  InspectState{Status: "running", Running: true},
		Config: InspectConfig{Image: "docker.io/library/busybox:latest"},
		TTOS: TTOSInspectInfo{Branches: []InspectBranch{{
			Metadata: Branch{ID: "main", State: "running"},
		}}},
	}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"inspect", "demo"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("inspect status=%d, stderr=%q", status, stderr.String())
	}
	var infos []InspectInfo
	if err := json.Unmarshal(stdout.Bytes(), &infos); err != nil {
		t.Fatalf("decoding inspect output %q: %v", stdout.String(), err)
	}
	if len(infos) != 1 || infos[0].ID != "demo" || infos[0].State.Status != "running" || infos[0].Config.Image != "docker.io/library/busybox:latest" {
		t.Errorf("inspect output=%+v", infos)
	}

	stdout.Reset()
	if status := runCommand([]string{"inspect", "--format", `{{.Name}}|{{.State.Status}}|{{len .TTOS.Branches}}`, "demo"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("formatted inspect status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := stdout.String(), "/demo|running|1\n"; got != want {
		t.Errorf("formatted inspect=%q, want %q", got, want)
	}

	stdout.Reset()
	b.checkResult = ForkEligibility{Eligible: true}
	if status := runCommand([]string{"inspect", "--fork-check", "-f", `{{(index .TTOS.Branches 0).ForkEligibility.Eligible}}`, "demo"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("fork-check inspect status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := b.checkTarget, (selector{container: "demo", branch: "main"}); got != want {
		t.Errorf("fork-check target=%+v, want %+v", got, want)
	}
	if got, want := stdout.String(), "true\n"; got != want {
		t.Errorf("fork-check output=%q, want %q", got, want)
	}

	if status := runCommand([]string{"inspect", "--type", "image", "demo"}, &stdout, &stderr, b); status == 0 {
		t.Fatal("inspect accepted unsupported object type")
	}
	if status := runCommand([]string{"inspect", "demo@main"}, &stdout, &stderr, b); status == 0 {
		t.Fatal("inspect accepted branch selector")
	}
	if status := runCommand([]string{"inspect", "--format", "{{", "demo"}, &stdout, &stderr, b); status == 0 {
		t.Fatal("inspect accepted invalid template")
	}
}

func TestBuildInspectInfo(t *testing.T) {
	containerID := strings.Repeat("c", 64)
	created := time.Date(2026, 7, 17, 1, 2, 3, 4, time.UTC)
	started := created.Add(time.Second)
	exit := 23
	stopTimeout := 4
	pidsLimit := int64(23)
	container := ttosbundle.ContainerInfo{
		ID: containerID, Name: "demo", Reference: "docker.io/library/alpine:3.20", Digest: "sha256:abc", Platform: "linux/amd64",
		RootFS: "/roots/alpine", NetworkMode: "host", AutoRemove: true, StopSignal: "SIGUSR1", StopTimeout: &stopTimeout, PidsLimit: &pidsLimit, State: "running", ExitStatus: &exit, CreatedAt: created, StartedAt: &started,
	}
	spec := &specs.Spec{
		Root: &specs.Root{Readonly: true},
		Process: &specs.Process{
			Args: []string{"/bin/sh", "-c", "sleep 10"}, Env: []string{"A=1"}, Cwd: "/work",
			User: specs.User{UID: 123, GID: 456},
		},
		Mounts: []specs.Mount{
			{Type: "proc", Source: "proc", Destination: "/proc"},
			{Type: "bind", Source: "/host/data", Destination: "/data", Options: []string{"rbind", "ro"}},
			{Type: "tmpfs", Source: "ttos-volume-cache", Destination: "/cache", Options: []string{"rw"}},
			{Type: "tmpfs", Source: "tmpfs", Destination: "/scratch", Options: []string{"rw", "mode=0700", "size=1m"}},
			{Type: "tmpfs", Source: "tmpfs", Destination: "/worktmp", Options: []string{"rw", "size=2097152", "mode=0750"}},
			{Type: "proc", Source: "proc", Destination: "/alt-proc", Options: []string{"ro", "nosuid", "nodev", "noexec"}},
			{Type: "sysfs", Source: "sysfs", Destination: "/alt-sys", Options: []string{"ro", "nosuid", "nodev", "noexec"}},
		},
		Hostname: "custom-host",
		Annotations: map[string]string{
			"dev.timetravelos.config.entrypoint": `["/bin/sh"]`,
			"dev.timetravelos.config.cmd":        `["-c","sleep 10"]`,
			"dev.timetravelos.config.user":       "app",
			"dev.timetravelos.config.labels":     `{"suite":"ttos"}`,
			"dev.timetravelos.config.tmpfs":      `{"/scratch":"mode=0700,size=1m"}`,
		},
	}
	family := &Family{ID: containerID, Name: "demo", State: "running", SandboxPID: 4321, Branches: []Branch{
		{ID: "child", ParentID: "main", State: "running"},
		{ID: "main", State: "paused"},
	}}
	info := buildInspectInfo(container, spec, family, "")
	if info.ID != containerID || info.Name != "/demo" || info.Path != "/bin/sh" || !reflect.DeepEqual(info.Args, []string{"-c", "sleep 10"}) {
		t.Errorf("identity/process view=%+v", info)
	}
	if info.State.Status != "paused" || !info.State.Running || !info.State.Paused || info.State.Pid != 4321 || info.State.ExitCode != 23 {
		t.Errorf("state=%+v", info.State)
	}
	if info.Config.Hostname != "custom-host" || info.Config.User != "app" || info.Config.WorkingDir != "/work" || info.Config.Image != container.Reference {
		t.Errorf("config=%+v", info.Config)
	}
	if !reflect.DeepEqual(info.Config.Entrypoint, []string{"/bin/sh"}) || !reflect.DeepEqual(info.Config.Cmd, []string{"-c", "sleep 10"}) || info.Config.Labels["suite"] != "ttos" {
		t.Errorf("Docker config reconstruction=%+v", info.Config)
	}
	if len(info.Mounts) != 5 || info.Mounts[0].Type != "bind" || info.Mounts[0].RW || info.Mounts[1].Name != "cache" || !info.Mounts[1].RW || info.Mounts[2].Type != "tmpfs" || !info.Mounts[2].RW || info.Mounts[3].Type != "proc" || info.Mounts[3].RW || info.Mounts[4].Type != "sysfs" || info.Mounts[4].RW {
		t.Errorf("mounts=%+v", info.Mounts)
	}
	if got := info.HostConfig.Binds; !reflect.DeepEqual(got, []string{"/host/data:/data:ro"}) {
		t.Errorf("binds=%q", got)
	}
	if got := info.HostConfig.Tmpfs["/scratch"]; got != "mode=0700,size=1m" {
		t.Errorf("tmpfs inspect option=%q", got)
	}
	if !info.HostConfig.ReadonlyRootfs {
		t.Error("inspect omitted read-only rootfs policy")
	}
	if info.Config.StopSignal != "SIGUSR1" || info.HostConfig.StopTimeout == nil || *info.HostConfig.StopTimeout != 4 {
		t.Errorf("inspect stop policy=(%q, %v), want (SIGUSR1, 4)", info.Config.StopSignal, info.HostConfig.StopTimeout)
	}
	if info.HostConfig.PidsLimit == nil || *info.HostConfig.PidsLimit != pidsLimit {
		t.Errorf("inspect pids limit=%v, want %d", info.HostConfig.PidsLimit, pidsLimit)
	}
	if !info.HostConfig.AutoRemove {
		t.Error("inspect omitted auto-remove policy")
	}
	if info.HostConfig.NetworkMode != "host" {
		t.Errorf("network mode=%q, want host", info.HostConfig.NetworkMode)
	}
	if _, ok := info.Config.Volumes["/cache"]; !ok {
		t.Errorf("volumes=%v", info.Config.Volumes)
	}
	if len(info.TTOS.Branches) != 2 || info.TTOS.Branches[0].Metadata.ID != "child" {
		t.Errorf("TTOS branches=%+v", info.TTOS.Branches)
	}
}

func TestInternalAutoRemoveWaitsThenForcesRemoval(t *testing.T) {
	b := &fakeBackend{waitStatus: uint32(7 << 8)}
	if err := runAutoRemove([]string{"demo"}, b); err != nil {
		t.Fatalf("runAutoRemove: %v", err)
	}
	if b.waited != (selector{container: "demo", branch: "main"}) {
		t.Errorf("waited=%+v", b.waited)
	}
	if b.removed != "demo" || !b.removeForce {
		t.Errorf("remove=(%q, force=%t)", b.removed, b.removeForce)
	}
	if err := runAutoRemove([]string{"demo@child"}, b); err == nil {
		t.Fatal("internal auto-remove accepted a branch selector")
	}
}

func TestRemove(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"rm", "--force", "demo"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("rm status=%d, stderr=%q", status, stderr.String())
	}
	if b.removed != "demo" || !b.removeForce || stdout.String() != "demo\n" {
		t.Errorf("removed=%q force=%t output=%q", b.removed, b.removeForce, stdout.String())
	}
}

func TestVolumeCommands(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"volume", "create", "cache"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("volume create status=%d stderr=%q", status, stderr.String())
	}
	if b.ensuredVolume != "cache" || stdout.String() != "cache\n" {
		t.Errorf("create ensured=%q output=%q", b.ensuredVolume, stdout.String())
	}

	stdout.Reset()
	if status := runCommand([]string{"volume", "ls", "-q"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("volume ls status=%d stderr=%q", status, stderr.String())
	}
	if stdout.String() != "cache\n" {
		t.Errorf("quiet list output=%q", stdout.String())
	}

	stdout.Reset()
	if status := runCommand([]string{"volume", "inspect", "cache"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("volume inspect status=%d stderr=%q", status, stderr.String())
	}
	var inspected []ttosbundle.VolumeInfo
	if err := json.Unmarshal(stdout.Bytes(), &inspected); err != nil {
		t.Fatalf("decoding volume inspect: %v", err)
	}
	if len(inspected) != 1 || inspected[0].Name != "cache" || inspected[0].Driver != "local-tmpfs" {
		t.Errorf("inspect returned %+v", inspected)
	}

	stdout.Reset()
	if status := runCommand([]string{"volume", "rm", "cache"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("volume rm status=%d stderr=%q", status, stderr.String())
	}
	if b.removedVolume != "cache" || stdout.String() != "cache\n" {
		t.Errorf("remove volume=%q output=%q", b.removedVolume, stdout.String())
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

func TestKillParsesSignalAndSelectsBranch(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"kill", "--signal", "SIGTERM", "web@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("kill status=%d, stderr=%q", status, stderr.String())
	}
	if len(b.signals) != 1 || b.signals[0].target != (selector{container: "web", branch: "child"}) || b.signals[0].signal != 15 || b.signals[0].all {
		t.Errorf("signal calls=%+v", b.signals)
	}
}

func TestStopGraceful(t *testing.T) {
	timeout := 3
	b := &fakeBackend{inspectInfo: InspectInfo{
		Config:     InspectConfig{StopSignal: "SIGUSR1"},
		HostConfig: InspectHostConfig{StopTimeout: &timeout},
	}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"stop", "web@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("stop status=%d, stderr=%q", status, stderr.String())
	}
	if len(b.signals) != 1 || b.signals[0].signal != 10 {
		t.Errorf("signal calls=%+v, want configured SIGUSR1 only", b.signals)
	}
	if b.waited != (selector{container: "web", branch: "child"}) {
		t.Errorf("wait target=%+v", b.waited)
	}
}

func TestStopEscalatesToKill(t *testing.T) {
	timeout := 0
	b := &fakeBackend{
		waitRelease: make(chan struct{}),
		inspectInfo: InspectInfo{
			Config:     InspectConfig{StopSignal: "SIGUSR2"},
			HostConfig: InspectHostConfig{StopTimeout: &timeout},
		},
	}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"stop", "web"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("stop status=%d, stderr=%q", status, stderr.String())
	}
	if len(b.signals) != 2 || b.signals[0].signal != 12 || b.signals[1].signal != 9 {
		t.Errorf("signal calls=%+v, want configured SIGUSR2 then SIGKILL", b.signals)
	}
}

func TestStopTimeoutOverride(t *testing.T) {
	configuredTimeout := 30
	b := &fakeBackend{
		waitRelease: make(chan struct{}),
		inspectInfo: InspectInfo{
			Config:     InspectConfig{StopSignal: "SIGTERM"},
			HostConfig: InspectHostConfig{StopTimeout: &configuredTimeout},
		},
	}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"stop", "--time", "0", "web"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("stop override status=%d, stderr=%q", status, stderr.String())
	}
	if len(b.signals) != 2 || b.signals[0].signal != 15 || b.signals[1].signal != 9 {
		t.Errorf("signal calls=%+v, want SIGTERM then immediate SIGKILL", b.signals)
	}
}

func TestWaitPrintsDockerExitCodesInArgumentOrder(t *testing.T) {
	main := selector{container: "web", branch: "main"}
	child := selector{container: "web", branch: "child"}
	b := &fakeBackend{waitStatuses: map[selector]uint32{
		main:  uint32(7 << 8),
		child: uint32(syscall.SIGTERM),
	}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"container", "wait", "web", "web@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("wait status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := stdout.String(), "7\n143\n"; got != want {
		t.Errorf("wait output=%q, want %q", got, want)
	}
	b.mu.Lock()
	calls := append([]selector(nil), b.waitCalls...)
	b.mu.Unlock()
	if len(calls) != 2 || !containsSelector(calls, main) || !containsSelector(calls, child) {
		t.Errorf("wait calls=%+v", calls)
	}

	b = &fakeBackend{}
	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"wait", "--condition", "removed", "web"}, &stdout, &stderr, b); status == 0 {
		t.Fatal("wait accepted unsupported removed condition")
	}
}

func containsSelector(values []selector, target selector) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestRestartRunningExitedAndPausedFamilies(t *testing.T) {
	for _, test := range []struct {
		name       string
		state      string
		wantSignal bool
		wantResume bool
	}{
		{name: "running", state: "running", wantSignal: true},
		{name: "exited", state: "exited"},
		{name: "paused", state: "paused", wantSignal: true, wantResume: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			timeout := 1
			b := &fakeBackend{inspectInfo: InspectInfo{
				State:      InspectState{Status: test.state},
				Config:     InspectConfig{StopSignal: "SIGQUIT"},
				HostConfig: InspectHostConfig{StopTimeout: &timeout},
			}}
			var stdout, stderr bytes.Buffer
			if status := runCommand([]string{"restart", "web"}, &stdout, &stderr, b); status != 0 {
				t.Fatalf("restart status=%d, stderr=%q", status, stderr.String())
			}
			if got, want := stdout.String(), "web\n"; got != want {
				t.Errorf("restart output=%q, want %q", got, want)
			}
			if got, want := b.startCalls, []startCall{{name: "web", detach: true}}; !reflect.DeepEqual(got, want) {
				t.Errorf("start calls=%+v, want %+v", got, want)
			}
			if got := len(b.signals) != 0; got != test.wantSignal {
				t.Errorf("signals=%+v, wantSignal=%t", b.signals, test.wantSignal)
			}
			wantTarget := selector{container: "web", branch: "main"}
			if test.wantSignal && (len(b.signals) != 1 || b.signals[0].target != wantTarget || b.signals[0].signal != 3) {
				t.Errorf("restart signals=%+v, want configured main SIGQUIT", b.signals)
			}
			if test.wantResume && b.resumed != wantTarget {
				t.Errorf("resumed=%+v, want %+v", b.resumed, wantTarget)
			}
		})
	}

	descendantInfo := InspectInfo{
		State: InspectState{Status: "running", Running: true},
		TTOS: TTOSInspectInfo{Branches: []InspectBranch{
			{Metadata: Branch{ID: "main", State: "running"}},
			{Metadata: Branch{ID: "child", ParentID: "main", State: "running"}},
		}},
	}
	if got, want := restartDataLossReason(descendantInfo), `live descendant branch "child"`; got != want {
		t.Errorf("descendant restart reason=%q, want %q", got, want)
	}
	volumeInfo := InspectInfo{
		State: InspectState{Status: "running", Running: true},
		TTOS:  TTOSInspectInfo{Runtime: &Family{Volumes: []string{"cache"}}},
	}
	if got, want := restartDataLossReason(volumeInfo), "in-memory managed volume state (cache)"; got != want {
		t.Errorf("volume restart reason=%q, want %q", got, want)
	}
	b := &fakeBackend{inspectInfo: descendantInfo}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"restart", "web"}, &stdout, &stderr, b); status == 0 {
		t.Fatal("restart discarded a descendant without --force")
	}
	if len(b.signals) != 0 || len(b.startCalls) != 0 {
		t.Errorf("rejected restart mutated backend: signals=%+v starts=%+v", b.signals, b.startCalls)
	}
	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"restart", "--force", "web"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("forced restart status=%d, stderr=%q", status, stderr.String())
	}

	b = &fakeBackend{}
	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"restart", "web@child"}, &stdout, &stderr, b); status == 0 {
		t.Fatal("restart accepted an individual branch")
	}
}

func TestAcquireConsoleStdinIsExclusiveAndReleasable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "main.stdin.lock")
	target := selector{container: "demo", branch: "main"}
	first, err := acquireConsoleStdin(path, target)
	if err != nil {
		t.Fatalf("first acquireConsoleStdin: %v", err)
	}
	if _, err := acquireConsoleStdin(path, target); err == nil {
		first.Close()
		t.Fatal("second acquireConsoleStdin succeeded")
	} else if !strings.Contains(err.Error(), "stdin for demo@main is already attached") {
		first.Close()
		t.Fatalf("second acquireConsoleStdin error=%q", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("closing first owner: %v", err)
	}
	second, err := acquireConsoleStdin(path, target)
	if err != nil {
		t.Fatalf("reacquireConsoleStdin: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("closing second owner: %v", err)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatalf("stating stdin lock: %v", err)
	} else if info.Mode().Perm() != 0600 {
		t.Errorf("stdin lock mode=%o, want 600", info.Mode().Perm())
	}
}

func TestParseSignal(t *testing.T) {
	for _, test := range []struct {
		value string
		want  int
	}{
		{value: "9", want: 9},
		{value: "KILL", want: 9},
		{value: "sigusr1", want: 10},
	} {
		got, err := parseSignal(test.value)
		if err != nil || got != test.want {
			t.Errorf("parseSignal(%q)=(%d, %v), want (%d, nil)", test.value, got, err, test.want)
		}
	}
	if _, err := parseSignal("NOPE"); err == nil {
		t.Error("parseSignal(NOPE) succeeded")
	}
}

func TestReadConsoleWaitStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "main.wait-status")
	if status, found, err := readConsoleWaitStatus(path); err != nil || found || status != 0 {
		t.Fatalf("missing status=(%d, %t, %v), want (0, false, nil)", status, found, err)
	}
	if err := os.WriteFile(path, []byte("1792\n"), 0600); err != nil {
		t.Fatalf("writing status: %v", err)
	}
	if status, found, err := readConsoleWaitStatus(path); err != nil || !found || status != 1792 {
		t.Fatalf("status=(%d, %t, %v), want (1792, true, nil)", status, found, err)
	}
	if err := os.WriteFile(path, []byte("partial"), 0600); err != nil {
		t.Fatalf("writing invalid status: %v", err)
	}
	if _, _, err := readConsoleWaitStatus(path); err == nil {
		t.Fatal("invalid status was accepted")
	}
}

func TestUnsafeNoHostIsolationRequiresExplicitOptIn(t *testing.T) {
	opts, remaining, done, err := parseGlobalOptions([]string{"run", "busybox"}, io.Discard)
	if err != nil || done || opts.unsafeNoHostIsolation || !reflect.DeepEqual(remaining, []string{"run", "busybox"}) {
		t.Fatalf("default options=(%+v, %q, %t, %v), want hardened launch", opts, remaining, done, err)
	}
	opts, remaining, done, err = parseGlobalOptions([]string{"--unsafe-no-host-isolation", "run", "busybox"}, io.Discard)
	if err != nil || done || !opts.unsafeNoHostIsolation || !reflect.DeepEqual(remaining, []string{"run", "busybox"}) {
		t.Fatalf("unsafe options=(%+v, %q, %t, %v), want explicit opt-in", opts, remaining, done, err)
	}

	wantHardened := []string{"--root", "/runtime", "--rootless=true", "--network=none", "--directfs=false", "--overlay2=all:memory"}
	if got := (&runtimeBackend{root: "/runtime"}).configuredRunscArgs(); !reflect.DeepEqual(got, wantHardened) {
		t.Errorf("hardened runsc args=%q, want %q", got, wantHardened)
	}
	wantHost := []string{"--root", "/runtime", "--rootless=true", "--network=host", "--directfs=false", "--overlay2=all:memory"}
	if got := (&runtimeBackend{root: "/runtime"}).configuredRunscArgsForNetwork("host"); !reflect.DeepEqual(got, wantHost) {
		t.Errorf("host-network runsc args=%q, want %q", got, wantHost)
	}
	wantUnsafe := []string{
		"--root", "/runtime", "--rootless=false", "--unsafe-no-host-isolation=true",
		"--network=none", "--directfs=false", "--overlay2=all:memory",
	}
	if got := (&runtimeBackend{root: "/runtime", unsafeNoHostIsolation: true}).configuredRunscArgs(); !reflect.DeepEqual(got, wantUnsafe) {
		t.Errorf("unsafe runsc args=%q, want %q", got, wantUnsafe)
	}
}

func TestRunConfiguredCommandDoesNotInheritCallerPipes(t *testing.T) {
	dir := t.TempDir()
	runsc := filepath.Join(dir, "runsc")
	script := `#!/bin/sh
echo discarded-output
echo launch-diagnostic >&2
sleep 3 &
exit 0
`
	if err := os.WriteFile(runsc, []byte(script), 0755); err != nil {
		t.Fatalf("writing fake runsc: %v", err)
	}
	var stdout, stderr bytes.Buffer
	b := &runtimeBackend{root: "/runtime", runsc: runsc, stdout: &stdout, stderr: &stderr}
	started := time.Now()
	status, err := b.runConfiguredCommand("start", "demo")
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("launch waited %v for a descendant holding stdio", elapsed)
	}
	if err != nil || status != 0 {
		t.Fatalf("launch returned status=%d err=%v", status, err)
	}
	if stdout.Len() != 0 {
		t.Errorf("runsc launch stdout leaked to container output: %q", stdout.String())
	}
	if got, want := stderr.String(), "launch-diagnostic\n"; got != want {
		t.Errorf("runsc launch diagnostic=%q, want %q", got, want)
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
	if [ -e "$0.stopped" ]; then
		echo '[{"id":"web","pid":0,"status":"stopped"}]'
	else
		echo '[{"id":"web","pid":4321,"status":"running"}]'
	fi
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
if [ "$3" = "timetravel" ] && [ "$4" = "web" ] && [ "$5" = "exec-list" ] && [ "$6" = "child" ]; then
	echo '[]'
	exit 0
fi

if [ "$3" = "timetravel" ] && [ "$4" = "--console" ] && [ "$5" = "init" ] && [ "$6" = "--console-log" ] && [ "$8" = "--console-log-max-bytes" ] && [ "$9" = "4096" ] && [ "${10}" = "--console-log-max-files" ] && [ "${11}" = "3" ] && [ "${12}" = "--console-wait-status" ] && [ "${14}" = "--console-ready-fd" ] && [ "${15}" = "4" ] && [ "${16}" = "web" ] && [ "${17}" = "console-follow" ] && [ "${18}" = "child" ]; then
	touch "$0.retention"
	printf 1 >&4
	exit 0
fi
if [ "$3" = "timetravel" ] && [ "$4" = "web" ] && [ "$5" = "ps" ] && [ "$6" = "main" ]; then
	echo '[{"uid":0,"pid":1,"ppid":0,"pgid":1,"threads":[1],"c":0,"tty":"?","stime":"12:00","time":"1ms","cmd":"init"}]'
	exit 0
fi
if [ "$3" = "timetravel" ] && [ "$4" = "web" ] && [ "$5" = "ps" ] && [ "$6" = "child" ]; then
	echo '[{"uid":1000,"pid":7,"ppid":1,"pgid":7,"threads":[7,8],"c":3,"tty":"?","stime":"12:34","time":"9ms","cmd":"worker"}]'
	exit 0
fi
if [ "$3" = "timetravel" ] && [ "$4" = "web" ] && [ "$5" = "check" ] && [ "$6" = "child" ]; then
	echo '{"eligible":true}'
	exit 0
fi
if [ "$3" = "timetravel" ] && [ "$4" = "--signal" ] && [ "$5" = "15" ] && [ "$6" = "web" ] && [ "$7" = "signal" ] && [ "$8" = "child" ]; then
	exit 0
fi
if [ "$3" = "timetravel" ] && [ "$4" = "web" ] && [ "$5" = "wait" ] && [ "$6" = "child" ]; then
	echo '{"wait_status":15}'
	exit 0
fi
if [ "$3" = "timetravel" ] && [ "$4" = "web" ] && [ "$5" = "wait" ] && [ "$6" = "main" ]; then
	exit 128
fi
echo "unexpected command: $*" >&2
exit 2
`
	if err := os.WriteFile(runsc, []byte(script), 0755); err != nil {
		t.Fatalf("writing fake runsc: %v", err)
	}
	dataRoot := filepath.Join(dir, "data")
	containerDir := filepath.Join(dataRoot, "containers", "web")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatalf("creating bundle: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	metadata, err := json.Marshal(ttosbundle.ContainerInfo{Name: "web", BundleDir: bundleDir, ConsoleLogMaxBytes: 4096, ConsoleLogMaxFiles: 3, State: "running", CreatedAt: time.Now()})
	if err != nil {
		t.Fatalf("encoding metadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(containerDir, "container.json"), metadata, 0600); err != nil {
		t.Fatalf("writing metadata: %v", err)
	}
	consoleDir := filepath.Join(containerDir, "console")
	if err := os.MkdirAll(consoleDir, 0700); err != nil {
		t.Fatalf("creating console directory: %v", err)
	}
	staleRotatedLog := filepath.Join(consoleDir, "child.log"+ttosconsole.RotatedSuffix)
	if err := os.WriteFile(staleRotatedLog, []byte("stale"), 0600); err != nil {
		t.Fatalf("writing stale rotated log: %v", err)
	}
	staleOlderLog := ttosconsole.GenerationPath(filepath.Join(consoleDir, "child.log"), 2)
	if err := os.WriteFile(staleOlderLog, []byte("older"), 0600); err != nil {
		t.Fatalf("writing older stale log: %v", err)
	}
	b := &runtimeBackend{root: "/runtime", runsc: runsc, dataRoot: dataRoot}
	families, err := b.listFamilies()
	if err != nil {
		t.Fatalf("listFamilies: %v", err)
	}
	if len(families) != 1 || families[0].ID != "web" || len(families[0].Branches) != 1 || families[0].Branches[0].ID != "main" {
		t.Fatalf("listFamilies returned %+v", families)
	}
	inspected, err := b.inspect("web")
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if inspected.State.Status != "running" || inspected.State.Pid != 4321 || len(inspected.TTOS.Branches) != 1 || len(inspected.TTOS.Branches[0].Processes) != 1 || inspected.TTOS.Branches[0].Processes[0].PID != 1 {
		t.Errorf("runtime-enriched inspect=%+v", inspected)
	}
	child, err := b.fork(selector{container: "web", branch: "main"}, "child", true)
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if child.ID != "child" || child.ParentID != "main" || child.State != "paused" || child.ForkTotalNS != 3_000_000 || child.KernelForkNS != 2_000_000 {
		t.Errorf("fork returned %+v", child)
	}
	if _, err := os.Stat(filepath.Join(containerDir, "console", "child.log")); err != nil {
		t.Errorf("fork console log: %v", err)
	}
	if _, err := os.Stat(staleRotatedLog); !os.IsNotExist(err) {
		t.Errorf("stale rotated log still exists: %v", err)
	}
	if _, err := os.Stat(staleOlderLog); !os.IsNotExist(err) {
		t.Errorf("older stale log still exists: %v", err)
	}
	marker := runsc + ".retention"
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("console sidecar did not receive persisted retention flags")
		}
		time.Sleep(time.Millisecond)
	}

	processes, err := b.processes(selector{container: "web", branch: "child"})
	if err != nil {
		t.Fatalf("processes: %v", err)
	}
	if len(processes) != 1 || processes[0].PID != 7 || processes[0].UID != 1000 || processes[0].Cmd != "worker" {
		t.Errorf("processes returned %+v", processes)
	}
	eligibility, err := b.checkFork(selector{container: "web", branch: "child"})
	if err != nil {
		t.Fatalf("checkFork: %v", err)
	}
	if !eligibility.Eligible || len(eligibility.Issues) != 0 {
		t.Errorf("checkFork returned %+v", eligibility)
	}
	if err := b.signal(selector{container: "web", branch: "child"}, 15, false); err != nil {
		t.Fatalf("signal: %v", err)
	}
	status, err := b.wait(selector{container: "web", branch: "child"})
	if err != nil || status != 15 {
		t.Errorf("wait returned (%d, %v), want (15, nil)", status, err)
	}
	store := ttosbundle.Store{Root: dataRoot}
	mainConsole, err := store.BranchConsolePaths("web", "main")
	if err != nil {
		t.Fatalf("BranchConsolePaths(main): %v", err)
	}
	if err := os.WriteFile(mainConsole.WaitStatus, []byte("9\n"), 0600); err != nil {
		t.Fatalf("writing persistent main wait status: %v", err)
	}
	status, err = b.wait(selector{container: "web", branch: "main"})
	if err != nil || status != 9 {
		t.Errorf("wait fallback returned (%d, %v), want (9, nil)", status, err)
	}
	persisted, err := store.LoadContainer("web")
	if err != nil {
		t.Fatalf("LoadContainer after main wait: %v", err)
	}
	if persisted.State != "exited" || persisted.ExitStatus == nil || *persisted.ExitStatus != 137 || persisted.ExitedAt == nil {
		t.Errorf("main wait did not reconcile persistent state: %+v", persisted)
	}
	if err := b.destroy(selector{container: "web", branch: "child"}); err == nil {
		t.Error("destroy swallowed a live-sandbox control error")
	}
	if err := os.WriteFile(runsc+".stopped", nil, 0600); err != nil {
		t.Fatalf("marking fake sandbox stopped: %v", err)
	}
	if err := b.destroy(selector{container: "web", branch: "child"}); err != nil {
		t.Errorf("destroy after sandbox exit: %v", err)
	}
	if err := b.destroy(selector{container: "missing", branch: "child"}); err == nil {
		t.Error("destroy accepted a nonexistent persistent container")
	}
}

func TestRuntimeBackendResolvesStableContainerID(t *testing.T) {
	dir := t.TempDir()
	id := "1234567890ab" + strings.Repeat("c", 52)
	runsc := filepath.Join(dir, "runsc")
	script := `#!/bin/sh
id="` + id + `"
if [ "$1" != "--root" ] || [ "$2" != "/runtime" ]; then
	echo "unexpected globals: $*" >&2
	exit 2
fi
shift 2
case "$1 $2 $3 $4" in
"list --format=json --sandbox ")
	printf "[{\"id\":\"%s\",\"pid\":2468,\"status\":\"running\"}]\n" "$id"
	;;
"timetravel $id branches ")
	echo "[{\"ID\":\"main\",\"ParentID\":\"\",\"State\":\"running\",\"ForkMetrics\":{\"SourceDowntime\":0,\"Kernel\":{\"Total\":0},\"Total\":0}}]"
	;;
"timetravel $id ps main")
	echo "[{\"uid\":0,\"pid\":1,\"ppid\":0,\"pgid\":1,\"threads\":[1],\"c\":0,\"tty\":\"?\",\"stime\":\"12:00\",\"time\":\"1ms\",\"cmd\":\"init\"}]"
	;;
"timetravel $id check main")
	echo "{\"eligible\":true}"
	;;
"timetravel $id pause main"|"timetravel $id resume main")
	;;
"delete --force $id ")
	;;
*)
	echo "unexpected command: $*" >&2
	exit 2
	;;
esac
`
	if err := os.WriteFile(runsc, []byte(script), 0755); err != nil {
		t.Fatalf("writing fake runsc: %v", err)
	}
	dataRoot := filepath.Join(dir, "data")
	containerDir := filepath.Join(dataRoot, "containers", id)
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatalf("creating bundle: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	metadata, err := json.Marshal(ttosbundle.ContainerInfo{
		ID: id, Name: "web", BundleDir: bundleDir, State: "running", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("encoding metadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(containerDir, "container.json"), metadata, 0600); err != nil {
		t.Fatalf("writing metadata: %v", err)
	}

	b := &runtimeBackend{root: "/runtime", runsc: runsc, dataRoot: dataRoot}
	families, err := b.listFamilies()
	if err != nil {
		t.Fatalf("listFamilies: %v", err)
	}
	if len(families) != 1 || families[0].ID != id || families[0].Name != "web" || families[0].SandboxPID != 2468 {
		t.Fatalf("stable family=%+v", families)
	}
	for _, identifier := range []string{"web", id[:16]} {
		info, err := b.inspect(identifier)
		if err != nil {
			t.Fatalf("inspect(%q): %v", identifier, err)
		}
		if info.ID != id || info.Name != "/web" || len(info.TTOS.Branches) != 1 || len(info.TTOS.Branches[0].Processes) != 1 {
			t.Errorf("inspect(%q)=%+v", identifier, info)
		}
	}
	target := selector{container: "web", branch: "main"}
	if err := b.pause(target); err != nil {
		t.Fatalf("pause by name: %v", err)
	}
	if err := b.resume(selector{container: id[:16], branch: "main"}); err != nil {
		t.Fatalf("resume by ID prefix: %v", err)
	}
	if eligibility, err := b.checkFork(target); err != nil || !eligibility.Eligible {
		t.Fatalf("checkFork by name=(%+v, %v)", eligibility, err)
	}
	if err := b.remove("web", true); err != nil {
		t.Fatalf("remove by name: %v", err)
	}
	if _, err := os.Stat(containerDir); !os.IsNotExist(err) {
		t.Errorf("removed ID-keyed state still exists: %v", err)
	}
}

func TestRuntimeBackendExecUsesBranchFlag(t *testing.T) {
	dir := t.TempDir()
	runsc := filepath.Join(dir, "runsc")
	script := `#!/bin/sh
if [ "$1" = "--root" ] && [ "$2" = "/runtime" ] &&
   [ "$3" = "exec" ] && [ "$4" = "--branch" ] && [ "$5" = "child" ] &&
   [ "$6" = "--env" ] && [ "$7" = "A=1" ] &&
   [ "$8" = "--cwd" ] && [ "$9" = "/work" ] &&
   [ "${10}" = "--user" ] && [ "${11}" = "123:456" ] &&
   [ "${12}" = "--additional-gids" ] && [ "${13}" = "12" ] &&
   [ "${14}" = "--additional-gids" ] && [ "${15}" = "34" ] &&
   [ "${16}" = "--cap" ] && [ "${17}" = "CAP_CHOWN" ] &&
   [ "${18}" = "--cap" ] && [ "${19}" = "CAP_SYS_PTRACE" ] &&
   [ "${20}" = "web" ] && [ "${21}" = "echo" ] && [ "${22}" = "ok" ]; then
	echo branch-output
	exit 0
fi
echo "unexpected command: $*" >&2
exit 2
`
	if err := os.WriteFile(runsc, []byte(script), 0755); err != nil {
		t.Fatalf("writing fake runsc: %v", err)
	}
	var stdout, stderr bytes.Buffer
	b := &runtimeBackend{root: "/runtime", runsc: runsc, stdout: &stdout, stderr: &stderr}
	status, err := b.exec(execRequest{
		target:         selector{container: "web", branch: "child"},
		args:           []string{"echo", "ok"},
		env:            []string{"A=1"},
		cwd:            "/work",
		user:           "123:456",
		additionalGIDs: []string{"12", "34"},
		capabilities:   []string{"CAP_CHOWN", "CAP_SYS_PTRACE"},
	})
	if err != nil || status != 0 {
		t.Fatalf("exec returned status=%d err=%v stderr=%q", status, err, stderr.String())
	}
	if stdout.String() != "branch-output\n" {
		t.Errorf("stdout=%q", stdout.String())
	}
}

func TestRuntimeBackendExecForwardsProcessFile(t *testing.T) {
	dir := t.TempDir()
	runsc := filepath.Join(dir, "runsc")
	script := `#!/bin/sh
if [ "$1" = "--root" ] && [ "$2" = "/runtime" ] &&
   [ "$3" = "exec" ] && [ "$4" = "--branch" ] && [ "$5" = "child" ] &&
   [ "$6" = "--process" ] && [ "$7" = "/process.json" ] &&
   [ "$8" = "web" ] && [ -z "$9" ]; then
	exit 0
fi
echo "unexpected command: $*" >&2
exit 2
`
	if err := os.WriteFile(runsc, []byte(script), 0755); err != nil {
		t.Fatalf("writing fake runsc: %v", err)
	}
	var stderr bytes.Buffer
	b := &runtimeBackend{root: "/runtime", runsc: runsc, stderr: &stderr}
	status, err := b.exec(execRequest{
		target:      selector{container: "web", branch: "child"},
		processPath: "/process.json",
	})
	if err != nil || status != 0 {
		t.Fatalf("exec --process returned status=%d err=%v stderr=%q", status, err, stderr.String())
	}
}

func TestRemoveStoppedRuntimeBeforeStart(t *testing.T) {
	dir := t.TempDir()
	runsc := filepath.Join(dir, "runsc")
	script := `#!/bin/sh
if [ "$1" != "--root" ] || [ "$2" != "/runtime" ]; then
	echo "unexpected globals: $*" >&2
	exit 2
fi
if [ "$3" = "list" ]; then
	case "$(cat "$0.mode")" in
	stopped) echo '[{"id":"web","status":"stopped"}]' ;;
	running) echo '[{"id":"web","status":"running"}]' ;;
	absent) echo '[]' ;;
	esac
	exit 0
fi
if [ "$3" = "delete" ] && [ "$4" = "--force" ] && [ "$5" = "web" ]; then
	touch "$0.deleted"
	exit 0
fi
echo "unexpected command: $*" >&2
exit 2
`
	if err := os.WriteFile(runsc, []byte(script), 0755); err != nil {
		t.Fatalf("writing fake runsc: %v", err)
	}
	b := &runtimeBackend{root: "/runtime", runsc: runsc}
	if err := os.WriteFile(runsc+".mode", []byte("stopped\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := b.removeStoppedRuntime("web"); err != nil {
		t.Fatalf("removeStoppedRuntime(stopped): %v", err)
	}
	if _, err := os.Stat(runsc + ".deleted"); err != nil {
		t.Errorf("stopped runtime was not deleted: %v", err)
	}
	if err := os.Remove(runsc + ".deleted"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runsc+".mode", []byte("running\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := b.removeStoppedRuntime("web"); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("removeStoppedRuntime(running)=%v, want already-running error", err)
	}
	if _, err := os.Stat(runsc + ".deleted"); !os.IsNotExist(err) {
		t.Errorf("running runtime delete marker err=%v", err)
	}
}

func TestListFamiliesMergesPersistentStateAndReconcilesStaleRunning(t *testing.T) {
	dir := t.TempDir()
	runsc := filepath.Join(dir, "runsc")
	if err := os.WriteFile(runsc, []byte(`#!/bin/sh
if [ "$3" = "list" ]; then
	echo '[]'
	exit 0
fi
exit 2
`), 0755); err != nil {
		t.Fatalf("writing fake runsc: %v", err)
	}

	dataRoot := filepath.Join(dir, "data")
	bundleDir := filepath.Join(dataRoot, "containers", "demo", "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatalf("creating bundle: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	createdAt := time.Date(2026, time.July, 17, 1, 2, 3, 0, time.UTC)
	metadata, err := json.Marshal(ttosbundle.ContainerInfo{
		Name:      "demo",
		Reference: "docker.io/library/alpine:3.20",
		Digest:    "sha256:abcd",
		BundleDir: bundleDir,
		State:     "created",
		CreatedAt: createdAt,
		Volumes:   []ttosbundle.ManagedVolume{{Name: "cache", Destination: "/cache"}},
	})
	if err != nil {
		t.Fatalf("encoding metadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataRoot, "containers", "demo", "container.json"), metadata, 0600); err != nil {
		t.Fatalf("writing metadata: %v", err)
	}

	b := &runtimeBackend{root: "/runtime", runsc: runsc, dataRoot: dataRoot}
	families, err := b.listFamilies()
	if err != nil {
		t.Fatalf("listFamilies: %v", err)
	}
	if len(families) != 1 || families[0].ID != "demo" || families[0].State != "created" || families[0].Branches[0].State != "created" {
		t.Fatalf("listFamilies returned %+v", families)
	}

	if families[0].Image != "docker.io/library/alpine:3.20" || families[0].Digest != "sha256:abcd" || !families[0].CreatedAt.Equal(createdAt) || !families[0].Branches[0].CreatedAt.Equal(createdAt) || !reflect.DeepEqual(families[0].Volumes, []string{"cache"}) {
		t.Errorf("persistent family metadata=%+v", families[0])
	}
	store := ttosbundle.Store{Root: dataRoot}
	if err := store.UpdateState("demo", "running", nil); err != nil {
		t.Fatalf("marking container running: %v", err)
	}
	families, err = b.listFamilies()
	if err != nil {
		t.Fatalf("reconciling listFamilies: %v", err)
	}
	if len(families) != 1 || families[0].State != "exited" || families[0].Branches[0].State != "exited" {
		t.Fatalf("reconciled listFamilies returned %+v", families)
	}
	info, err := store.LoadContainer("demo")
	if families[0].Image == "" || !families[0].Branches[0].CreatedAt.Equal(createdAt) {
		t.Errorf("reconciliation lost persistent metadata: %+v", families[0])
	}
	if err != nil {
		t.Fatalf("loading reconciled metadata: %v", err)
	}
	if info.State != "exited" || info.ExitedAt == nil {
		t.Errorf("reconciled metadata = %+v, want exited with timestamp", info)
	}
}

type signalCall struct {
	target selector
	signal int
	all    bool
}

type startCall struct {
	name        string
	detach      bool
	interactive bool
}

type fakeBackend struct {
	mu                sync.Mutex
	families          []Family
	processTarget     selector
	processList       []ProcessInfo
	checkTarget       selector
	checkResult       ForkEligibility
	err               error
	forkSource        selector
	forkDestination   string
	forkPaused        bool
	paused            selector
	resumed           selector
	destroyed         selector
	signals           []signalCall
	waited            selector
	waitCalls         []selector
	waitStatus        uint32
	waitStatuses      map[selector]uint32
	waitRelease       chan struct{}
	pullReference     string
	pullPlatform      string
	pullSnapshotter   string
	pullUnpack        bool
	pullInfo          ttosimages.Info
	imageList         []ImageInfo
	imageInspectID    string
	imageInspectInfo  ImageInspectInfo
	imageRemoveID     string
	imageRemoveForce  bool
	imageRemoveResult ImageRemoveResult
	runRequest        runRequest
	containerID       string
	runStatus         int
	created           bool
	started           string
	startCalls        []startCall
	startDetach       bool
	startInteractive  bool
	attached          attachRequest
	logsTarget        selector
	logsExecID        string
	logsFollow        bool
	logsTail          int
	inspectInfo       InspectInfo
	removed           string
	removeForce       bool
	execRequest       execRequest
	execStatus        int
	execSessionList   []runscExecSession
	execControlTarget selector
	execControlID     string
	execControlSignal int

	volumes       []ttosbundle.VolumeInfo
	ensuredVolume string
	removedVolume string
}

func (b *fakeBackend) resultID(request runRequest) string {
	if b.containerID != "" {
		return b.containerID
	}
	return request.name
}

func (b *fakeBackend) create(request runRequest) (string, error) {
	b.runRequest = request
	b.created = true
	return b.resultID(request), b.err
}

func (b *fakeBackend) run(request runRequest) (containerRunResult, error) {
	b.runRequest = request
	return containerRunResult{ID: b.resultID(request), Status: b.runStatus}, b.err
}

func (b *fakeBackend) start(name string, detach, interactive bool) (int, error) {
	b.startCalls = append(b.startCalls, startCall{name: name, detach: detach, interactive: interactive})
	b.started = name
	b.startDetach = detach
	b.startInteractive = interactive
	return b.runStatus, b.err
}

func (b *fakeBackend) attach(request attachRequest) (uint32, error) {
	b.attached = request
	b.attached.detachKeys = append([]byte(nil), request.detachKeys...)
	return b.waitStatus, b.err
}

func (b *fakeBackend) logs(target selector, execID string, follow bool, tail int) error {
	b.logsTarget = target
	b.logsExecID = execID
	b.logsFollow = follow
	b.logsTail = tail
	return b.err
}

func (b *fakeBackend) exec(request execRequest) (int, error) {
	b.execRequest = request
	return b.execStatus, b.err
}

func (b *fakeBackend) execSessions(target selector) ([]runscExecSession, error) {
	b.execControlTarget = target
	return append([]runscExecSession(nil), b.execSessionList...), b.err
}

func (b *fakeBackend) waitExecSession(target selector, execID string) (uint32, error) {
	b.execControlTarget = target
	b.execControlID = execID
	return b.waitStatus, b.err
}

func (b *fakeBackend) signalExecSession(target selector, execID string, signal int) error {
	b.execControlTarget = target
	b.execControlID = execID
	b.execControlSignal = signal
	return b.err
}

func (b *fakeBackend) inspect(name string) (InspectInfo, error) {
	return b.inspectInfo, b.err
}

func (b *fakeBackend) remove(name string, force bool) error {
	b.removed = name
	b.removeForce = force
	return b.err
}

func (b *fakeBackend) pull(reference, platform, snapshotter string, unpack bool) (ttosimages.Info, error) {
	b.pullReference = reference
	b.pullPlatform = platform
	b.pullSnapshotter = snapshotter
	b.pullUnpack = unpack
	return b.pullInfo, b.err
}

func (b *fakeBackend) listImages() ([]ImageInfo, error) {
	return b.imageList, b.err
}

func (b *fakeBackend) inspectImage(identifier string) (ImageInspectInfo, error) {
	b.imageInspectID = identifier
	return b.imageInspectInfo, b.err
}

func (b *fakeBackend) removeImage(identifier string, force bool) (ImageRemoveResult, error) {
	b.imageRemoveID = identifier
	b.imageRemoveForce = force
	return b.imageRemoveResult, b.err
}

func (b *fakeBackend) listFamilies() ([]Family, error) {
	return b.families, b.err
}

func (b *fakeBackend) processes(target selector) ([]ProcessInfo, error) {
	b.processTarget = target
	return b.processList, b.err
}

func (b *fakeBackend) checkFork(target selector) (ForkEligibility, error) {
	b.checkTarget = target
	return b.checkResult, b.err
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

func (b *fakeBackend) signal(target selector, signal int, all bool) error {
	b.signals = append(b.signals, signalCall{target: target, signal: signal, all: all})
	if signal == 9 && b.waitRelease != nil {
		close(b.waitRelease)
	}
	return b.err
}

func (b *fakeBackend) wait(target selector) (uint32, error) {
	b.mu.Lock()
	b.waited = target
	b.waitCalls = append(b.waitCalls, target)
	status := b.waitStatus
	if value, ok := b.waitStatuses[target]; ok {
		status = value
	}
	release := b.waitRelease
	err := b.err
	b.mu.Unlock()
	if release != nil {
		<-release
	}
	return status, err
}

func (b *fakeBackend) ensureVolume(name string) (ttosbundle.VolumeInfo, error) {
	b.ensuredVolume = name
	if b.err != nil {
		return ttosbundle.VolumeInfo{}, b.err
	}
	for _, volume := range b.volumes {
		if volume.Name == name {
			return volume, nil
		}
	}
	volume := ttosbundle.VolumeInfo{Name: name, Driver: "local-tmpfs"}
	b.volumes = append(b.volumes, volume)
	return volume, nil
}

func (b *fakeBackend) listVolumes() ([]ttosbundle.VolumeInfo, error) {
	return append([]ttosbundle.VolumeInfo(nil), b.volumes...), b.err
}

func (b *fakeBackend) inspectVolume(name string) (ttosbundle.VolumeInfo, error) {
	for _, volume := range b.volumes {
		if volume.Name == name {
			return volume, b.err
		}
	}
	return ttosbundle.VolumeInfo{}, fmt.Errorf("volume %q does not exist", name)
}

func (b *fakeBackend) removeVolume(name string) error {
	b.removedVolume = name
	return b.err
}
