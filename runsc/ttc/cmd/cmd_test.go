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
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
	"gvisor.dev/gvisor/runsc/ttc/cluster"
	ttcconsole "gvisor.dev/gvisor/runsc/ttc/console"
	ttcimages "gvisor.dev/gvisor/runsc/ttc/images"
)

func writeCurrentBundleFixture(path string, input ttcbundle.ContainerInfo) error {
	info := input
	info.Branches = append([]ttcbundle.BranchInfo(nil), input.Branches...)
	if info.ID == "" {
		info.ID = filepath.Base(filepath.Dir(path))
	}
	if info.State == "" {
		info.State = "created"
	}
	if len(info.Branches) == 0 {
		info.Branches = []ttcbundle.BranchInfo{{ID: "main", CreatedAt: info.CreatedAt}}
	}
	for i := range info.Branches {
		branch := &info.Branches[i]
		if branch.FilesystemBranch == "" {
			branch.FilesystemBranch = branch.ID
		}
		if branch.State == "" {
			branch.State = info.State
		}
		if branch.StorageState == "" {
			branch.StorageState = "local"
		}
	}
	if !info.LiveForkEligible && info.LiveForkWaiver == "" {
		info.LiveForkEligible = true
	}
	encoded, err := json.Marshal(info)
	if err != nil {
		return err
	}
	return os.WriteFile(path, encoded, 0600)
}

func TestDistributedPageRelayAcceptsPastIdleFilesystemStream(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	relay := newDistributedPageRelay(listener, nil, "source", "session")
	defer relay.Close()
	idle, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	active, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	if _, err := active.Write([]byte("TTCF")); err != nil {
		t.Fatal(err)
	}
	if err := active.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var one [1]byte
	if _, err := active.Read(one[:]); !errors.Is(err, io.EOF) && !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("second stream was not classified while first stayed idle: %v", err)
	}
}

func TestDockerCompatibleHelpIsLocalAndSuccessful(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "ttcd.sock")
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{"flag command", []string{"run", "--help"}, "Usage of run:"},
		{"container namespace", []string{"container", "--help"}, "Usage: ttc container COMMAND"},
		{"container subcommand", []string{"container", "run", "--help"}, "Usage of run:"},
		{"pause", []string{"pause", "--help"}, "Usage: ttc pause"},
		{"unpause", []string{"unpause", "--help"}, "Usage: ttc unpause"},
		{"top", []string{"top", "--help"}, "Usage: ttc top"},
		{"cp", []string{"cp", "--help"}, "Usage: ttc cp"},
		{"events", []string{"events", "--help"}, "Usage: ttc events"},
		{"image inspect", []string{"image", "inspect", "--help"}, "Usage: ttc image inspect"},
		{"image load", []string{"image", "load", "--help"}, "Usage: ttc image load"},
		{"image save", []string{"image", "save", "--help"}, "Usage: ttc image save"},
		{"commit", []string{"commit", "--help"}, "Usage: ttc commit"},
		{"exec wait", []string{"exec", "wait", "--help"}, "Usage: ttc exec wait"},
		{"daemon", []string{"daemon", "--help"}, "Usage: ttc daemon"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			args := append([]string{"--daemon-socket", socket}, test.args...)
			if status := Run(args, strings.NewReader(""), &stdout, &stderr); status != 0 {
				t.Fatalf("status=%d, stdout=%q, stderr=%q", status, stdout.String(), stderr.String())
			}
			if !strings.Contains(stdout.String(), test.want) {
				t.Fatalf("stdout=%q, want %q", stdout.String(), test.want)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr=%q", stderr.String())
			}
			if _, err := os.Stat(socket); !os.IsNotExist(err) {
				t.Fatalf("help created daemon socket: %v", err)
			}
		})
	}
}

func TestQuarantineCommandsAddressBranchState(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"quarantine", "web@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("quarantine status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := b.quarantineTarget, (selector{container: "web", branch: "child"}); got != want || !b.quarantineEnabled {
		t.Fatalf("quarantine call=(%v, %t), want (%v, true)", got, b.quarantineEnabled, want)
	}
	if got, want := stdout.String(), "web@child\n"; got != want {
		t.Fatalf("quarantine output=%q, want %q", got, want)
	}

	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"unquarantine", "web@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("unquarantine status=%d, stderr=%q", status, stderr.String())
	}
	if b.quarantineTarget != (selector{container: "web", branch: "child"}) || b.quarantineEnabled {
		t.Fatalf("unquarantine call=(%v, %t)", b.quarantineTarget, b.quarantineEnabled)
	}
}

func TestMigrateCommand(t *testing.T) {
	backend := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"migrate", "--node", "node-b", "web@main"}, &stdout, &stderr, backend); status != 0 {
		t.Fatalf("status=%d stderr=%q", status, stderr.String())
	}
	if backend.migrateTarget.String() != "web@main" || backend.migrateNode != "node-b" || stdout.String() != "web@main\n" {
		t.Fatalf("migration=(%s, %q) stdout=%q", backend.migrateTarget, backend.migrateNode, stdout.String())
	}
}

func TestTTYStatRoutesNamedSessionAndJSON(t *testing.T) {
	backend := &fakeBackend{ttyObservation: kernel.InternalConsoleObservation{
		Version: 1, InputPendingBytes: 7, Canonical: true, ForegroundPGID: 42,
	}}
	var stdout bytes.Buffer
	if err := runTTY([]string{"stat", "demo@main", "--session", "build1", "--json"}, &stdout, backend); err != nil {
		t.Fatal(err)
	}
	if backend.ttyTarget != (selector{container: "demo", branch: "main"}) || backend.ttySession != "build1" {
		t.Fatalf("tty route target=%v session=%q", backend.ttyTarget, backend.ttySession)
	}
	var got kernel.InternalConsoleObservation
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 || got.InputPendingBytes != 7 || !got.Canonical || got.ForegroundPGID != 42 {
		t.Fatalf("tty observation=%+v", got)
	}
}

func TestTTYStatHumanOutputIncludesCausalFields(t *testing.T) {
	backend := &fakeBackend{ttyObservation: kernel.InternalConsoleObservation{
		Version: 1, SessionEpoch: 9, InputConsumedBytes: 17,
		TermiosGeneration: 2, ForegroundPGIDGeneration: 3,
		InputFlushGeneration: 4, ReplicaOpenCount: 5,
	}}
	var stdout bytes.Buffer
	if err := runTTY([]string{"stat", "demo@main", "--session", "build1"}, &stdout, backend); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"session_epoch=9", "input_consumed=17", "termios_generation=2",
		"foreground_generation=3", "input_flush_generation=4", "replica_open_count=5",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("human observation %q does not contain %q", stdout.String(), want)
		}
	}
}

func TestStatsJSONIncludesFamilyTotals(t *testing.T) {
	backend := &fakeBackend{resourceStatsResult: FamilyResourceStats{
		Container: "demo", ResidentMemoryUniqueBytes: 4096, FilesystemUniqueBytes: 8192,
		ImageCacheBytes: 16384, ImageCacheTotalBytes: 32768,
		SnapshotGraph: &SnapshotGraphResourceStats{Sequence: 7, Objects: 11, FamilyReachableObjects: 5, PendingReferenceOperations: 1},
		Branches: []BranchResourceStats{{
			Target: "demo@main", State: "running", NodeID: "node-a", Quarantined: true,
			SnapshotID: 3, MemoryRoots: []string{"memory/root"}, MemoryRootObjects: []string{"object-3"}, FilesystemRoot: "filesystem-root",
			ResidentMemory: ResourceBytes{PrivateBytes: 4096, TotalBytes: 4096},
		}},
	}}
	var stdout bytes.Buffer
	if err := runStats([]string{"--json", "demo@main"}, &stdout, io.Discard, backend); err != nil {
		t.Fatal(err)
	}
	if backend.resourceStatsTarget != (selector{container: "demo", branch: "main"}) {
		t.Fatalf("stats target=%v", backend.resourceStatsTarget)
	}
	var got FamilyResourceStats
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ResidentMemoryUniqueBytes != 4096 || got.FilesystemUniqueBytes != 8192 || got.ImageCacheBytes != 16384 || got.ImageCacheTotalBytes != 32768 || len(got.Branches) != 1 ||
		got.SnapshotGraph == nil || got.SnapshotGraph.Sequence != 7 || got.SnapshotGraph.FamilyReachableObjects != 5 || got.Branches[0].SnapshotID != 3 || got.Branches[0].NodeID != "node-a" || !got.Branches[0].Quarantined || got.Branches[0].FilesystemRoot != "filesystem-root" {
		t.Fatalf("stats=%+v", got)
	}
}

func TestParseSelector(t *testing.T) {
	tests := []struct {
		input string
		want  selector
		err   bool
	}{
		{input: "web", err: true},
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

func TestPortCommandsAreBranchAware(t *testing.T) {
	b := &fakeBackend{portList: []ttcbundle.PortBinding{
		{HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 80, Protocol: "tcp", BranchID: "main"},
		{HostIP: "127.0.0.1", HostPort: 8081, ContainerPort: 80, Protocol: "tcp", BranchID: "child"},
	}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"port", "publish", "web@child", "127.0.0.1:8082:8080"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("port publish status=%d, stderr=%q", status, stderr.String())
	}
	if b.publishedTarget != (selector{container: "web", branch: "child"}) || b.publishedBinding != (ttcbundle.PortBinding{HostIP: "127.0.0.1", HostPort: 8082, ContainerPort: 8080, Protocol: "tcp", BranchID: "child"}) {
		t.Errorf("publish=(%+v, %+v)", b.publishedTarget, b.publishedBinding)
	}
	stdout.Reset()
	if status := runCommand([]string{"port", "ls", "web@main"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("port ls status=%d, stderr=%q", status, stderr.String())
	}
	if !strings.Contains(stdout.String(), "web@main") || strings.Contains(stdout.String(), "web@child") {
		t.Errorf("port ls output=%q", stdout.String())
	}
	stdout.Reset()
	if status := runCommand([]string{"port", "ls", "--json", "web@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("port ls child status=%d, stderr=%q", status, stderr.String())
	}
	if strings.Contains(stdout.String(), `"branch_id": "main"`) || !strings.Contains(stdout.String(), `"branch_id": "child"`) {
		t.Errorf("filtered port JSON=%q", stdout.String())
	}
	stdout.Reset()
	if status := runCommand([]string{"port", "unpublish", "web@child", "127.0.0.1:8081:80/tcp"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("port unpublish status=%d, stderr=%q", status, stderr.String())
	}
	if b.unpublishedTarget != (selector{container: "web", branch: "child"}) || b.unpublishedBinding.Branch() != "child" || b.unpublishedBinding.HostPort != 8081 {
		t.Errorf("unpublish=(%+v, %+v)", b.unpublishedTarget, b.unpublishedBinding)
	}
}

func TestParsePublishedPortsAssignsInitialBranch(t *testing.T) {
	bindings, err := parsePublishedPorts([]string{"127.0.0.1:8080:80"})
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings[0].BranchID != "main" {
		t.Fatalf("bindings = %+v, want one main-branch binding", bindings)
	}
}

func TestWriteTree(t *testing.T) {
	created := time.Date(2026, 7, 17, 8, 9, 10, 123, time.UTC)
	families := []Family{{
		ID:    "web",
		State: "running",
		Branches: []Branch{
			{ID: "retry", ParentID: "test-a", State: "running", ForkTotalNS: 2_000_000, SourceDowntimeNS: 1_000_000},
			{ID: "test-b", ParentID: "main", State: "paused"},
			{ID: "main", State: "running", CreatedAt: created, InitPID: 1, InitState: "running", MemorySharing: "cow"},
			{ID: "test-a", ParentID: "main", State: "paused"},
		},
	}}
	var out bytes.Buffer
	writeTree(&out, families)
	want := "web (running)\n" +
		"└── main running pid1=running(1) memory=cow created=2026-07-17T08:09:10.000000123Z\n" +
		"    ├── test-a paused\n" +
		"    │   └── retry running fork=2ms downtime=1ms\n" +
		"    └── test-b paused\n"
	if got := out.String(); got != want {
		t.Errorf("writeTree output:\n%s\nwant:\n%s", got, want)
	}
}

func TestListJSONIncludesBranchParentage(t *testing.T) {
	b := &fakeBackend{families: []Family{{
		ID: "web", State: "running",
		Branches: []Branch{
			{ID: "main", State: "running"},
			{ID: "child", ParentID: "main", State: "paused"},
		},
	}}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"ls", "--json"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("ls --json status=%d, stderr=%q", status, stderr.String())
	}
	var families []struct {
		Branches []map[string]any `json:"branches"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &families); err != nil {
		t.Fatalf("decoding ls JSON: %v", err)
	}
	if got := families[0].Branches[0]["parent"]; got != nil {
		t.Errorf("root parent=%v, want omitted", got)
	}
	child := families[0].Branches[1]
	if child["parent"] != "main" {
		t.Errorf("child parentage=%v, want parent main", child)
	}
	if _, ok := child["parent_id"]; ok {
		t.Errorf("child parentage=%v, parent_id must not be emitted", child)
	}
}

func TestStoppedStateIsExposedAsExited(t *testing.T) {
	container := ttcbundle.ContainerInfo{
		ID: "legacy", Name: "legacy", State: "stopped",
		Branches: []ttcbundle.BranchInfo{{ID: "main", State: "stopped"}},
	}
	inspected := buildInspectInfo(container, nil, nil, "")
	if got := inspected.State.Status; got != "exited" {
		t.Fatalf("inspect state=%q, want exited", got)
	}
	if got := inspected.TTC.Container.State; got != "exited" {
		t.Fatalf("persistent container state=%q, want exited", got)
	}
	if got := inspected.TTC.Container.Branches[0].State; got != "exited" {
		t.Fatalf("persistent branch state=%q, want exited", got)
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
	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"ps", "-aq"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("ps -aq status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := stdout.String(), "web@main\nweb@child\nweb@old\nprepared@main\n"; got != want {
		t.Errorf("ps -aq=%q, want %q", got, want)
	}
}

func TestPsSizeUsesBranchAccounting(t *testing.T) {
	b := &fakeBackend{
		families: []Family{
			{Name: "web", ImageSize: 1000000, Branches: []Branch{
				{ID: "main", State: "running"},
				{ID: "child", State: "paused"},
				{ID: "old", State: "exited"},
			}},
			{Name: "prepared", ImageSize: 2000000, Branches: []Branch{{ID: "main", State: "created"}}},
		},
		rootfsSizes: map[selector]uint64{
			{container: "web", branch: "main"}:  4096,
			{container: "web", branch: "child"}: 8192,
		},
	}
	var stdout, stderr bytes.Buffer
	args := []string{"ps", "--all", "--size", "--format", "{{.Names}}|{{.Size}}|{{.SizeRW}}|{{.SizeRootFS}}"}
	if status := runCommand(args, &stdout, &stderr, b); status != 0 {
		t.Fatalf("ps --size status=%d, stderr=%q", status, stderr.String())
	}
	want := "web@main|4.1kB (virtual 1MB)|4096|1004096\n" +
		"web@child|8.19kB (virtual 1.01MB)|8192|1008192\n" +
		"web@old|- (virtual 1MB)|0|1000000\n" +
		"prepared@main|0B (virtual 2MB)|0|2000000\n"
	if got := stdout.String(); got != want {
		t.Errorf("ps --size=%q, want %q", got, want)
	}
	wantCalls := []selector{{container: "web", branch: "main"}, {container: "web", branch: "child"}}
	if !reflect.DeepEqual(b.sizeCalls, wantCalls) {
		t.Errorf("size calls=%+v, want %+v", b.sizeCalls, wantCalls)
	}

	b.sizeCalls = nil
	stdout.Reset()
	if status := runCommand([]string{"ps", "--size", "--quiet"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("ps --size --quiet status=%d, stderr=%q", status, stderr.String())
	}
	if len(b.sizeCalls) != 0 {
		t.Errorf("quiet ps performed invisible size calls: %+v", b.sizeCalls)
	}
}

func TestPsDockerFiltersLatestAndFormatting(t *testing.T) {
	base := time.Date(2026, time.July, 17, 0, 0, 0, 0, time.UTC)
	digest := "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	b := &fakeBackend{families: []Family{
		{
			ID: "web", Image: "docker.io/library/alpine:3.20", Digest: digest,
			CreatedAt: base,
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

	stdout.Reset()
	if status := runCommand([]string{"top", "web@child", "-eo", "pid,ppid,nlwp,pcpu,comm"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("top -eo status=%d, stderr=%q", status, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 || !reflect.DeepEqual(strings.Fields(lines[0]), []string{"PID", "PPID", "NLWP", "%CPU", "COMMAND"}) || !reflect.DeepEqual(strings.Fields(lines[1]), []string{"7", "1", "2", "3", "worker"}) {
		t.Errorf("top -eo output=%q", stdout.String())
	}

	stdout.Reset()
	if status := runCommand([]string{"top", "web@child", "-o", "user=OWNER", "-opid,args"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("top repeated -o status=%d, stderr=%q", status, stderr.String())
	}
	lines = strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 || !reflect.DeepEqual(strings.Fields(lines[0]), []string{"OWNER", "PID", "COMMAND"}) || !reflect.DeepEqual(strings.Fields(lines[1]), []string{"1000", "7", "worker"}) {
		t.Errorf("top repeated -o output=%q", stdout.String())
	}
}

func TestTopRejectsUnsupportedProcessOptions(t *testing.T) {
	for _, args := range [][]string{
		{"top", "web", "aux"},
		{"top", "web", "-o", "pid,rss"},
		{"top", "web", "-o"},
		{"top", "--json", "web", "-ef"},
	} {
		b := &fakeBackend{}
		var stdout, stderr bytes.Buffer
		if status := runCommand(args, &stdout, &stderr, b); status != 1 {
			t.Errorf("%q status=%d, want 1", args, status)
		}
		if b.processTarget != (selector{}) {
			t.Errorf("%q queried backend before rejecting options", args)
		}
	}
}

func TestCommandsUseQualifiedBranchesAndBareForkDestination(t *testing.T) {
	b := &fakeBackend{families: []Family{{ID: "web", State: "running", Branches: []Branch{{ID: "main", State: "running"}}}}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"fork", "--paused", "web@main", "test-a"}, &stdout, &stderr, b); status != 0 {
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
	b.resumed = selector{}
	if status := runCommand([]string{"unpause", "web@test-a"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("unpause status=%d, stderr=%q", status, stderr.String())
	}
	if b.resumed != (selector{container: "web", branch: "test-a"}) {
		t.Errorf("unpause target=%+v", b.resumed)
	}

	stdout.Reset()
	if status := runCommand([]string{"rm", "web@test-a"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("branch-targeted rm status=%d, stderr=%q", status, stderr.String())
	}
	if b.destroyed != (selector{container: "web", branch: "test-a"}) {
		t.Errorf("destroy target=%+v", b.destroyed)
	}
	if b.destroyForce {
		t.Error("branch-targeted rm without -f forced removal")
	}
}

func TestForkRejectsRemovedCheckFlag(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"fork", "--check", "web"}, &stdout, &stderr, b); status == 0 {
		t.Fatal("fork accepted the removed --check flag")
	}
	if b.forkDestination != "" {
		t.Fatalf("removed --check flag attempted a fork to %q", b.forkDestination)
	}
	if !strings.Contains(stderr.String(), "flag provided but not defined: -check") {
		t.Fatalf("stderr=%q, want unknown --check flag", stderr.String())
	}
}

func TestForkGeneratesDestination(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"fork", "web@main"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("fork status=%d, stderr=%q", status, stderr.String())
	}
	if !strings.HasPrefix(b.forkDestination, "fork-") {
		t.Errorf("generated destination %q does not start with fork-", b.forkDestination)
	}
	if got := stdout.String(); got != "web@"+b.forkDestination+"\n" {
		t.Errorf("fork output=%q, want %q", got, "web@"+b.forkDestination+"\n")
	}
}

func TestForkCreatesSiblingBatch(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"fork", "--paused", "web@main", "a", "b", "c"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("fork status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := b.forkDestinations, []string{"a", "b", "c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("fork destinations=%q, want %q", got, want)
	}
	if !b.forkPaused {
		t.Fatal("batch fork did not preserve --paused")
	}
	if got, want := stdout.String(), "web@a\nweb@b\nweb@c\n"; got != want {
		t.Errorf("fork output=%q, want %q", got, want)
	}
}

type fakeDistributedBackend struct {
	*fakeBackend
	source           selector
	targetNode       string
	targetContainer  string
	targetContainers []string
	targetPaused     bool
}

type fakeElasticNodeBackend struct {
	*fakeBackend
	stoppedNode  string
	stoppedMode  string
	stoppedLocal bool
}

func (*fakeElasticNodeBackend) clusterNodes() ([]cluster.NodeRecord, error) { return nil, nil }
func (b *fakeElasticNodeBackend) stopNode(node, mode string, local bool) ([]selector, error) {
	b.stoppedNode, b.stoppedMode, b.stoppedLocal = node, mode, local
	return nil, nil
}
func (*fakeElasticNodeBackend) rebalanceNode(string, string, bool) (cluster.FixedTerminalPlan, error) {
	return cluster.FixedTerminalPlan{}, nil
}
func (*fakeElasticNodeBackend) plannerSnapshot(string, bool) (cluster.PlannerSnapshot, error) {
	return cluster.PlannerSnapshot{}, nil
}

func TestNodeStopAcceptsPolicyAfterNode(t *testing.T) {
	b := &fakeElasticNodeBackend{fakeBackend: &fakeBackend{}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"node", "stop", "node-b", "--migrate-all"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("node stop status=%d stderr=%q", status, stderr.String())
	}
	if b.stoppedNode != "node-b" || b.stoppedMode != "migrate" || b.stoppedLocal {
		t.Fatalf("node stop call=(%q, %q, %t)", b.stoppedNode, b.stoppedMode, b.stoppedLocal)
	}
}

func (b *fakeDistributedBackend) forkRemote(source selector, targetNode, targetContainer string, paused bool) (selector, error) {
	b.source = source
	b.targetNode = targetNode
	b.targetContainer = targetContainer
	b.targetPaused = paused
	return parseSelector(targetContainer)
}

func (b *fakeDistributedBackend) forkRemoteMany(source selector, targetNode string, targetContainers []string, paused bool) ([]selector, error) {
	b.source = source
	b.targetNode = targetNode
	b.targetContainers = append([]string(nil), targetContainers...)
	b.targetPaused = paused
	results := make([]selector, len(targetContainers))
	for i, target := range targetContainers {
		parsed, err := parseSelector(target)
		if err != nil {
			return nil, err
		}
		results[i] = parsed
	}
	if len(targetContainers) == 1 {
		b.targetContainer = targetContainers[0]
	}
	return results, nil
}

func TestForkTargetsRemoteNode(t *testing.T) {
	b := &fakeDistributedBackend{fakeBackend: &fakeBackend{}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"fork", "--paused", "--node", "node-b", "web@child", "remote"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("fork status=%d, stderr=%q", status, stderr.String())
	}
	if b.source != (selector{container: "web", branch: "child"}) || b.targetNode != "node-b" || b.targetContainer != "web@remote" || !b.targetPaused {
		t.Fatalf("distributed fork call source=%+v node=%q container=%q paused=%t", b.source, b.targetNode, b.targetContainer, b.targetPaused)
	}
	if got, want := stdout.String(), "web@remote\n"; got != want {
		t.Fatalf("fork output=%q, want %q", got, want)
	}
}

func TestForkTargetsRemoteSiblingBatch(t *testing.T) {
	b := &fakeDistributedBackend{fakeBackend: &fakeBackend{}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"fork", "--node", "node-b", "web@main", "remote-a", "remote-b"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("fork status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := b.targetContainers, []string{"web@remote-a", "web@remote-b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("distributed targets=%q, want %q", got, want)
	}
	if got, want := stdout.String(), "web@remote-a\nweb@remote-b\n"; got != want {
		t.Fatalf("fork output=%q, want %q", got, want)
	}
}

func TestForkGeneratesQualifiedRemoteDestination(t *testing.T) {
	b := &fakeDistributedBackend{fakeBackend: &fakeBackend{}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"fork", "--node", "node-b", "web@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("fork status=%d, stderr=%q", status, stderr.String())
	}
	if !strings.HasPrefix(b.targetContainer, "web@fork-") {
		t.Fatalf("generated remote container=%q", b.targetContainer)
	}
	if got, want := stdout.String(), b.targetContainer+"\n"; got != want {
		t.Fatalf("fork output=%q, want %q", got, want)
	}
}

func TestForkRejectsQualifiedRemoteDestination(t *testing.T) {
	b := &fakeDistributedBackend{fakeBackend: &fakeBackend{}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"fork", "--node", "node-b", "web@main", "remote@second"}, &stdout, &stderr, b); status != 0 {
		if !strings.Contains(stderr.String(), "must be a branch name") {
			t.Fatalf("fork status=%d, stderr=%q", status, stderr.String())
		}
		return
	}
	t.Fatal("qualified remote destination was accepted")
}

func TestForkRejectsQualifiedLocalDestination(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	status := runCommand([]string{"fork", "web@main", "other@child"}, &stdout, &stderr, b)
	if status != 1 {
		t.Fatalf("status=%d, want 1", status)
	}
	if !strings.Contains(stderr.String(), "must be a branch name, not CONTAINER@BRANCH") {
		t.Errorf("stderr=%q", stderr.String())
	}
}

func TestRemoveAcceptsFamiliesAndQualifiedBranches(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	status := runCommand([]string{"rm", "web"}, &stdout, &stderr, b)
	if status != 0 || b.removed != "web" {
		t.Fatalf("family rm status=%d removed=%q stderr=%q", status, b.removed, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"rm", "web@main"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("branch-targeted rm main status=%d, stderr=%q", status, stderr.String())
	}
	if b.destroyed != (selector{container: "web", branch: "main"}) {
		t.Errorf("destroy target=%+v", b.destroyed)
	}
	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"rm", "-f", "web@main"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("forced branch-targeted rm main status=%d, stderr=%q", status, stderr.String())
	}
	if !b.destroyForce {
		t.Error("rm -f did not force branch removal")
	}
}

func TestPullNormalizesDockerReference(t *testing.T) {
	b := &fakeBackend{pullInfo: ttcimages.Info{
		Reference: "docker.io/library/ubuntu:latest",
		Digest:    "sha256:1234",
		Platform:  "linux/amd64",
		PullStats: &ttcimages.PullStats{DownloadedBytes: 1024, ReusedBytes: 2048},
	}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"pull", "ubuntu"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("pull status=%d, stderr=%q", status, stderr.String())
	}
	if b.pullReference != "docker.io/library/ubuntu:latest" {
		t.Errorf("pull reference=%q", b.pullReference)
	}
	want := "Pulled docker.io/library/ubuntu:latest\nDigest: sha256:1234\nPlatform: linux/amd64\nContent: 1024 bytes downloaded, 2048 bytes reused\n"
	if got := stdout.String(); got != want {
		t.Errorf("pull output=%q, want %q", got, want)
	}
}

func TestImagePullAlias(t *testing.T) {
	b := &fakeBackend{pullInfo: ttcimages.Info{Digest: "sha256:alias"}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"image", "pull", "-q", "alpine:3.20"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("image pull status=%d, stderr=%q", status, stderr.String())
	}
	if b.pullReference != "docker.io/library/alpine:3.20" || stdout.String() != "sha256:alias\n" {
		t.Fatalf("image pull reference=%q output=%q", b.pullReference, stdout.String())
	}
}

func TestPullOptionsAndQuietOutput(t *testing.T) {
	b := &fakeBackend{pullInfo: ttcimages.Info{Digest: "sha256:5678"}}
	var stdout, stderr bytes.Buffer
	args := []string{"pull", "--platform", "linux/arm64", "-q", "example/app:v1"}
	if status := runCommand(args, &stdout, &stderr, b); status != 0 {
		t.Fatalf("pull status=%d, stderr=%q", status, stderr.String())
	}
	if b.pullReference != "docker.io/example/app:v1" || b.pullPlatform != "linux/arm64" {
		t.Errorf("pull call: reference=%q platform=%q", b.pullReference, b.pullPlatform)
	}
	if got := stdout.String(); got != "sha256:5678\n" {
		t.Errorf("quiet pull output=%q", got)
	}
}

func TestImageLoadUsesDockerCLIAndRestoresEveryTag(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "images.tar")
	if err := os.WriteFile(archive, []byte("archive bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"load", "-i", archive, "--platform", "linux/amd64"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("load status=%d, stderr=%q", status, stderr.String())
	}
	if b.loadedBytes != len("archive bytes") || b.loadedPlatform != "linux/amd64" {
		t.Fatalf("load bytes=%d platform=%q", b.loadedBytes, b.loadedPlatform)
	}
	want := "Loaded image: one:latest\nLoaded image: two:test\n"
	if got := stdout.String(); got != want {
		t.Fatalf("load output=%q, want %q", got, want)
	}
	stdout.Reset()
	if status := runCommand([]string{"image", "load", "-q", "-i", archive}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("image load status=%d, stderr=%q", status, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("quiet image load output=%q", stdout.String())
	}
	if status := runCommand([]string{"image", "import", archive}, &stdout, &stderr, b); status == 0 {
		t.Fatal("legacy image import unexpectedly succeeded")
	}
}

func TestImageSaveUsesDockerCLIAndStreamsArchive(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"save", "--platform", "linux/amd64", "one:latest", "two:test"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("save status=%d, stderr=%q", status, stderr.String())
	}
	if got := stdout.String(); got != "image archive" {
		t.Fatalf("save archive=%q", got)
	}
	if want := []string{"one:latest", "two:test"}; !reflect.DeepEqual(b.savedIdentifiers, want) || b.savedPlatform != "linux/amd64" {
		t.Fatalf("save identifiers=%q platform=%q", b.savedIdentifiers, b.savedPlatform)
	}
	output := filepath.Join(t.TempDir(), "images.tar")
	if status := runCommand([]string{"image", "save", "-o", output, "one:latest"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("image save status=%d, stderr=%q", status, stderr.String())
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "image archive" {
		t.Fatalf("saved file=%q", data)
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
	for _, want := range []string{"REPOSITORY", "TAG", "IMAGE ID", "PLATFORM", "ROOTFS", "busybox", "latest", "stable", "0123456789ab", "linux/amd64", "2.15MB", "2 hours ago"} {
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

func TestImageInspectAggregatesAliasesAndTTCState(t *testing.T) {
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
			Labels:     map[string]string{"base.label": "kept"},
		},
		{
			Reference:    "docker.io/library/busybox:stable",
			Digest:       digest,
			Platform:     "linux/arm64/v8",
			Materialized: true,
			LocalCache:   &ImageCacheInfo{CompleteFiles: 3, TotalFiles: 5, CompleteBytes: 1024, TotalBytes: 2048},
		},
	}
	got, err := inspectImageFromInventory(infos, "busybox")
	if err != nil {
		t.Fatalf("inspectImageFromInventory: %v", err)
	}
	if got.ID != digest || got.OS != "linux" || got.Architecture != "arm64" || got.Variant != "v8" || !got.Materialized {
		t.Errorf("inspect info=%+v", got)
	}
	if got.LocalCache == nil || got.LocalCache.CompleteBytes != 1024 || got.LocalCache.TotalBytes != 2048 {
		t.Errorf("LocalCache=%+v", got.LocalCache)
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
	if got.Config.Labels["base.label"] != "kept" {
		t.Errorf("Config.Labels=%v", got.Config.Labels)
	}
}

func TestCommitCommandUsesDockerOptions(t *testing.T) {
	digest := "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	b := &fakeBackend{commitInfo: ttcimages.Info{Digest: digest}}
	var stdout, stderr bytes.Buffer
	args := []string{
		"commit", "--author", "TTC Harness <ttc@example.com>", "--message", "prepared session",
		"--change", "LABEL timetravel.usage=agent-session", "--change", `CMD ["agent","run"]`,
		"--pause=false", "demo@child", "example/session:v1",
	}
	if status := runCommand(args, &stdout, &stderr, b); status != 0 {
		t.Fatalf("commit status=%d, stderr=%q", status, stderr.String())
	}
	wantChanges := []string{"LABEL timetravel.usage=agent-session", `CMD ["agent","run"]`}
	if b.commitTarget != (selector{container: "demo", branch: "child"}) || b.commitReference != "example/session:v1" ||
		b.commitOptions.Author != "TTC Harness <ttc@example.com>" || b.commitOptions.Message != "prepared session" ||
		b.commitOptions.Pause || !reflect.DeepEqual(b.commitOptions.Changes, wantChanges) {
		t.Fatalf("commit target=%+v reference=%q options=%+v", b.commitTarget, b.commitReference, b.commitOptions)
	}
	if stdout.String() != digest+"\n" {
		t.Fatalf("commit output=%q", stdout.String())
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
	args := []string{"run", "--name", "demo", "-dit", "--rm", "--read-only", "--stop-signal", "SIGUSR2", "--stop-timeout", "7", "--pids-limit", "23", "-m", "128m", "--cpus", "1.5", "--cpu-shares", "512", "--shm-size", "32m", "--group-add", "wheel", "--group-add", "4242", "--cap-add", "sys_admin", "--cap-drop", "cap_net_raw", "--ulimit", "nofile=64:128", "--ulimit", "core=0", "--pull", "never", "--network", "host", "--waive-live-fork", "-e", "A=1", "--label", "suite=fast", "--workdir", "/work", "--user", "123:456", "-h", "demo-host", "--entrypoint", "/entry", "--log-driver", "json-file", "--log-opt", "max-size=1.5m", "--log-opt", "max-file=4", "--log-opt", "env=A", "--log-opt", "labels=suite", "-v", "/host/a:/host-data:ro", "--mount", "type=bind,src=/host/b,dst=/config,readonly,immutable-source", "--tmpfs", "/scratch:mode=0700,size=1m", "--mount", "type=tmpfs,target=/worktmp,tmpfs-size=2097152,tmpfs-mode=0750", "--mount", "type=proc,target=/alt-proc,readonly", "--mount", "type=sysfs,target=/alt-sys", "busybox:1.36", "echo", "ok"}
	if status := runCommand(args, &stdout, &stderr, b); status != 0 {
		t.Fatalf("run status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := stdout.String(), containerID+"\n"; got != want {
		t.Errorf("run output=%q, want %q", got, want)
	}
	if b.runRequest.name != "demo" || b.runRequest.reference != "busybox:1.36" || !b.runRequest.detach || !b.runRequest.interactive || !b.runRequest.terminal || !b.runRequest.autoRemove || !b.runRequest.readonlyRootfs || b.runRequest.cwd != "/work" || b.runRequest.user != "123:456" || b.runRequest.hostname != "demo-host" || b.runRequest.networkMode != "host" || b.runRequest.liveForkEligible || b.runRequest.liveForkWaiver == "" || b.runRequest.pullPolicy != "never" || b.runRequest.stopSignal != "SIGUSR2" || b.runRequest.stopTimeout != 7 {
		t.Errorf("run request=%+v", b.runRequest)
	}
	if b.runRequest.pidsLimit == nil || *b.runRequest.pidsLimit != 23 {
		t.Errorf("pids limit=%v, want 23", b.runRequest.pidsLimit)
	}
	if b.runRequest.memoryLimit == nil || *b.runRequest.memoryLimit != 128*1024*1024 {
		t.Errorf("memory limit=%v, want 128 MiB", b.runRequest.memoryLimit)
	}
	if b.runRequest.nanoCPUs == nil || *b.runRequest.nanoCPUs != 1_500_000_000 {
		t.Errorf("NanoCPUs=%v, want 1500000000", b.runRequest.nanoCPUs)
	}
	if b.runRequest.cpuShares == nil || *b.runRequest.cpuShares != 512 {
		t.Errorf("CPU shares=%v, want 512", b.runRequest.cpuShares)
	}
	if got, want := b.runRequest.shmSize, int64(32*1024*1024); got != want {
		t.Errorf("shared memory size=%d, want %d", got, want)
	}
	if got, want := b.runRequest.groupAdd, []string{"wheel", "4242"}; !reflect.DeepEqual(got, want) {
		t.Errorf("group add=%q, want %q", got, want)
	}
	if got, want := b.runRequest.capAdd, []string{"SYS_ADMIN"}; !reflect.DeepEqual(got, want) {
		t.Errorf("cap add=%q, want %q", got, want)
	}
	if got, want := b.runRequest.capDrop, []string{"NET_RAW"}; !reflect.DeepEqual(got, want) {
		t.Errorf("cap drop=%q, want %q", got, want)
	}
	wantUlimits := []ttcbundle.Ulimit{{Name: "core", Soft: 0, Hard: 0}, {Name: "nofile", Soft: 64, Hard: 128}}
	if !reflect.DeepEqual(b.runRequest.ulimits, wantUlimits) {
		t.Errorf("ulimits=%+v, want %+v", b.runRequest.ulimits, wantUlimits)
	}
	if got, want := strings.Join(b.runRequest.args, " "), "echo ok"; got != want {
		t.Errorf("run args=%q, want %q", got, want)
	}
	if got, want := strings.Join(b.runRequest.entrypoint, " "), "/entry"; got != want {
		t.Errorf("entrypoint=%q, want %q", got, want)
	}
	wantMounts := []ttcbundle.HostMount{
		{Source: "/host/a", Destination: "/host-data", Readonly: true},
		{Source: "/host/b", Destination: "/config", Readonly: true, ImmutableSource: true},
	}
	if !reflect.DeepEqual(b.runRequest.hostMounts, wantMounts) {
		t.Errorf("host mounts=%+v, want %+v", b.runRequest.hostMounts, wantMounts)
	}
	wantFilesystems := []ttcbundle.FilesystemMount{
		{Type: "tmpfs", Destination: "/scratch", Options: []string{"rw", "mode=0700", "size=1m"}, HostConfig: true, InspectOptions: "mode=0700,size=1m"},
		{Type: "tmpfs", Destination: "/worktmp", Options: []string{"rw", "size=2097152", "mode=0750"}},
		{Type: "proc", Destination: "/alt-proc", Readonly: true, Options: []string{"ro", "nosuid", "nodev", "noexec"}},
		{Type: "sysfs", Destination: "/alt-sys", Readonly: true, Options: []string{"ro", "nosuid", "nodev", "noexec"}},
	}
	if !reflect.DeepEqual(b.runRequest.filesystemMounts, wantFilesystems) {
		t.Errorf("filesystem mounts=%+v, want %+v", b.runRequest.filesystemMounts, wantFilesystems)
	}
	if b.runRequest.logDriver != "json-file" {
		t.Errorf("log driver=%q, want json-file", b.runRequest.logDriver)
	}
	if got, want := b.runRequest.logOptions, map[string]string{"env": "A", "labels": "suite"}; !reflect.DeepEqual(got, want) {
		t.Errorf("log options=%q, want %q", got, want)
	}
	if b.runRequest.consoleLogMaxBytes != 1572864 || b.runRequest.consoleLogMaxFiles != 4 {
		t.Errorf("log retention=(%d, %d), want (1572864, 4)", b.runRequest.consoleLogMaxBytes, b.runRequest.consoleLogMaxFiles)
	}
}

func TestRunAndCreateParseTimeTravelNetworking(t *testing.T) {
	wantPorts := []ttcbundle.PortBinding{
		{HostIP: "0.0.0.0", HostPort: 8080, ContainerPort: 80, Protocol: "tcp", BranchID: "main"},
		{HostIP: "127.0.0.1", HostPort: 8443, ContainerPort: 443, Protocol: "tcp", BranchID: "main"},
	}
	for _, command := range []string{"run", "create"} {
		t.Run(command, func(t *testing.T) {
			backend := &fakeBackend{containerID: "demo"}
			var stdout, stderr bytes.Buffer
			args := []string{command, "--network=ttc", "--nat", "--ip", "192.0.2.44", "--mac-address", "02:aa:bb:cc:dd:ee", "-p", "8080:80", "--publish", "127.0.0.1:8443:443/tcp", "busybox"}
			if status := runCommand(args, &stdout, &stderr, backend); status != 0 {
				t.Fatalf("status=%d, stderr=%q", status, stderr.String())
			}
			if backend.runRequest.networkMode != "ttc" || !backend.runRequest.networkNAT {
				t.Errorf("network request=%+v", backend.runRequest)
			}
			if backend.runRequest.networkIP != "192.0.2.44" || backend.runRequest.networkMAC != "02:aa:bb:cc:dd:ee" {
				t.Errorf("virtual NIC=(%q, %q), want custom IP and MAC", backend.runRequest.networkIP, backend.runRequest.networkMAC)
			}
			if !reflect.DeepEqual(backend.runRequest.publishedPorts, wantPorts) {
				t.Errorf("published ports=%+v, want %+v", backend.runRequest.publishedPorts, wantPorts)
			}
		})
	}
}

func TestImageProgressIsHumanReadable(t *testing.T) {
	var output bytes.Buffer
	progress := newImageProgress(&output)
	progress(ttcimages.ProgressEvent{Phase: ttcimages.ProgressResolving})
	progress(ttcimages.ProgressEvent{Phase: ttcimages.ProgressDownloading, Total: 1000})
	progress(ttcimages.ProgressEvent{Phase: ttcimages.ProgressDownloading, Current: 500, Total: 1000})
	progress(ttcimages.ProgressEvent{Phase: ttcimages.ProgressDownloadDone, Current: 1000, Total: 1000})
	progress(ttcimages.ProgressEvent{Phase: ttcimages.ProgressExtracting, Total: 1000, Index: 1, Count: 2})
	progress(ttcimages.ProgressEvent{Phase: ttcimages.ProgressExtracted, Current: 1000, Total: 1000, Index: 1, Count: 2})
	for _, want := range []string{"Resolving image metadata", "Pulling fs layer", "Downloading 500B / 1kB (50%)", "Download complete", "Extracting layer 1/2", "Pull complete"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("progress output %q does not contain %q", output.String(), want)
		}
	}
}

func TestImageProgressRepaintsOneTerminalLinePerLayer(t *testing.T) {
	var output bytes.Buffer
	progress := newImageProgressWithTerminal(&output, true, 80)
	first := ttcimages.ProgressEvent{Phase: ttcimages.ProgressDownloading, Total: 10 << 20}
	first.Descriptor.Digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	second := ttcimages.ProgressEvent{Phase: ttcimages.ProgressDownloading, Total: 20 << 20}
	second.Descriptor.Digest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	progress(ttcimages.ProgressEvent{Phase: ttcimages.ProgressResolving})
	progress(first)
	progress(second)
	first.Current = 5 << 20
	progress(first)
	first.Phase = ttcimages.ProgressDownloadDone
	first.Current = first.Total
	progress(first)
	second.Phase = ttcimages.ProgressAlreadyExists
	second.Current = second.Total
	progress(second)
	first.Phase = ttcimages.ProgressExtracting
	first.Current = 4 << 20
	first.Index = 1
	first.Count = 2
	progress(first)
	first.Phase = ttcimages.ProgressExtracted
	first.Current = first.Total
	progress(first)

	got := output.String()
	for _, want := range []string{
		"Resolving image metadata\n",
		"\x1b[1A",
		"\x1b[2A",
		"\r\x1b[2Kaaaaaaaaaaaa: Downloading",
		"bbbbbbbbbbbb: Already exists",
		"Extracting 1/2",
		"aaaaaaaaaaaa: Pull complete",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("interactive progress output %q does not contain %q", got, want)
		}
	}
}

func TestTimeTravelNetworkingRejectsInvalidCombinations(t *testing.T) {
	tests := [][]string{
		{"run", "--nat", "busybox"},
		{"run", "--ip", "192.0.2.44", "busybox"},
		{"create", "--network=ttc", "--ip", "192.0.2.2", "busybox"},
		{"create", "--network=ttc", "--mac-address", "01:00:5e:00:00:01", "busybox"},
		{"run", "-p", "8080:80", "busybox"},
		{"create", "--network=ttc", "-p", "8080:80/udp", "busybox"},
		{"create", "--network=ttc", "-p", "not-a-port", "busybox"},
		{"create", "--network=ttc", "-p", "127.0.0.1:8080:80", "-p", "127.0.0.1:8080:81", "busybox"},
	}
	for _, args := range tests {
		backend := &fakeBackend{}
		var stdout, stderr bytes.Buffer
		if status := runCommand(args, &stdout, &stderr, backend); status != 1 {
			t.Errorf("runCommand(%q) status=%d, want 1", args, status)
		}
		if backend.runRequest.reference != "" {
			t.Errorf("runCommand(%q) reached backend", args)
		}
	}
}

func TestParseTmpfsMountDockerOptions(t *testing.T) {
	const value = "/data:ro,suid,dev,noexec,sync,dirsync,noatime,nodiratime,size=64m,mode=1770,uid=123,gid=456,nr_inodes=400k,nr_blocks=1024"
	got, err := parseTmpfsMount(value)
	if err != nil {
		t.Fatalf("parseTmpfsMount: %v", err)
	}
	want := ttcbundle.FilesystemMount{
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
	const absent = "TTC_TEST_ABSENT_ENV_7281"
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
				"--memory", "64m",
				"--cpu-period", "200000",
				"--cpu-quota", "-1",
				"--ulimit", "stack=8388608",
				"--group-add", "4242",
				"--cap-add", "chown",
				"--cap-drop", "all",
				"--add-host", "db:192.0.2.10",
				"--add-host", "v6=[2001:0db8::10]",
				"--dns", "203.0.113.53",
				"--dns", "2001:0db8::53",
				"--dns-search", "example.test",
				"--dns-option", "ndots:2",
				"--dns-opt", "rotate",
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
			if b.runRequest.memoryLimit == nil || *b.runRequest.memoryLimit != 64*1024*1024 {
				t.Errorf("memory limit=%v, want 64 MiB", b.runRequest.memoryLimit)
			}
			if b.runRequest.cpuPeriod == nil || *b.runRequest.cpuPeriod != 200000 || b.runRequest.cpuQuota == nil || *b.runRequest.cpuQuota != -1 {
				t.Errorf("CPU period/quota=(%v, %v), want (200000, -1)", b.runRequest.cpuPeriod, b.runRequest.cpuQuota)
			}
			if got, want := b.runRequest.groupAdd, []string{"4242"}; !reflect.DeepEqual(got, want) {
				t.Errorf("group add=%q, want %q", got, want)
			}
			if got, want := b.runRequest.capAdd, []string{"CHOWN"}; !reflect.DeepEqual(got, want) {
				t.Errorf("cap add=%q, want %q", got, want)
			}
			if got, want := b.runRequest.capDrop, []string{"ALL"}; !reflect.DeepEqual(got, want) {
				t.Errorf("cap drop=%q, want %q", got, want)
			}
			if got, want := b.runRequest.extraHosts, []string{"db:192.0.2.10", "v6:2001:db8::10"}; !reflect.DeepEqual(got, want) {
				t.Errorf("extra hosts=%q, want %q", got, want)
			}
			if got, want := b.runRequest.dns, []string{"203.0.113.53", "2001:db8::53"}; !reflect.DeepEqual(got, want) {
				t.Errorf("DNS=%q, want %q", got, want)
			}
			if got, want := b.runRequest.dnsSearch, []string{"example.test"}; !reflect.DeepEqual(got, want) {
				t.Errorf("DNS search=%q, want %q", got, want)
			}
			if got, want := b.runRequest.dnsOptions, []string{"ndots:2", "rotate"}; !reflect.DeepEqual(got, want) {
				t.Errorf("DNS options=%q, want %q", got, want)
			}
			if got, want := b.runRequest.shmSize, ttcbundle.DefaultShmSizeBytes; got != want {
				t.Errorf("default shared memory size=%d, want %d", got, want)
			}
			if got, want := b.runRequest.ulimits, []ttcbundle.Ulimit{{Name: "stack", Soft: 8388608, Hard: 8388608}}; !reflect.DeepEqual(got, want) {
				t.Errorf("ulimits=%+v, want %+v", got, want)
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

func TestRunAndCreateRejectInvalidCapability(t *testing.T) {
	for _, command := range []string{"run", "create"} {
		for _, flagName := range []string{"--cap-add", "--cap-drop"} {
			b := &fakeBackend{}
			var stdout, stderr bytes.Buffer
			if status := runCommand([]string{command, flagName, "not-a-capability", "busybox"}, &stdout, &stderr, b); status != 1 {
				t.Errorf("%s invalid %s status=%d, want 1", command, flagName, status)
			}
			if b.runRequest.reference != "" {
				t.Errorf("%s invalid %s invoked backend", command, flagName)
			}
		}
	}
}

func TestRunAndCreateRejectInvalidShmSize(t *testing.T) {
	for _, command := range []string{"run", "create"} {
		for _, value := range []string{"invalid", "-1"} {
			b := &fakeBackend{}
			var stdout, stderr bytes.Buffer
			if status := runCommand([]string{command, "--shm-size", value, "busybox"}, &stdout, &stderr, b); status != 1 {
				t.Errorf("%s invalid shm size %q status=%d, want 1", command, value, status)
			}
			if b.runRequest.reference != "" {
				t.Errorf("%s invalid shm size invoked backend", command)
			}
		}
	}
}

func TestRunAndCreateRejectInvalidUlimit(t *testing.T) {
	for _, command := range []string{"run", "create"} {
		b := &fakeBackend{}
		var stdout, stderr bytes.Buffer
		if status := runCommand([]string{command, "--ulimit", "nofile=2:1", "busybox"}, &stdout, &stderr, b); status != 1 {
			t.Errorf("%s invalid ulimit status=%d, want 1", command, status)
		}
		if b.runRequest.reference != "" {
			t.Errorf("%s invalid ulimit invoked backend", command)
		}
	}
}

func TestRunAndCreateRejectInvalidStopPolicy(t *testing.T) {
	for _, args := range [][]string{
		{"run", "--stop-signal", "SIGINVALID", "busybox"},
		{"create", "--stop-timeout", "-2", "busybox"},
		{"run", "--pids-limit", "-2", "busybox"},
		{"run", "--memory", "-1", "busybox"},
		{"create", "--memory", "1m", "busybox"},
		{"create", "--cpus", "0.001", "busybox"},
		{"run", "--cpu-shares", "1", "busybox"},
		{"run", "--cpus", "1", "--cpu-quota", "50000", "busybox"},
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

func TestRunAndCreateRejectInvalidNetworkConfiguration(t *testing.T) {
	invalid := [][]string{
		{"--add-host", "missing-address"},
		{"--add-host", "host:host-gateway"},
		{"--dns", "not-an-ip"},
		{"--dns-search", "bad..domain"},
		{"--dns-option", "ndots:2 rotate"},
	}
	for _, command := range []string{"run", "create"} {
		for _, option := range invalid {
			b := &fakeBackend{}
			var stdout, stderr bytes.Buffer
			args := append([]string{command}, option...)
			args = append(args, "busybox")
			if status := runCommand(args, &stdout, &stderr, b); status != 1 {
				t.Errorf("runCommand(%q) status=%d, want 1", args, status)
			}
			if b.runRequest.reference != "" {
				t.Errorf("runCommand(%q) invoked backend with %+v", args, b.runRequest)
			}
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

func TestRunAndCreateRequireExplicitHostNetworkForkWaiver(t *testing.T) {
	for _, command := range []string{"run", "create"} {
		b := &fakeBackend{}
		var stdout, stderr bytes.Buffer
		if status := runCommand([]string{command, "--network=host", "busybox"}, &stdout, &stderr, b); status != 1 {
			t.Errorf("%s host network without waiver status=%d, want 1", command, status)
		}
		if !strings.Contains(stderr.String(), "--waive-live-fork") {
			t.Errorf("%s host network error=%q, want waiver guidance", command, stderr.String())
		}
		stderr.Reset()
		if status := runCommand([]string{command, "--network=host", "--waive-live-fork", "busybox"}, &stdout, &stderr, b); status != 0 {
			t.Fatalf("%s host network with waiver status=%d: %s", command, status, stderr.String())
		}
		if b.runRequest.liveForkEligible || b.runRequest.liveForkWaiver == "" {
			t.Errorf("%s request eligibility=(%t, %q), want explicit waiver", command, b.runRequest.liveForkEligible, b.runRequest.liveForkWaiver)
		}
	}
}

func TestRunRejectsInvalidLogOptions(t *testing.T) {
	for _, option := range []string{"max-size=0", "max-size=nope", "max-file=0", "labels=one,,two", "env-regex=[", "unknown=value", "missing"} {
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

func TestRunAndCreateValidateLogDrivers(t *testing.T) {
	for _, command := range []string{"run", "create"} {
		for _, args := range [][]string{
			{command, "--log-driver", "journald", "busybox"},
			{command, "--log-driver", "none", "--log-opt", "max-file=2", "busybox"},
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
		b := &fakeBackend{containerID: strings.Repeat("b", 64)}
		var stdout, stderr bytes.Buffer
		if status := runCommand([]string{command, "--log-driver", "none", "busybox"}, &stdout, &stderr, b); status != 0 {
			t.Fatalf("%s --log-driver none status=%d, stderr=%q", command, status, stderr.String())
		}
		if b.runRequest.logDriver != "none" {
			t.Errorf("%s log driver=%q, want none", command, b.runRequest.logDriver)
		}
	}
}

func TestParseImmutableHostMountAssertion(t *testing.T) {
	mounts, err := parseHostMounts([]string{"/host/a:/data:ro,immutable-source"}, []string{"type=bind,source=/host/b,target=/config,readonly,immutable-source=true"})
	if err != nil {
		t.Fatalf("parseHostMounts: %v", err)
	}
	if len(mounts) != 2 || !mounts[0].ImmutableSource || !mounts[1].ImmutableSource {
		t.Errorf("immutable host mounts=%+v", mounts)
	}
}

func TestWritableHostMountsDefaultToManagedCoW(t *testing.T) {
	for _, input := range []struct {
		volumes []string
		mounts  []string
	}{
		{volumes: []string{"/host:/data"}},
		{mounts: []string{"type=bind,source=/host,target=/data"}},
	} {
		mounts, err := parseHostMounts(input.volumes, input.mounts)
		if err != nil {
			t.Fatal(err)
		}
		if len(mounts) != 1 || !mounts[0].ManagedCoW || mounts[0].Readonly || mounts[0].Copy {
			t.Errorf("writable host mounts=%+v", mounts)
		}
	}
}

func TestRunAndCreateRejectImagePlatformOverride(t *testing.T) {
	for _, args := range [][]string{
		{"run", "--platform", "linux/arm64", "busybox"},
		{"create", "--platform=linux/arm64", "busybox"},
	} {
		b := &fakeBackend{}
		var stdout, stderr bytes.Buffer
		if status := runCommand(args, &stdout, &stderr, b); status != 1 {
			t.Errorf("runCommand(%q) status=%d, want 1", args, status)
		}
		if b.runRequest.reference != "" {
			t.Errorf("runCommand(%q) invoked backend with %+v", args, b.runRequest)
		}
		if !strings.Contains(stderr.String(), "flag provided but not defined: -platform") {
			t.Errorf("runCommand(%q) stderr=%q, want unsupported platform flag", args, stderr.String())
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
	if status := runCommand([]string{"create", "--name", "demo", "-it", "--rm", "--stop-signal", "SIGQUIT", "--stop-timeout", "4", "-h", "create-host", "--net", "host", "--waive-live-fork", "busybox", "sleep", "30"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("create status=%d, stderr=%q", status, stderr.String())
	}
	if !b.created || b.runRequest.name != "demo" || b.runRequest.pullPolicy != "missing" || b.runRequest.hostname != "create-host" || b.runRequest.networkMode != "host" || b.runRequest.liveForkEligible || b.runRequest.liveForkWaiver == "" || !b.runRequest.interactive || !b.runRequest.terminal || !b.runRequest.autoRemove || b.runRequest.stopSignal != "SIGQUIT" || b.runRequest.stopTimeout != 4 || strings.Join(b.runRequest.args, " ") != "sleep 30" {
		t.Errorf("create request=%+v created=%t", b.runRequest, b.created)
	}
	if got, want := stdout.String(), containerID+"\n"; got != want {
		t.Errorf("create output=%q, want %q", got, want)
	}
	stdout.Reset()
	if status := runCommand([]string{"start", "demo@main"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("start status=%d, stderr=%q", status, stderr.String())
	}
	if b.started != (selector{container: "demo", branch: "main"}) || !b.startDetach || b.startInteractive {
		t.Errorf("start target=%v detach=%t interactive=%t", b.started, b.startDetach, b.startInteractive)
	}
	if stdout.String() != "demo@main\n" {
		t.Errorf("start output=%q", stdout.String())
	}
}

func TestStartMultipleAndContainerAliases(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"container", "start", "one@main", "two@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("container start status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := stdout.String(), "one@main\ntwo@child\n"; got != want {
		t.Errorf("multi-start output=%q, want %q", got, want)
	}
	if got, want := b.startCalls, []startCall{{target: selector{container: "one", branch: "main"}, detach: true}, {target: selector{container: "two", branch: "child"}, detach: true}}; !reflect.DeepEqual(got, want) {
		t.Errorf("start calls=%+v, want %+v", got, want)
	}

	b = &fakeBackend{}
	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"start", "--attach", "one@main", "two@main"}, &stdout, &stderr, b); status == 0 {
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
			args = append(args, "demo@main")
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
		{"attach", "--no-stdout", "--no-stderr", "demo@main"},
		{"attach", "-i", "--no-stdin", "demo@main"},
		{"attach", "-i", "--takeover", "demo@main"},
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
	if status := runCommand([]string{"attach", "-i", "demo@main"}, &stdout, &stderr, b); status != 0 {
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
	since := "2026-07-17T01:02:03.123456789Z"
	until := "2026-07-17T02:03:04Z"
	if status := runCommand([]string{"logs", "--follow", "--exec", "exec-one", "--tail", "7", "--since", since, "--until", until, "--timestamps", "--details", "demo@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("logs status=%d, stderr=%q", status, stderr.String())
	}
	options := b.logsOptions
	if b.logsTarget != (selector{container: "demo", branch: "child"}) || options.ExecID != "exec-one" || !options.Follow || options.Tail != 7 || !options.Timestamps || !options.Details {
		t.Errorf("logs target=%+v options=%+v", b.logsTarget, options)
	}
	if options.Since == nil || options.Since.Format(time.RFC3339Nano) != since || options.Until == nil || options.Until.Format(time.RFC3339Nano) != until {
		t.Errorf("logs time options=%+v", options)
	}
	if status := runCommand([]string{"logs", "--tail", "invalid", "demo@main"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("logs with Docker-compatible invalid tail status=%d, stderr=%q", status, stderr.String())
	}
	if b.logsOptions.Tail != -1 {
		t.Errorf("invalid --tail selected %d, want all (-1)", b.logsOptions.Tail)
	}
	if status := runCommand([]string{"logs", "--since", "not-a-time", "demo@main"}, &stdout, &stderr, b); status == 0 {
		t.Fatal("logs accepted an invalid --since value")
	}
}

func TestParseLogTime(t *testing.T) {
	now := time.Date(2026, 7, 17, 3, 0, 0, 0, time.UTC)
	tests := map[string]time.Time{
		"2026-07-17T01:02:03.123456789Z": time.Date(2026, 7, 17, 1, 2, 3, 123456789, time.UTC),
		"60.25":                          time.Unix(60, 250000000).UTC(),
		"-1.5":                           time.Unix(-1, -500000000).UTC(),
		"90m":                            now.Add(-90 * time.Minute),
	}
	for input, want := range tests {
		got, err := parseLogTime(input, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseLogTime(%q)=%v, %v; want %v", input, got, err, want)
		}
	}
}

func TestConsoleLogOutputFiltersAndPrefixesLines(t *testing.T) {
	since := time.Date(2026, 7, 17, 1, 0, 0, 0, time.UTC)
	until := since.Add(2 * time.Second)
	output := newConsoleLogOutput(logsOptions{Since: &since, Until: &until, Timestamps: true, Details: true}, map[string]string{"zeta": "two", "alpha": "one"})
	var stdout, stderr bytes.Buffer
	frames := []ttcconsole.Frame{
		{Stream: ttcconsole.StreamStdout, Timestamp: since.Add(-time.Second), Data: []byte("old\n")},
		{Stream: ttcconsole.StreamStdout, Timestamp: since, Data: []byte("one\ntwo")},
		{Stream: ttcconsole.StreamStdout, Timestamp: since.Add(time.Second), Data: []byte("-continued\nthree\n")},
		{Stream: ttcconsole.StreamStderr, Timestamp: until, Data: []byte("problem\n")},
		{Stream: ttcconsole.StreamStderr, Timestamp: until.Add(time.Nanosecond), Data: []byte("new\n")},
	}
	for _, frame := range frames {
		if err := output.write(frame, &stdout, &stderr); err != nil {
			t.Fatalf("write(%+v): %v", frame, err)
		}
	}
	wantStdout := "2026-07-17T01:00:00Z alpha=one,zeta=two one\n2026-07-17T01:00:00Z alpha=one,zeta=two two-continued\n2026-07-17T01:00:01Z alpha=one,zeta=two three\n"
	if got := stdout.String(); got != wantStdout {
		t.Errorf("stdout=%q, want %q", got, wantStdout)
	}
	if got, want := stderr.String(), "2026-07-17T01:00:02Z alpha=one,zeta=two problem\n"; got != want {
		t.Errorf("stderr=%q, want %q", got, want)
	}
}

func TestConsoleLogTailPreservesGlobalLineOrderAndStreams(t *testing.T) {
	tail := &consoleLogTail{limit: 3}
	stdout := tail.writer(ttcconsole.StreamStdout)
	stderr := tail.writer(ttcconsole.StreamStderr)
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
	w, err := ttcconsole.NewRotatingWriter(path, 34)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	for _, value := range []string{"drop\n", "keep-one\n", "keep-two\n"} {
		if err := ttcconsole.WriteFrame(w, ttcconsole.StreamStdout, []byte(value)); err != nil {
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
	if _, err := follower.drain(tail.writer(ttcconsole.StreamStdout), tail.writer(ttcconsole.StreamStderr)); err != nil {
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
		{stream: ttcconsole.StreamStdout, data: "hello"},
		{stream: ttcconsole.StreamStderr, data: "problem"},
		{stream: ttcconsole.StreamStdout},
		{stream: ttcconsole.StreamStderr},
	} {
		if err := ttcconsole.WriteFrame(&encoded, frame.stream, []byte(frame.data)); err != nil {
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
	w, err := ttcconsole.NewRotatingWriter(path, 28)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	for _, value := range []string{"first", "second"} {
		if err := ttcconsole.WriteFrame(w, ttcconsole.StreamStdout, []byte(value)); err != nil {
			t.Fatalf("WriteFrame(%q): %v", value, err)
		}
	}
	follower, err := openConsoleLogFollower(path)
	if err != nil {
		t.Fatalf("openConsoleLogFollower: %v", err)
	}
	defer follower.file.Close()
	if err := ttcconsole.WriteFrame(w, ttcconsole.StreamStdout, []byte("third")); err != nil {
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
	w, err := ttcconsole.NewRotatingWriterWithFiles(path, 10, 2)
	if err != nil {
		t.Fatalf("NewRotatingWriterWithFiles: %v", err)
	}
	if err := ttcconsole.WriteFrame(w, ttcconsole.StreamStdout, []byte("first")); err != nil {
		t.Fatalf("WriteFrame(first): %v", err)
	}
	follower, err := openConsoleLogFollower(path, 2)
	if err != nil {
		t.Fatalf("openConsoleLogFollower: %v", err)
	}
	defer follower.file.Close()
	for _, value := range []string{"second", "third"} {
		if err := ttcconsole.WriteFrame(w, ttcconsole.StreamStdout, []byte(value)); err != nil {
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
	w, err := ttcconsole.NewRotatingWriter(path, 28)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	for _, value := range []string{"first", "second", "third"} {
		if err := ttcconsole.WriteFrame(w, ttcconsole.StreamStderr, []byte(value)); err != nil {
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
	w, err := ttcconsole.NewRotatingWriterWithFiles(path, 10, 3)
	if err != nil {
		t.Fatalf("NewRotatingWriterWithFiles: %v", err)
	}
	for _, value := range []string{"drop", "two", "three", "four"} {
		if err := ttcconsole.WriteFrame(w, ttcconsole.StreamStdout, []byte(value)); err != nil {
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
	if !strings.HasPrefix(request.execID, "exec-") {
		t.Errorf("attached exec has no stable session ID: %q", request.execID)
	}
}

func TestExecSessionBeforeCombinedShortOptions(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"exec", "--session", "build1", "-dit", "demo@main", "sh"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("exec status=%d, stderr=%q", status, stderr.String())
	}
	if b.execRequest.execID != "build1" || !b.execRequest.detach || !b.execRequest.interactive || !b.execRequest.terminal {
		t.Fatalf("exec request=%+v", b.execRequest)
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

func TestExecNamedDurableSession(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"exec", "--session", "build1", "-d", "demo@child", "make"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("named detached exec status=%d, stderr=%q", status, stderr.String())
	}
	if got := b.execRequest.execID; got != "build1" {
		t.Fatalf("exec session=%q, want build1", got)
	}
	if got, want := stdout.String(), "demo@child:build1\n"; got != want {
		t.Fatalf("detached exec output=%q, want %q", got, want)
	}

	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"exec", "--session", "../escape", "-d", "demo@child", "true"}, &stdout, &stderr, b); status == 0 {
		t.Fatal("exec accepted an unsafe session name")
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

func TestDiffHumanAndJSON(t *testing.T) {
	b := &fakeBackend{diffChanges: []BranchPathChange{
		{Path: "/work/new", Kind: "add", Type: "regular", Mount: "/"},
		{Path: "/work/old", Kind: "delete", Type: "regular", Mount: "/"},
	}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"diff", "demo@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("diff status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := stdout.String(), "A /work/new\nD /work/old\n"; got != want {
		t.Fatalf("diff output=%q, want %q", got, want)
	}
	if b.processTarget != (selector{container: "demo", branch: "child"}) {
		t.Fatalf("diff target=%+v", b.processTarget)
	}
	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"diff", "--json", "demo@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("diff --json status=%d, stderr=%q", status, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"path": "/work/new"`) || !strings.Contains(stdout.String(), `"kind": "add"`) {
		t.Fatalf("diff JSON=%q", stdout.String())
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
	if status := runCommand([]string{"exec", "demo@main", "false"}, &stdout, &stderr, b); status != 23 {
		t.Fatalf("exec status=%d, want 23; stderr=%q", status, stderr.String())
	}
}

func TestInspect(t *testing.T) {
	b := &fakeBackend{inspectInfo: InspectInfo{
		ID: "demo", Name: "/demo", Image: "sha256:abc",
		State:  InspectState{Status: "running", Running: true},
		Config: InspectConfig{Image: "docker.io/library/busybox:latest"},
		TTC: TTCInspectInfo{Branches: []InspectBranch{{
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
	if status := runCommand([]string{"inspect", "--format", `{{.Name}}|{{.State.Status}}|{{len .TTC.Branches}}`, "demo"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("formatted inspect status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := stdout.String(), "/demo|running|1\n"; got != want {
		t.Errorf("formatted inspect=%q, want %q", got, want)
	}

	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"inspect", "--fork-check", "demo"}, &stdout, &stderr, b); status == 0 {
		t.Fatal("inspect accepted the removed --fork-check flag")
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

func TestPublishedPortStateUsesActiveRoot(t *testing.T) {
	shared := t.TempDir()
	local := t.TempDir()
	backend := &runtimeBackend{dataRoot: shared, localRoot: local}
	binding := ttcbundle.PortBinding{HostIP: "127.0.0.1", HostPort: 18080, ContainerPort: 80, Protocol: "tcp"}
	lockPath, logPath := portSidecarPaths(backend.activeDataRoot(), "container", binding)
	for _, path := range []string{lockPath, logPath} {
		if !strings.HasPrefix(path, local+string(os.PathSeparator)) {
			t.Fatalf("sidecar path %q is not below active root %q", path, local)
		}
		if strings.HasPrefix(path, shared+string(os.PathSeparator)) {
			t.Fatalf("sidecar path %q leaked into shared root %q", path, shared)
		}
	}
}

func TestBuildInspectInfo(t *testing.T) {
	containerID := strings.Repeat("c", 64)
	created := time.Date(2026, 7, 17, 1, 2, 3, 4, time.UTC)
	started := created.Add(time.Second)
	exit := 23
	stopTimeout := 4
	pidsLimit := int64(23)
	memoryLimit := int64(256 * 1024 * 1024)
	nanoCPUs := int64(1_500_000_000)
	cpuShares := uint64(512)
	cpuPeriod := uint64(100000)
	cpuQuota := int64(150000)
	shmSize := int64(32 * 1024 * 1024)
	groupAdd := []string{"wheel", "4242"}
	capAdd := []string{"SYS_ADMIN"}
	capDrop := []string{"NET_RAW"}
	ulimits := []ttcbundle.Ulimit{{Name: "core", Soft: 0, Hard: 0}, {Name: "nofile", Soft: 64, Hard: 128}}
	extraHosts := []string{"db:192.0.2.10", "v6:2001:db8::10"}
	dns := []string{"203.0.113.53"}
	dnsSearch := []string{"example.test"}
	dnsOptions := []string{"ndots:2", "rotate"}
	container := ttcbundle.ContainerInfo{
		ID: containerID, Name: "demo", Reference: "docker.io/library/alpine:3.20", Digest: "sha256:abc", Platform: "linux/amd64",
		RootFS: "/roots/alpine", NetworkMode: "host", LogDriver: "json-file", ConsoleLogMaxBytes: 65536, ConsoleLogMaxFiles: 3, AutoRemove: true, StopSignal: "SIGUSR1", StopTimeout: &stopTimeout, PidsLimit: &pidsLimit, MemoryLimit: &memoryLimit, NanoCPUs: &nanoCPUs, CPUShares: &cpuShares, CPUPeriod: &cpuPeriod, CPUQuota: &cpuQuota, ShmSize: &shmSize, GroupAdd: groupAdd, CapAdd: capAdd, CapDrop: capDrop, Ulimits: ulimits, ExtraHosts: extraHosts, DNS: dns, DNSSearch: dnsSearch, DNSOptions: dnsOptions, State: "running", ExitStatus: &exit, CreatedAt: created, StartedAt: &started,
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
			{Type: "bind", Source: "/state/network/hosts", Destination: "/etc/hosts", Options: []string{"rbind", "ro"}},
			{Type: "tmpfs", Source: "tmpfs", Destination: "/scratch", Options: []string{"rw", "mode=0700", "size=1m"}},
			{Type: "tmpfs", Source: "tmpfs", Destination: "/worktmp", Options: []string{"rw", "size=2097152", "mode=0750"}},
			{Type: "proc", Source: "proc", Destination: "/alt-proc", Options: []string{"ro", "nosuid", "nodev", "noexec"}},
			{Type: "sysfs", Source: "sysfs", Destination: "/alt-sys", Options: []string{"ro", "nosuid", "nodev", "noexec"}},
		},
		Hostname: "custom-host",
		Annotations: map[string]string{
			"dev.timetravelcontainer.config.entrypoint": `["/bin/sh"]`,
			"dev.timetravelcontainer.config.cmd":        `["-c","sleep 10"]`,
			"dev.timetravelcontainer.config.user":       "app",
			"dev.timetravelcontainer.config.labels":     `{"suite":"ttc"}`,
			"dev.timetravelcontainer.config.tmpfs":      `{"/scratch":"mode=0700,size=1m"}`,
			ttcbundle.InternalMountsAnnotation:          `["/etc/hosts"]`,
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
	if !reflect.DeepEqual(info.Config.Entrypoint, []string{"/bin/sh"}) || !reflect.DeepEqual(info.Config.Cmd, []string{"-c", "sleep 10"}) || info.Config.Labels["suite"] != "ttc" {
		t.Errorf("Docker config reconstruction=%+v", info.Config)
	}
	if len(info.Mounts) != 4 || info.Mounts[0].Type != "bind" || info.Mounts[0].RW || info.Mounts[1].Type != "tmpfs" || !info.Mounts[1].RW || info.Mounts[2].Type != "proc" || info.Mounts[2].RW || info.Mounts[3].Type != "sysfs" || info.Mounts[3].RW {
		t.Errorf("mounts=%+v", info.Mounts)
	}
	if got := info.TTC.ExternalViews; len(got) != 1 || got[0].Source != "/host/data" || got[0].Destination != "/data" || !got[0].Readonly || got[0].Semantics != "shared-external-view" {
		t.Errorf("external views=%+v", got)
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
	if info.HostConfig.Memory != memoryLimit || info.HostConfig.NanoCPUs != nanoCPUs || info.HostConfig.CPUShares != cpuShares || info.HostConfig.CPUPeriod != cpuPeriod || info.HostConfig.CPUQuota != cpuQuota {
		t.Errorf("inspect resources=%+v", info.HostConfig)
	}
	if !reflect.DeepEqual(info.HostConfig.GroupAdd, groupAdd) || !reflect.DeepEqual(info.HostConfig.CapAdd, capAdd) || !reflect.DeepEqual(info.HostConfig.CapDrop, capDrop) {
		t.Errorf("inspect groups/capabilities=(%q, %q, %q)", info.HostConfig.GroupAdd, info.HostConfig.CapAdd, info.HostConfig.CapDrop)
	}
	if info.HostConfig.ShmSize != shmSize {
		t.Errorf("inspect shared memory size=%d, want %d", info.HostConfig.ShmSize, shmSize)
	}
	if !reflect.DeepEqual(info.HostConfig.Ulimits, ulimits) {
		t.Errorf("inspect ulimits=%+v, want %+v", info.HostConfig.Ulimits, ulimits)
	}
	if !info.HostConfig.AutoRemove {
		t.Error("inspect omitted auto-remove policy")
	}
	if info.HostConfig.LogConfig.Type != "json-file" || info.HostConfig.LogConfig.Config["max-size"] != "65536" || info.HostConfig.LogConfig.Config["max-file"] != "3" {
		t.Errorf("log config=%+v", info.HostConfig.LogConfig)
	}
	if info.HostConfig.NetworkMode != "host" {
		t.Errorf("network mode=%q, want host", info.HostConfig.NetworkMode)
	}
	if !reflect.DeepEqual(info.HostConfig.ExtraHosts, extraHosts) || !reflect.DeepEqual(info.HostConfig.DNS, dns) || !reflect.DeepEqual(info.HostConfig.DNSSearch, dnsSearch) || !reflect.DeepEqual(info.HostConfig.DNSOptions, dnsOptions) {
		t.Errorf("inspect network configuration=(%q, %q, %q, %q)", info.HostConfig.ExtraHosts, info.HostConfig.DNS, info.HostConfig.DNSSearch, info.HostConfig.DNSOptions)
	}
	var networkTemplate bytes.Buffer
	if err := writeInspectFormat(&networkTemplate, []InspectInfo{info}, `{{index .HostConfig.Dns 0}}|{{index .HostConfig.DnsSearch 0}}|{{index .HostConfig.DnsOptions 0}}|{{.HostConfig.ShmSize}}`); err != nil {
		t.Fatalf("Docker DNS inspect template: %v", err)
	}
	if got, want := networkTemplate.String(), "203.0.113.53|example.test|ndots:2|33554432\n"; got != want {
		t.Errorf("Docker DNS inspect template=%q, want %q", got, want)
	}
	var logTemplate bytes.Buffer
	if err := writeInspectFormat(&logTemplate, []InspectInfo{info}, `{{.HostConfig.LogConfig.Type}}|{{index .HostConfig.LogConfig.Config "max-size"}}|{{index .HostConfig.LogConfig.Config "max-file"}}`); err != nil {
		t.Fatalf("Docker log-config inspect template: %v", err)
	}
	if got, want := logTemplate.String(), "json-file|65536|3\n"; got != want {
		t.Errorf("Docker log-config inspect template=%q, want %q", got, want)
	}

	if len(info.TTC.Branches) != 2 || info.TTC.Branches[0].Metadata.ID != "child" {
		t.Errorf("TTC branches=%+v", info.TTC.Branches)
	}
}

func TestInternalAutoRemoveNowForcesRemoval(t *testing.T) {
	b := &fakeBackend{
		waitStatus: uint32(7 << 8),
		inspectInfo: InspectInfo{TTC: TTCInspectInfo{Branches: []InspectBranch{{
			Metadata: Branch{ID: "main", State: "exited"},
		}}}},
	}
	if err := runAutoRemoveNow([]string{"demo@main"}, b); err != nil {
		t.Fatalf("runAutoRemoveNow: %v", err)
	}
	if b.removed != "demo" || !b.removeForce || !b.removeVolumes {
		t.Errorf("remove=(%q, force=%t, volumes=%t)", b.removed, b.removeForce, b.removeVolumes)
	}
	if err := runAutoRemoveNow([]string{"demo"}, b); err == nil {
		t.Fatal("internal auto-remove accepted an unqualified family")
	}
}

func TestInternalAutoRemovePreservesForkedPeers(t *testing.T) {
	b := &fakeBackend{
		inspectInfo: InspectInfo{TTC: TTCInspectInfo{Branches: []InspectBranch{
			{Metadata: Branch{ID: "main", State: "exited"}},
			{Metadata: Branch{ID: "child", State: "running"}},
		}}},
	}
	if err := runAutoRemoveNow([]string{"demo@main"}, b); err != nil {
		t.Fatalf("runAutoRemoveNow: %v", err)
	}
	if b.destroyed != (selector{container: "demo", branch: "main"}) || !b.destroyForce {
		t.Errorf("destroyed=%+v force=%t", b.destroyed, b.destroyForce)
	}
	if b.removed != "" {
		t.Errorf("removed family %q despite live peer", b.removed)
	}
}

func TestRemove(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"rm", "--force", "--volumes", "demo"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("rm status=%d, stderr=%q", status, stderr.String())
	}
	if b.removed != "demo" || !b.removeForce || !b.removeVolumes || stdout.String() != "demo\n" {
		t.Errorf("removed=%q force=%t volumes=%t output=%q", b.removed, b.removeForce, b.removeVolumes, stdout.String())
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
	b = &fakeBackend{}
	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"kill", "web@child", "SIGUSR1"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("positional kill status=%d, stderr=%q", status, stderr.String())
	}
	if len(b.signals) != 1 || b.signals[0].target != (selector{container: "web", branch: "child"}) || b.signals[0].signal != 10 {
		t.Errorf("positional signal calls=%+v", b.signals)
	}
}

func TestStopGraceful(t *testing.T) {
	timeout := 3
	b := &fakeBackend{inspectInfo: InspectInfo{
		Config:     InspectConfig{StopSignal: "SIGUSR1"},
		HostConfig: InspectHostConfig{StopTimeout: &timeout},
		TTC:        TTCInspectInfo{Branches: []InspectBranch{{Metadata: Branch{ID: "child", State: "running"}}}},
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
	want := selector{container: "web", branch: "child"}
	if b.preparedStop != want || b.discarded != want {
		t.Errorf("prepare=%+v discard=%+v, want %+v", b.preparedStop, b.discarded, want)
	}
}

func TestStopEscalatesToKill(t *testing.T) {
	timeout := 0
	b := &fakeBackend{
		waitRelease: make(chan struct{}),
		inspectInfo: InspectInfo{
			Config:     InspectConfig{StopSignal: "SIGUSR2"},
			HostConfig: InspectHostConfig{StopTimeout: &timeout},
			TTC:        TTCInspectInfo{Branches: []InspectBranch{{Metadata: Branch{ID: "main", State: "running"}}}},
		},
	}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"stop", "web@main"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("stop status=%d, stderr=%q", status, stderr.String())
	}
	if len(b.signals) != 2 || b.signals[0].signal != 12 || b.signals[1].signal != 9 {
		t.Errorf("signal calls=%+v, want configured SIGUSR2 then SIGKILL", b.signals)
	}
}

func TestStopAcceptsGracefulExitRacingWithKill(t *testing.T) {
	timeout := 0
	b := &fakeBackend{
		waitRelease: make(chan struct{}),
		killErr:     errors.New("control socket closed"),
		inspectInfo: InspectInfo{
			Config:     InspectConfig{StopSignal: "SIGTERM"},
			HostConfig: InspectHostConfig{StopTimeout: &timeout},
			TTC:        TTCInspectInfo{Branches: []InspectBranch{{Metadata: Branch{ID: "main", State: "running"}}}},
		},
	}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"stop", "web@main"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("stop race status=%d, stderr=%q", status, stderr.String())
	}
}

func TestStopTimeoutOverride(t *testing.T) {
	configuredTimeout := 30
	b := &fakeBackend{
		waitRelease: make(chan struct{}),
		inspectInfo: InspectInfo{
			Config:     InspectConfig{StopSignal: "SIGTERM"},
			HostConfig: InspectHostConfig{StopTimeout: &configuredTimeout},
			TTC:        TTCInspectInfo{Branches: []InspectBranch{{Metadata: Branch{ID: "main", State: "running"}}}},
		},
	}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"stop", "--time", "0", "web@main"}, &stdout, &stderr, b); status != 0 {
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
	if status := runCommand([]string{"container", "wait", "web@main", "web@child"}, &stdout, &stderr, b); status != 0 {
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

func TestStopPausedAndDehydratedDiscardsWithoutResume(t *testing.T) {
	for _, state := range []string{"paused", "dehydrated"} {
		t.Run(state, func(t *testing.T) {
			b := &fakeBackend{inspectInfo: InspectInfo{TTC: TTCInspectInfo{Branches: []InspectBranch{{Metadata: Branch{ID: "child", State: state}}}}}}
			var stdout, stderr bytes.Buffer
			if status := runCommand([]string{"stop", "web@child"}, &stdout, &stderr, b); status != 0 {
				t.Fatalf("stop status=%d stderr=%q", status, stderr.String())
			}
			if len(b.signals) != 0 || b.resumed.container != "" || b.preparedStop.container != "" {
				t.Fatalf("stop %s signaled/resumed/prepared: signals=%+v resumed=%+v prepared=%+v", state, b.signals, b.resumed, b.preparedStop)
			}
			if b.discarded != (selector{container: "web", branch: "child"}) {
				t.Fatalf("discarded=%+v", b.discarded)
			}
		})
	}
}

func TestStopFamilyAppliesBranchStateSemantics(t *testing.T) {
	b := &fakeBackend{inspectInfo: InspectInfo{TTC: TTCInspectInfo{Branches: []InspectBranch{
		{Metadata: Branch{ID: "main", State: "running"}},
		{Metadata: Branch{ID: "paused", State: "paused"}},
		{Metadata: Branch{ID: "cold", State: "dehydrated"}},
		{Metadata: Branch{ID: "done", State: "exited"}},
	}}}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"stop", "web"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("stop family status=%d stderr=%q", status, stderr.String())
	}
	if got, want := stdout.String(), "web\n"; got != want {
		t.Fatalf("stdout=%q, want %q", got, want)
	}
	if len(b.signals) != 1 || b.signals[0].target != (selector{container: "web", branch: "main"}) {
		t.Fatalf("family stop signals=%+v", b.signals)
	}
	wantDiscards := []selector{{container: "web", branch: "cold"}, {container: "web", branch: "done"}, {container: "web", branch: "main"}, {container: "web", branch: "paused"}}
	if !reflect.DeepEqual(b.discards, wantDiscards) {
		t.Fatalf("family stop discards=%+v, want %+v", b.discards, wantDiscards)
	}
	if b.resumed.container != "" {
		t.Fatalf("family stop resumed %+v", b.resumed)
	}
}

func TestDehydrateAcceptsBranchOrFamily(t *testing.T) {
	b := &fakeBackend{inspectInfo: InspectInfo{TTC: TTCInspectInfo{Branches: []InspectBranch{
		{Metadata: Branch{ID: "main", State: "running"}},
		{Metadata: Branch{ID: "paused", State: "paused"}},
		{Metadata: Branch{ID: "cold", State: "dehydrated"}},
	}}}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"dehydrate", "web"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("dehydrate family status=%d stderr=%q", status, stderr.String())
	}
	want := []selector{{container: "web", branch: "main"}, {container: "web", branch: "paused"}}
	if !reflect.DeepEqual(b.dehydratedTargets, want) {
		t.Fatalf("dehydrated=%+v, want %+v", b.dehydratedTargets, want)
	}
	b.dehydratedTargets = nil
	if status := runCommand([]string{"dehydrate", "web@paused"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("dehydrate branch status=%d stderr=%q", status, stderr.String())
	}
	if !reflect.DeepEqual(b.dehydratedTargets, []selector{{container: "web", branch: "paused"}}) {
		t.Fatalf("branch dehydration=%+v", b.dehydratedTargets)
	}
}

func TestRestartRunningExitedPausedAndDehydratedBranches(t *testing.T) {
	for _, test := range []struct {
		name        string
		state       string
		wantSignal  bool
		wantDiscard bool
	}{
		{name: "running", state: "running", wantSignal: true, wantDiscard: true},
		{name: "exited", state: "exited"},
		{name: "paused", state: "paused", wantDiscard: true},
		{name: "dehydrated", state: "dehydrated", wantDiscard: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			timeout := 1
			b := &fakeBackend{inspectInfo: InspectInfo{
				Config:     InspectConfig{StopSignal: "SIGQUIT"},
				HostConfig: InspectHostConfig{StopTimeout: &timeout},
				TTC:        TTCInspectInfo{Branches: []InspectBranch{{Metadata: Branch{ID: "child", State: test.state}}}},
			}}
			var stdout, stderr bytes.Buffer
			if status := runCommand([]string{"restart", "web@child"}, &stdout, &stderr, b); status != 0 {
				t.Fatalf("restart status=%d, stderr=%q", status, stderr.String())
			}
			if got, want := stdout.String(), "web@child\n"; got != want {
				t.Errorf("restart output=%q, want %q", got, want)
			}
			wantTarget := selector{container: "web", branch: "child"}
			if got, want := b.startCalls, []startCall{{target: wantTarget, detach: true}}; !reflect.DeepEqual(got, want) {
				t.Errorf("start calls=%+v, want %+v", got, want)
			}
			if got := len(b.signals) != 0; got != test.wantSignal {
				t.Errorf("signals=%+v, wantSignal=%t", b.signals, test.wantSignal)
			}
			if test.wantSignal && (len(b.signals) != 1 || b.signals[0].target != wantTarget || b.signals[0].signal != 3) {
				t.Errorf("restart signals=%+v, want configured child SIGQUIT", b.signals)
			}
			if test.wantDiscard != (b.discarded == wantTarget) {
				t.Errorf("discarded=%+v, wantDiscard=%t", b.discarded, test.wantDiscard)
			}
			if b.resumed.container != "" {
				t.Errorf("restart resumed branch: %+v", b.resumed)
			}
		})
	}

	peerInfo := InspectInfo{
		TTC: TTCInspectInfo{Branches: []InspectBranch{
			{Metadata: Branch{ID: "main", State: "exited"}},
			{Metadata: Branch{ID: "child", State: "running"}},
		}},
	}
	b := &fakeBackend{inspectInfo: peerInfo}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"restart", "web@main"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("restart with active peer status=%d stderr=%q", status, stderr.String())
	}
	if got, want := b.startCalls, []startCall{{target: selector{container: "web", branch: "main"}, detach: true}}; !reflect.DeepEqual(got, want) {
		t.Errorf("restart starts=%+v, want %+v", got, want)
	}

	b = &fakeBackend{}
	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"restart", "web"}, &stdout, &stderr, b); status == 0 {
		t.Fatal("restart accepted a family name")
	}
}

func TestFreshStartPeers(t *testing.T) {
	info := ttcbundle.ContainerInfo{Branches: []ttcbundle.BranchInfo{
		{ID: "target", State: "exited"},
		{ID: "z-running", State: "running"},
		{ID: "a-paused", State: "paused"},
		{ID: "cold", State: "dehydrated"},
		{ID: "old", State: "stopped"},
	}}
	got, err := freshStartPeers(info, selector{container: "demo", branch: "target"})
	if err != nil {
		t.Fatalf("freshStartPeers: %v", err)
	}
	want := []selector{
		{container: "demo", branch: "a-paused"},
		{container: "demo", branch: "z-running"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("freshStartPeers=%+v, want %+v", got, want)
	}

	info.Branches = append(info.Branches, ttcbundle.BranchInfo{ID: "busy", State: "dehydrating"})
	if _, err := freshStartPeers(info, selector{container: "demo", branch: "target"}); err == nil || !strings.Contains(err.Error(), "busy is dehydrating") {
		t.Fatalf("freshStartPeers transient error=%v", err)
	}
}

func TestRehydrateAcceptsPublishingCapture(t *testing.T) {
	target := selector{container: "web", branch: "main"}
	b := &fakeBackend{inspectInfo: InspectInfo{TTC: TTCInspectInfo{Branches: []InspectBranch{{
		Metadata: Branch{ID: target.branch, State: "dehydrating"},
	}}}}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"rehydrate", target.String()}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("rehydrate publishing capture status=%d stderr=%q", status, stderr.String())
	}
	if got := b.rehydratedTargets; !reflect.DeepEqual(got, []selector{target}) {
		t.Fatalf("rehydrated targets=%+v, want %+v", got, []selector{target})
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

func TestStopContainerConsoleDrainsRemovesInactiveFollower(t *testing.T) {
	dataRoot := t.TempDir()
	base := filepath.Join(dataRoot, "containers", "demo", "console", "exec", "main", "exec-test")
	if err := os.MkdirAll(filepath.Dir(base), 0700); err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string]string{
		base + ".lock":        "",
		base + ".pid":         "2147483647\n",
		base + ".wait-status": "0\n",
	} {
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	b := runtimeBackend{dataRoot: dataRoot}
	if err := b.stopContainerConsoleDrains("demo"); err != nil {
		t.Fatalf("stopContainerConsoleDrains: %v", err)
	}
	for _, path := range []string{base + ".pid", base + ".wait-status"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%q still exists or could not be inspected: %v", path, err)
		}
	}
}

func TestVerifyCgroupV2Resources(t *testing.T) {
	const pid = 4242
	root := t.TempDir()
	dir := filepath.Join(root, "user.slice", "ttc", "demo")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	memory := int64(256 * 1024 * 1024)
	quota := int64(150000)
	period := uint64(100000)
	shares := uint64(512)
	resources := &specs.LinuxResources{
		Memory: &specs.LinuxMemory{Limit: &memory},
		CPU:    &specs.LinuxCPU{Quota: &quota, Period: &period, Shares: &shares},
	}
	write("cgroup.procs", "7\n4242\n")
	write("memory.max", "268435456\n")
	write("cpu.max", "150000 100000\n")
	write("cpu.weight", "20\n")
	membership := []byte("0::/user.slice/ttc/demo\n")
	if err := verifyCgroupV2Resources(pid, membership, root, "ttc/demo", resources); err != nil {
		t.Fatalf("verifyCgroupV2Resources: %v", err)
	}

	write("memory.max", "max\n")
	if err := verifyCgroupV2Resources(pid, membership, root, "ttc/demo", resources); err == nil || !strings.Contains(err.Error(), "memory.max") {
		t.Fatalf("wrong memory.max error=%v", err)
	}
	write("memory.max", "268435456\n")
	if err := verifyCgroupV2Resources(pid, []byte("1:cpu:/legacy\n"), root, "ttc/demo", resources); err == nil || !strings.Contains(err.Error(), "unified cgroup v2") {
		t.Fatalf("missing cgroup v2 error=%v", err)
	}
	if err := verifyCgroupV2Resources(pid, membership, root, "ttc/other", resources); err == nil || !strings.Contains(err.Error(), "want suffix") {
		t.Fatalf("wrong cgroup path error=%v", err)
	}
	write("cgroup.procs", "7\n")
	if err := verifyCgroupV2Resources(pid, membership, root, "ttc/demo", resources); err == nil || !strings.Contains(err.Error(), "is not a member") {
		t.Fatalf("missing PID error=%v", err)
	}
}

func TestClientDoesNotAcceptStorageOrDirectOptions(t *testing.T) {
	for _, option := range []string{"--root=/runtime", "--runsc=/ttcd", "--data-root=/data", "--direct"} {
		if _, _, _, err := parseGlobalOptions([]string{option, "ps"}, io.Discard); err == nil {
			t.Fatalf("legacy client option %q was accepted", option)
		}
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

	// The dentry cache is sized from this process's descriptor limit, so what
	// it should be is asked for rather than written down.
	dcache := []string(nil)
	if size, known := dentryCacheSize(); known {
		dcache = []string{"--dcache=" + strconv.Itoa(size)}
	}
	wantHardened := append([]string{"--root", "/runtime", "--platform=systrap", "--panic-log=/runtime/sentry-panic.log", "--rootless=true", "--network=none", "--directfs=false", "--allow-suid=true", "--overlay2=all:memory"}, dcache...)
	if got := (&runtimeBackend{root: "/runtime"}).configuredRunscArgs(); !reflect.DeepEqual(got, wantHardened) {
		t.Errorf("hardened runsc args=%q, want %q", got, wantHardened)
	}
	wantHost := append([]string{"--root", "/runtime", "--platform=systrap", "--panic-log=/runtime/sentry-panic.log", "--rootless=true", "--network=host", "--directfs=false", "--allow-suid=true", "--overlay2=all:memory"}, dcache...)
	if got := (&runtimeBackend{root: "/runtime"}).configuredRunscArgsForNetwork("host"); !reflect.DeepEqual(got, wantHost) {
		t.Errorf("host-network runsc args=%q, want %q", got, wantHost)
	}
	wantTTCNAT := append(append([]string{"--root", "/runtime", "--platform=systrap", "--panic-log=/runtime/sentry-panic.log", "--rootless=true", "--network=ttc", "--directfs=false", "--allow-suid=true", "--overlay2=all:memory"}, dcache...), "--ttc-network-nat=true")
	if got := (&runtimeBackend{root: "/runtime"}).configuredRunscArgsForNetworkNAT("ttc", true); !reflect.DeepEqual(got, wantTTCNAT) {
		t.Errorf("TTC NAT runsc args=%q, want %q", got, wantTTCNAT)
	}
	wantCustomNIC := append(append([]string(nil), wantTTCNAT...), "--ttc-network-ip=192.0.2.44", "--ttc-network-mac=02:aa:bb:cc:dd:ee")
	if got := (&runtimeBackend{root: "/runtime"}).configuredRunscArgsForNetworkConfig("ttc", true, "192.0.2.44", "02:aa:bb:cc:dd:ee"); !reflect.DeepEqual(got, wantCustomNIC) {
		t.Errorf("TTC custom NIC runsc args=%q, want %q", got, wantCustomNIC)
	}
	wantUnsafe := append([]string{
		"--root", "/runtime", "--platform=systrap", "--panic-log=/runtime/sentry-panic.log", "--rootless=false", "--unsafe-no-host-isolation=true",
		"--network=none", "--directfs=false", "--allow-suid=true", "--overlay2=all:memory",
	}, dcache...)
	if got := (&runtimeBackend{root: "/runtime", unsafeNoHostIsolation: true}).configuredRunscArgs(); !reflect.DeepEqual(got, wantUnsafe) {
		t.Errorf("unsafe runsc args=%q, want %q", got, wantUnsafe)
	}
	wantOldKernel := append(append([]string(nil), wantHardened...), "--gso=false")
	if got := (&runtimeBackend{root: "/runtime", disableHostGSO: true}).configuredRunscArgs(); !reflect.DeepEqual(got, wantOldKernel) {
		t.Errorf("old-kernel runsc args=%q, want %q", got, wantOldKernel)
	}
	wantKVM := append([]string(nil), wantHardened...)
	wantKVM[2] = "--platform=kvm"
	if got := (&runtimeBackend{root: "/runtime", runtimePlatform: "kvm"}).configuredRunscArgs(); !reflect.DeepEqual(got, wantKVM) {
		t.Errorf("KVM runsc args=%q, want %q", got, wantKVM)
	}
}

// The cache a sandbox is allowed fits in the descriptors it will be given.
//
// This is the whole of the fix: a login node hands its processes 8191
// descriptors and no more, the gofer spends close to two of them on every
// dentry the sentry holds, and the per-mount default of a thousand each meant a
// seven-mount sandbox was permitted a cache it could not pay for. It reached
// EMFILE with its caches still filling, and a gofer at EMFILE never recovers —
// nothing evicts on a descriptor shortage.
func TestTheDentryCacheFitsInTheDescriptorsTheGoferWillHave(t *testing.T) {
	size, known := dentryCacheSize()
	if !known {
		t.Skip("this process cannot read its own descriptor limit")
	}
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	// What a full cache costs the gofer, against what the gofer has. The
	// margin is the point: the other half is for the files the workload is
	// holding open, which are not cache and cannot be evicted.
	cost := size * goferFDsPerDentry
	if uint64(cost) >= limit.Cur && size < maxDentryCache {
		t.Fatalf("a full cache of %d dentries costs %d descriptors of %d", size, cost, limit.Cur)
	}
	if size < minDentryCache {
		t.Fatalf("cache of %d dentries is too small to be worth having", size)
	}
	if size > maxDentryCache {
		t.Fatalf("cache of %d dentries is past the ceiling of %d", size, maxDentryCache)
	}
}

// The sandbox is told the size, so that one cache serves every mount.
//
// Without the flag the sentry gives each mount its own thousand, which is what
// made the budget grow with the number of things mounted rather than staying
// inside what the gofer can hold.
func TestTheSandboxIsGivenOneCacheForAllOfItsMounts(t *testing.T) {
	size, known := dentryCacheSize()
	if !known {
		t.Skip("this process cannot read its own descriptor limit")
	}
	args := (&runtimeBackend{root: "/runtime"}).configuredRunscArgs()
	want := "--dcache=" + strconv.Itoa(size)
	for _, arg := range args {
		if arg == want {
			return
		}
	}
	t.Fatalf("runsc args %q do not carry %q", args, want)
}

func TestDehydrationNetworkArgs(t *testing.T) {
	none, err := dehydrationNetworkArgs(ttcbundle.ContainerInfo{NetworkMode: "none"})
	if err != nil {
		t.Fatal(err)
	}
	if !none.IsRestore || len(none.LoopbackLinks) != 1 || none.TimeTravel != nil {
		t.Fatalf("none network args=%+v", none)
	}
	ttc, err := dehydrationNetworkArgs(ttcbundle.ContainerInfo{
		NetworkMode: "ttc", NetworkNAT: true, NetworkIP: "192.0.2.44", NetworkMAC: "02:aa:bb:cc:dd:ee",
	})
	if err != nil {
		t.Fatal(err)
	}
	if ttc.TimeTravel == nil || !ttc.TimeTravel.NAT || ttc.TimeTravel.GuestIP.String() != "192.0.2.44" {
		t.Fatalf("ttc network args=%+v", ttc)
	}
	if got := ttc.TimeTravel.GuestMAC.String(); got != "02:aa:bb:cc:dd:ee" {
		t.Fatalf("restored MAC=%q", got)
	}
	if _, err := dehydrationNetworkArgs(ttcbundle.ContainerInfo{NetworkMode: "host"}); err == nil {
		t.Fatal("host network unexpectedly accepted for process-free restore")
	}
}

func TestRemoveConsumedRehydrationPageSource(t *testing.T) {
	for _, test := range []struct {
		name      string
		branch    string
		transient bool
	}{
		{name: "durable dehydration image", branch: "main"},
		{name: "transient peer bootstrap", branch: "fresh", transient: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dataRoot := t.TempDir()
			containerID := "container-id"
			memoryRoot := filepath.Join(dataRoot, "containers", containerID, "memory")
			encodedBranch := hex.EncodeToString([]byte(test.branch))
			sourceDir := filepath.Join(memoryRoot, "dehydrated", encodedBranch)
			if test.transient {
				sourceDir = filepath.Join(memoryRoot, ".fresh-start-test")
			}
			if err := os.MkdirAll(sourceDir, 0700); err != nil {
				t.Fatal(err)
			}
			snapshotPath := filepath.Join(sourceDir, "memory-0000.snapshot")
			if err := os.WriteFile(snapshotPath, []byte("lazy page data"), 0600); err != nil {
				t.Fatal(err)
			}
			openSnapshot, err := os.Open(snapshotPath)
			if err != nil {
				t.Fatal(err)
			}
			defer openSnapshot.Close()

			var stderr bytes.Buffer
			b := &runtimeBackend{dataRoot: dataRoot, stderr: &stderr}
			branch := ttcbundle.BranchInfo{ID: test.branch, FilesystemBranch: test.branch}
			argument := sourceDir
			if !test.transient {
				argument = ""
			}
			b.retainRehydrationPageSource(containerID, branch, argument)
			if stderr.Len() != 0 {
				t.Fatalf("removing page source warned: %s", stderr.String())
			}
			if _, err := os.Lstat(sourceDir); !os.IsNotExist(err) {
				t.Fatalf("source directory still exists: %v", err)
			}
			got, err := io.ReadAll(openSnapshot)
			if err != nil || string(got) != "lazy page data" {
				t.Fatalf("open snapshot after unlink=(%q, %v)", got, err)
			}

			// Cleanup is idempotent after the source has been removed.
			b.retainRehydrationPageSource(containerID, branch, argument)
			if stderr.Len() != 0 {
				t.Fatalf("repeated cleanup warned: %s", stderr.String())
			}
		})
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
	if [ -e "$0.deleted" ]; then
		echo '[]'
		exit 0
	fi
	if [ -e "$0.stopped" ]; then
		echo '[{"id":"web","pid":0,"status":"stopped"}]'
	else
		echo '[{"id":"web","pid":4321,"status":"running"}]'
	fi
	exit 0
fi
if [ "$3" = "delete" ] && [ "$4" = "--force" ] && [ "$5" = "web" ]; then
	touch "$0.deleted"
	exit 0
fi
if [ "$3" = "timetravel" ] && [ "$4" = "web" ] && [ "$5" = "branches" ]; then
	if [ -e "$0.stopped" ]; then
		exit 128
	fi
	if [ -e "$0.child" ]; then
		echo '[{"ID":"main","ParentID":"","FilesystemBranch":"main","State":"running","ForkMetrics":{"SourceDowntime":0,"Kernel":{"Total":0},"Total":0}},{"ID":"child","ParentID":"main","FilesystemBranch":"child","State":"paused","ForkMetrics":{"SourceDowntime":1000000,"Kernel":{"Total":2000000},"Total":3000000}}]'
	else
		echo '[{"ID":"main","ParentID":"","State":"running","ForkMetrics":{"SourceDowntime":0,"Kernel":{"Total":0},"Total":0}}]'
	fi
	exit 0
fi
if [ "$3" = "timetravel" ] && [ "$4" = "--source" ] && [ "$5" = "main" ] && [ "$6" = "--paused" ] && [ "$7" = "web" ] && [ "$8" = "fork" ] && [ "$9" = "child" ]; then
	touch "$0.child"
	echo '{"ID":"child","ParentID":"main","FilesystemBranch":"child","State":"paused","ForkMetrics":{"SourceDowntime":1000000,"Kernel":{"Total":2000000},"Total":3000000}}'
	exit 0
fi
if [ "$3" = "timetravel" ] && [ "$4" = "web" ] && [ "$5" = "exec-list" ] && [ "$6" = "child" ]; then
	echo '[]'
	exit 0
fi

if [ "$3" = "timetravel" ] && [ "$4" = "--console" ] && [ "$5" = "init" ] && [ "$6" = "--console-log" ] && [ "$8" = "--console-log-max-bytes" ] && [ "$9" = "4096" ] && [ "${10}" = "--console-log-max-files" ] && [ "${11}" = "3" ] && [ "${12}" = "--console-wait-status" ] && [ "${14}" = "--console-retention-stats" ] && [ "${16}" = "--console-pid-file" ] && [ "${18}" = "--console-ready-fd" ] && [ "${19}" = "4" ] && [ "${20}" = "web" ] && [ "${21}" = "console-follow" ] && [ "${22}" = "child" ]; then
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
if [ "$3" = "timetravel" ] && [ "$4" = "web" ] && [ "$5" = "size" ] && [ "$6" = "child" ]; then
	echo '{"writable_bytes":32768}'
	exit 0
fi
if [ "$3" = "timetravel" ] && [ "$4" = "--path" ] && { [ "$5" = "/cache" ] || [ "$5" = "/cache-ro" ]; } && [ "$6" = "web" ] && [ "$7" = "filesystem-size" ] && [ "$8" = "child" ]; then
	echo '{"writable_bytes":12288}'
	exit 0
fi
if [ "$3" = "timetravel" ] && [ "$4" = "--path" ] && [ "$5" = "/cache" ] && [ "$6" = "--output" ] && [ "$8" = "web" ] && [ "$9" = "filesystem-tar" ] && [ "${10}" = "child" ]; then
	printf fake-tar >"$7"
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
	if err := writeCurrentBundleFixture(filepath.Join(containerDir, "container.json"), ttcbundle.ContainerInfo{
		Name: "web", BundleDir: bundleDir, ImageSize: 2217006,
		ConsoleLogMaxBytes: 4096, ConsoleLogMaxFiles: 3, State: "running", CreatedAt: time.Now(),
		Branches: []ttcbundle.BranchInfo{{ID: "main", State: "running"}},
	}); err != nil {
		t.Fatalf("writing metadata: %v", err)
	}
	consoleDir := filepath.Join(containerDir, "console")
	if err := os.MkdirAll(consoleDir, 0700); err != nil {
		t.Fatalf("creating console directory: %v", err)
	}
	retentionData := []byte(`{"evicted_bytes":1234,"evicted_generations":2,"updated_at":"2026-07-17T00:00:00Z"}`)
	if err := os.WriteFile(ttcconsole.RetentionStatsPath(filepath.Join(consoleDir, "main.log")), retentionData, 0600); err != nil {
		t.Fatalf("writing retention stats: %v", err)
	}
	staleRotatedLog := filepath.Join(consoleDir, "child.log"+ttcconsole.RotatedSuffix)
	if err := os.WriteFile(staleRotatedLog, []byte("stale"), 0600); err != nil {
		t.Fatalf("writing stale rotated log: %v", err)
	}
	staleOlderLog := ttcconsole.GenerationPath(filepath.Join(consoleDir, "child.log"), 2)
	if err := os.WriteFile(staleOlderLog, []byte("older"), 0600); err != nil {
		t.Fatalf("writing older stale log: %v", err)
	}
	b := &runtimeBackend{root: "/runtime", runsc: runsc, dataRoot: dataRoot}
	families, err := b.listFamilies()
	if err != nil {
		t.Fatalf("listFamilies: %v", err)
	}
	if len(families) != 1 || families[0].ID != "web" || families[0].ImageSize != 2217006 || len(families[0].Branches) != 1 || families[0].Branches[0].ID != "main" {
		t.Fatalf("listFamilies returned %+v", families)
	}
	if got, err := b.rootfsSize(selector{container: "web", branch: "child"}); err != nil || got != 32768 {
		t.Fatalf("rootfsSize(child)=(%d, %v), want (32768, nil)", got, err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte(`{"root":{"readonly":true}}`), 0600); err != nil {
		t.Fatalf("writing read-only config: %v", err)
	}
	if got, err := b.rootfsSize(selector{container: "web", branch: "main"}); err != nil || got != 0 {
		t.Fatalf("rootfsSize(read-only main)=(%d, %v), want (0, nil)", got, err)
	}
	inspected, err := b.inspect("web")
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if inspected.State.Status != "running" || inspected.State.Pid != 4321 || len(inspected.TTC.Branches) != 1 || len(inspected.TTC.Branches[0].Processes) != 1 || inspected.TTC.Branches[0].Processes[0].PID != 1 || inspected.TTC.Branches[0].OutputRetention == nil || inspected.TTC.Branches[0].OutputRetention.EvictedBytes != 1234 {
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
	if err := b.destroy(selector{container: "web", branch: "child"}, false); err == nil || !strings.Contains(err.Error(), "use -f to force removal") {
		t.Fatalf("unforced paused-branch removal error=%v, want force diagnostic", err)
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
	if err := b.signal(selector{container: "web", branch: "child"}, 15, false); err != nil {
		t.Fatalf("signal: %v", err)
	}
	status, err := b.wait(selector{container: "web", branch: "child"})
	if err != nil || status != 15 {
		t.Errorf("wait returned (%d, %v), want (15, nil)", status, err)
	}
	store := ttcbundle.Store{Root: dataRoot}
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
	if err := b.destroy(selector{container: "web", branch: "child"}, true); err == nil {
		t.Error("destroy swallowed a live-sandbox control error")
	}
	if err := os.WriteFile(runsc+".stopped", nil, 0600); err != nil {
		t.Fatalf("marking fake sandbox stopped: %v", err)
	}
	families, err = b.listFamilies()
	if err != nil {
		t.Fatalf("listFamilies after runtime stopped: %v", err)
	}
	mainState := ""
	if len(families) == 1 {
		for _, branch := range families[0].Branches {
			if branch.ID == "main" {
				mainState = branch.State
			}
		}
	}
	if len(families) != 1 || families[0].State != "exited" || mainState != "exited" {
		t.Fatalf("stopped runtime leaked through TTC state: %+v", families)
	}
	if err := b.destroy(selector{container: "web", branch: "child"}, true); err != nil {
		t.Errorf("destroy after sandbox exit: %v", err)
	}
	if err := b.destroy(selector{container: "missing", branch: "child"}, true); err == nil {
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
	if err := writeCurrentBundleFixture(filepath.Join(containerDir, "container.json"), ttcbundle.ContainerInfo{
		ID: id, Name: "web", BundleDir: bundleDir, State: "running", CreatedAt: time.Now(),
	}); err != nil {
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
		if info.ID != id || info.Name != "/web" || len(info.TTC.Branches) != 1 || len(info.TTC.Branches[0].Processes) != 1 {
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
	if err := b.remove("web", true, false); err != nil {
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
   [ "$3" = "exec" ] && [ "$4" = "--ttc-allow-stopped-sandbox" ] &&
   [ "$5" = "--branch" ] && [ "$6" = "child" ] &&
   [ "$7" = "--exec-id" ] && [ "$8" = "exec-test" ] &&
   [ "$9" = "--internal-console" ] && [ "${10}" = "ttc-exec-test" ] &&
   [ "${11}" = "--env" ] && [ "${12}" = "A=1" ] &&
   [ "${13}" = "--cwd" ] && [ "${14}" = "/work" ] &&
   [ "${15}" = "--user" ] && [ "${16}" = "123:456" ] &&
   [ "${17}" = "--additional-gids" ] && [ "${18}" = "12" ] &&
   [ "${19}" = "--additional-gids" ] && [ "${20}" = "34" ] &&
   [ "${21}" = "--cap" ] && [ "${22}" = "CAP_CHOWN" ] &&
   [ "${23}" = "--cap" ] && [ "${24}" = "CAP_SYS_PTRACE" ] &&
   [ "${25}" = "web" ] && [ "${26}" = "echo" ] && [ "${27}" = "ok" ]; then
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
		execID:         "exec-test",
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
   [ "$3" = "exec" ] && [ "$4" = "--ttc-allow-stopped-sandbox" ] &&
   [ "$5" = "--branch" ] && [ "$6" = "child" ] &&
   [ "$7" = "--exec-id" ] && [ "$8" = "exec-test" ] &&
   [ "$9" = "--internal-console" ] && [ "${10}" = "ttc-exec-test" ] &&
   [ "${11}" = "--process" ] && [ "${12}" = "/process.json" ] &&
   [ "${13}" = "web" ] && [ -z "${14}" ]; then
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
		execID:      "exec-test",
		processPath: "/process.json",
	})
	if err != nil || status != 0 {
		t.Fatalf("exec --process returned status=%d err=%v stderr=%q", status, err, stderr.String())
	}
}

func TestRuntimeBackendTerminalExecUsesClientTERM(t *testing.T) {
	dir := t.TempDir()
	runsc := filepath.Join(dir, "runsc")
	script := `#!/bin/sh
case " $* " in
  *" --env TERM=tmux-256color "*) ;;
  *) echo "client TERM missing: $*" >&2; exit 2 ;;
esac
case " $* " in
  *" --internal-console-terminal "*) ;;
  *) echo "terminal exec flags missing: $*" >&2; exit 2 ;;
esac
case " $* " in
  *" web true ") exit 0 ;;
  *) echo "terminal exec command missing: $*" >&2; exit 2 ;;
esac
`
	if err := os.WriteFile(runsc, []byte(script), 0755); err != nil {
		t.Fatalf("writing fake runsc: %v", err)
	}
	var stderr bytes.Buffer
	b := &runtimeBackend{root: "/runtime", runsc: runsc, clientTERM: "tmux-256color", stderr: &stderr}
	status, err := b.exec(execRequest{
		target:   selector{container: "web", branch: "main"},
		args:     []string{"true"},
		terminal: true,
	})
	if err != nil || status != 0 {
		t.Fatalf("terminal exec returned status=%d err=%v stderr=%q", status, err, stderr.String())
	}
}

func TestRuntimeBackendTerminalExecPreservesExplicitTERM(t *testing.T) {
	dir := t.TempDir()
	runsc := filepath.Join(dir, "runsc")
	script := `#!/bin/sh
case " $* " in
  *" --env TERM=screen-256color "*) ;;
  *) echo "explicit TERM missing: $*" >&2; exit 2 ;;
esac
case " $* " in
  *" --env TERM=xterm-256color "*) echo "client TERM overrode explicit TERM: $*" >&2; exit 2 ;;
esac
exit 0
`
	if err := os.WriteFile(runsc, []byte(script), 0755); err != nil {
		t.Fatalf("writing fake runsc: %v", err)
	}
	var stderr bytes.Buffer
	b := &runtimeBackend{root: "/runtime", runsc: runsc, clientTERM: "xterm-256color", stderr: &stderr}
	status, err := b.exec(execRequest{
		target:   selector{container: "web", branch: "main"},
		args:     []string{"true"},
		env:      []string{"TERM=screen-256color"},
		terminal: true,
	})
	if err != nil || status != 0 {
		t.Fatalf("terminal exec returned status=%d err=%v stderr=%q", status, err, stderr.String())
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
	stopped) printf '[{"id":"web","pid":%s,"status":"stopped"}]\n' "$PPID" ;;
	running) printf '[{"id":"web","pid":%s,"status":"running"}]\n' "$PPID" ;;
	stale) echo '[{"id":"web","pid":2147483647,"status":"running"}]' ;;
	transition)
		count=$(cat "$0.transition")
		if [ "$count" -lt 2 ]; then
			echo $((count + 1)) >"$0.transition"
			printf '[{"id":"web","pid":%s,"status":"running"}]\n' "$PPID"
		else
			printf '[{"id":"web","pid":%s,"status":"stopped"}]\n' "$PPID"
		fi
		;;
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
	if err := b.removeStoppedRuntime("web", false); err != nil {
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
	if err := b.removeStoppedRuntime("web", false); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("removeStoppedRuntime(running)=%v, want already-running error", err)
	}
	if _, err := os.Stat(runsc + ".deleted"); !os.IsNotExist(err) {
		t.Errorf("running runtime delete marker err=%v", err)
	}
	if err := os.WriteFile(runsc+".mode", []byte("stale\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := b.removeStoppedRuntime("web", false); err != nil {
		t.Fatalf("removeStoppedRuntime(stale): %v", err)
	}
	if _, err := os.Stat(runsc + ".deleted"); err != nil {
		t.Errorf("stale runtime was not deleted: %v", err)
	}
	if err := os.Remove(runsc + ".deleted"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runsc+".mode", []byte("transition\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runsc+".transition", []byte("0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := b.removeStoppedRuntime("web", true); err != nil {
		t.Fatalf("removeStoppedRuntime(transition): %v", err)
	}
	if _, err := os.Stat(runsc + ".deleted"); err != nil {
		t.Errorf("transitioned runtime was not deleted: %v", err)
	}
}

func TestSetCommandStdinDoesNotHoldWaitOpen(t *testing.T) {
	input, inputWriter := io.Pipe()
	command := exec.Command("/bin/sh", "-c", "exit 0")
	cleanup, err := setCommandStdin(command, input)
	if err != nil {
		t.Fatalf("setCommandStdin: %v", err)
	}
	defer cleanup()
	defer inputWriter.Close()
	defer input.Close()
	if _, ok := command.Stdin.(*os.File); !ok {
		t.Fatalf("command stdin type=%T, want *os.File", command.Stdin)
	}
	if err := command.Start(); err != nil {
		t.Fatalf("starting command: %v", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("waiting for command: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("command Wait blocked on still-open daemon stdin")
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
	if err := writeCurrentBundleFixture(filepath.Join(dataRoot, "containers", "demo", "container.json"), ttcbundle.ContainerInfo{
		Name:      "demo",
		Reference: "docker.io/library/alpine:3.20",
		Digest:    "sha256:abcd",
		BundleDir: bundleDir,
		State:     "created",
		CreatedAt: createdAt,
	}); err != nil {
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

	if families[0].Image != "docker.io/library/alpine:3.20" || families[0].Digest != "sha256:abcd" || !families[0].CreatedAt.Equal(createdAt) || !families[0].Branches[0].CreatedAt.Equal(createdAt) {
		t.Errorf("persistent family metadata=%+v", families[0])
	}
	store := ttcbundle.Store{Root: dataRoot}
	if err := store.UpdateState("demo", "running", nil); err != nil {
		t.Fatalf("marking container running: %v", err)
	}
	consolePaths, err := store.BranchConsolePaths("demo", defaultBranchID)
	if err != nil {
		t.Fatalf("BranchConsolePaths: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(consolePaths.WaitStatus), 0700); err != nil {
		t.Fatalf("creating console directory: %v", err)
	}
	if err := os.WriteFile(consolePaths.WaitStatus, []byte("11\n"), 0600); err != nil {
		t.Fatalf("writing crash wait status: %v", err)
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
	if info.State != "exited" || info.ExitedAt == nil || info.ExitStatus == nil || *info.ExitStatus != 139 {
		t.Errorf("reconciled metadata = %+v, want exited with status 139 and timestamp", info)
	}
	inspected, err := b.inspect("demo")
	if err != nil {
		t.Fatalf("inspect after reconciliation: %v", err)
	}
	if inspected.State.Status != "exited" || inspected.State.ExitCode != 139 {
		t.Errorf("inspect after reconciliation = %+v, want exited with status 139", inspected.State)
	}
}

func TestListFamiliesIncludesRemotePlacementWithoutReconcilingIt(t *testing.T) {
	dir := t.TempDir()
	runsc := filepath.Join(dir, "runsc")
	if err := os.WriteFile(runsc, []byte("#!/bin/sh\n[ \"$3\" = list ] && { echo '[]'; exit 0; }\nexit 2\n"), 0755); err != nil {
		t.Fatal(err)
	}
	dataRoot := filepath.Join(dir, "data")
	bundleDir := filepath.Join(dataRoot, "containers", "remote", "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentBundleFixture(filepath.Join(dataRoot, "containers", "remote", "container.json"), ttcbundle.ContainerInfo{
		ID: "remote", Name: "remote", NodeID: "node-b", BundleDir: bundleDir,
		State: "running", CreatedAt: time.Now(),
		Branches: []ttcbundle.BranchInfo{{ID: defaultBranchID, State: "running", CreatedAt: time.Now()}},
	}); err != nil {
		t.Fatal(err)
	}
	b := &runtimeBackend{root: "/runtime", runsc: runsc, dataRoot: dataRoot, nodeID: "node-a"}
	families, err := b.listFamilies()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 1 || families[0].ID != "remote" || families[0].NodeID != "node-b" || families[0].State != "running" {
		t.Fatalf("remote families=%+v", families)
	}
	stored, err := (ttcbundle.Store{Root: dataRoot}).LoadContainer("remote")
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != "running" {
		t.Fatalf("remote metadata was reconciled locally: %+v", stored)
	}
}

func TestAllBranchesColdOnNodeIgnoresRemoteRunningPeers(t *testing.T) {
	info := ttcbundle.ContainerInfo{
		NodeID: "node-a",
		Branches: []ttcbundle.BranchInfo{
			{ID: "local", NodeID: "node-a", State: "dehydrated"},
			{ID: "remote", NodeID: "node-b", State: "running"},
		},
	}
	allCold, count := allBranchesColdOnNode(info, "node-a")
	if !allCold || count != 1 {
		t.Fatalf("allBranchesColdOnNode(node-a)=(%t,%d), want (true,1)", allCold, count)
	}
	allCold, count = allBranchesColdOnNode(info, "")
	if allCold || count != 2 {
		t.Fatalf("allBranchesColdOnNode(legacy)=(%t,%d), want (false,2)", allCold, count)
	}
}

func TestListFamiliesReconcilesDeadListedSandbox(t *testing.T) {
	dir := t.TempDir()
	runsc := filepath.Join(dir, "runsc")
	script := `#!/bin/sh
if [ "$1" != "--root" ] || [ "$2" != "/runtime" ]; then
	echo "unexpected globals: $*" >&2
	exit 2
fi
if [ "$3" = "list" ]; then
	if [ "$(cat "$0.mode")" = "live" ]; then
		pid=$PPID
	else
		pid=2147483647
	fi
	printf '[{"id":"web","pid":%s,"status":"running"}]\n' "$pid"
	exit 0
fi
if [ "$3" = "timetravel" ] && [ "$4" = "web" ] && [ "$5" = "branches" ]; then
	echo 'connecting to control server: connection refused' >&2
	exit 128
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
	if err := os.WriteFile(runsc+".mode", []byte("live\n"), 0600); err != nil {
		t.Fatal(err)
	}

	dataRoot := filepath.Join(dir, "data")
	containerDir := filepath.Join(dataRoot, "containers", "web")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	createdAt := time.Now().UTC()
	if err := writeCurrentBundleFixture(filepath.Join(containerDir, "container.json"), ttcbundle.ContainerInfo{
		ID: "web", Name: "web", BundleDir: bundleDir, State: "running", CreatedAt: createdAt,
		Branches: []ttcbundle.BranchInfo{
			{ID: "main", State: "exited", CreatedAt: createdAt},
			{ID: "test", ParentID: "main", State: "running", CreatedAt: createdAt},
		},
	}); err != nil {
		t.Fatal(err)
	}
	store := ttcbundle.Store{Root: dataRoot}
	consolePaths, err := store.BranchConsolePaths("web", defaultBranchID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(consolePaths.WaitStatus), 0700); err != nil {
		t.Fatal(err)
	}
	// This belongs to main's earlier exit and must not be attributed to test.
	if err := os.WriteFile(consolePaths.WaitStatus, []byte("0\n"), 0600); err != nil {
		t.Fatal(err)
	}

	b := &runtimeBackend{root: "/runtime", runsc: runsc, dataRoot: dataRoot}
	if _, err := b.listFamilies(); err != nil {
		t.Fatalf("listFamilies with live Sentry: %v", err)
	}
	info, err := store.LoadContainer("web")
	if err != nil {
		t.Fatal(err)
	}
	if info.State != "running" || info.Branches[1].State != "running" {
		t.Fatalf("transient RPC failure reconciled a live Sentry: %+v", info)
	}

	if err := os.WriteFile(runsc+".mode", []byte("stale\n"), 0600); err != nil {
		t.Fatal(err)
	}
	families, err := b.listFamilies()
	if err != nil {
		t.Fatalf("listFamilies with dead Sentry: %v", err)
	}
	if len(families) != 1 || families[0].State != "exited" || families[0].SandboxPID != 0 || families[0].Error == "" {
		t.Fatalf("dead listed sandbox was not reconciled: %+v", families)
	}
	info, err = store.LoadContainer("web")
	if err != nil {
		t.Fatal(err)
	}
	if info.State != "exited" || info.ExitStatus != nil {
		t.Fatalf("reconciled metadata=%+v, want exited with unknown status", info)
	}
	for _, branch := range info.Branches {
		if branch.State != "exited" {
			t.Errorf("branch %q state=%q, want exited", branch.ID, branch.State)
		}
	}
	if err := b.removeStoppedRuntime("web", false); err != nil {
		t.Fatalf("removeStoppedRuntime(stale): %v", err)
	}
	if _, err := os.Stat(runsc + ".deleted"); err != nil {
		t.Errorf("stale runtime was not deleted before restart: %v", err)
	}
}

type signalCall struct {
	target selector
	signal int
	all    bool
}

type startCall struct {
	target      selector
	detach      bool
	interactive bool
}

type fakeBackend struct {
	loadedBytes         int
	loadedPlatform      string
	savedIdentifiers    []string
	savedPlatform       string
	mu                  sync.Mutex
	families            []Family
	processTarget       selector
	processList         []ProcessInfo
	ttyTarget           selector
	ttySession          string
	ttyObservation      kernel.InternalConsoleObservation
	rootfsSizes         map[selector]uint64
	diffChanges         []BranchPathChange
	resourceStatsTarget selector
	resourceStatsResult FamilyResourceStats
	sizeCalls           []selector
	sizeErr             error
	err                 error
	killErr             error
	forkSource          selector
	forkDestination     string
	forkDestinations    []string
	forkPaused          bool
	migrateTarget       selector
	migrateNode         string
	publishedTarget     selector
	publishedBinding    ttcbundle.PortBinding
	portListContainer   string
	portList            []ttcbundle.PortBinding
	unpublishedTarget   selector
	unpublishedBinding  ttcbundle.PortBinding
	paused              selector
	resumed             selector
	quarantineTarget    selector
	quarantineEnabled   bool
	preparedStop        selector
	preparedStops       []selector
	discarded           selector
	discards            []selector
	dehydratedTargets   []selector
	rehydratedTargets   []selector
	destroyed           selector
	destroyForce        bool
	signals             []signalCall
	waited              selector
	waitCalls           []selector
	waitStatus          uint32
	waitStatuses        map[selector]uint32
	waitRelease         chan struct{}
	pullReference       string
	pullPlatform        string
	pullInfo            ttcimages.Info
	imageList           []ImageInfo
	imageInspectID      string
	imageInspectInfo    ImageInspectInfo
	imageRemoveID       string
	imageRemoveForce    bool
	imageRemoveResult   ImageRemoveResult
	commitTarget        selector
	commitReference     string
	commitOptions       imageCommitOptions
	commitInfo          ttcimages.Info
	runRequest          runRequest
	containerID         string
	runStatus           int
	created             bool
	started             selector
	startCalls          []startCall
	startDetach         bool
	startInteractive    bool
	attached            attachRequest
	logsTarget          selector
	logsOptions         logsOptions
	inspectInfo         InspectInfo
	removed             string
	removeForce         bool
	removeVolumes       bool
	execRequest         execRequest
	execStatus          int
	execSessionList     []runscExecSession
	execControlTarget   selector
	execControlID       string
	execControlSignal   int
	copyStdin           io.Reader
	copyExportTarget    selector
	copyExportPath      string
	copyExportFollow    bool
	copyExportData      []byte
	copyImportTarget    selector
	copyImportPath      string
	copyImportArchive   bool
	copyImportRaw       bool
	copyImportData      []byte
	checkpointRequest   checkpointRequest
	restoreSource       string
	restoreName         string
	renamed             []string
}

func (b *fakeBackend) migrate(target selector, node string) error {
	b.migrateTarget = target
	b.migrateNode = node
	return b.err
}

func (b *fakeBackend) resourceStats(target selector) (FamilyResourceStats, error) {
	b.resourceStatsTarget = target
	return b.resourceStatsResult, b.err
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

func (b *fakeBackend) start(target selector, detach, interactive bool) (int, error) {
	b.startCalls = append(b.startCalls, startCall{target: target, detach: detach, interactive: interactive})
	b.started = target
	b.startDetach = detach
	b.startInteractive = interactive
	return b.runStatus, b.err
}

func (b *fakeBackend) attach(request attachRequest) (uint32, error) {
	b.attached = request
	b.attached.detachKeys = append([]byte(nil), request.detachKeys...)
	return b.waitStatus, b.err
}

func (b *fakeBackend) logs(target selector, options logsOptions) error {
	b.logsTarget = target
	b.logsOptions = options
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

func (b *fakeBackend) remove(name string, force, volumes bool) error {
	b.removed = name
	b.removeForce = force
	b.removeVolumes = volumes
	return b.err
}

func (b *fakeBackend) loadImages(archive io.Reader, platform string) ([]ttcimages.Info, error) {
	if archive == nil {
		return nil, errors.New("no image archive on standard input")
	}
	data, err := io.ReadAll(archive)
	if err != nil {
		return nil, err
	}
	b.loadedBytes = len(data)
	b.loadedPlatform = platform
	return []ttcimages.Info{
		{Reference: "docker.io/library/one:latest", Digest: "sha256:one", Platform: platform},
		{Reference: "docker.io/library/two:test", Digest: "sha256:two", Platform: platform},
	}, nil
}

func (b *fakeBackend) saveImages(archive io.Writer, identifiers []string, platform string) error {
	b.savedIdentifiers = append([]string(nil), identifiers...)
	b.savedPlatform = platform
	_, err := archive.Write([]byte("image archive"))
	return err
}

func (b *fakeBackend) pull(reference, platform string, progress bool) (ttcimages.Info, error) {
	b.pullReference = reference
	b.pullPlatform = platform
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

func (b *fakeBackend) commit(target selector, reference string, options imageCommitOptions) (ttcimages.Info, error) {
	b.commitTarget = target
	b.commitReference = reference
	b.commitOptions = options
	b.commitOptions.Changes = append([]string(nil), options.Changes...)
	return b.commitInfo, b.err
}

func (b *fakeBackend) listFamilies() ([]Family, error) {
	return b.families, b.err
}

func (b *fakeBackend) processes(target selector) ([]ProcessInfo, error) {
	b.processTarget = target
	return b.processList, b.err
}

func (b *fakeBackend) consoleObservation(target selector, session string) (kernel.InternalConsoleObservation, error) {
	b.ttyTarget = target
	b.ttySession = session
	return b.ttyObservation, b.err
}

func (b *fakeBackend) watchConsole(target selector, session string, output io.Writer) error {
	b.ttyTarget = target
	b.ttySession = session
	if b.err != nil {
		return b.err
	}
	return json.NewEncoder(output).Encode(&b.ttyObservation)
}

func (b *fakeBackend) rootfsSize(target selector) (uint64, error) {
	b.sizeCalls = append(b.sizeCalls, target)
	if b.sizeErr != nil {
		return 0, b.sizeErr
	}
	return b.rootfsSizes[target], nil
}

func (b *fakeBackend) diff(target selector) ([]BranchPathChange, error) {
	b.processTarget = target
	return append([]BranchPathChange(nil), b.diffChanges...), b.err
}

func (b *fakeBackend) fork(source selector, destination string, paused bool) (Branch, error) {
	branches, err := b.forkMany(source, []string{destination}, paused, false)
	if err != nil {
		return Branch{}, err
	}
	return branches[0], nil
}

func (b *fakeBackend) forkMany(source selector, destinations []string, paused, quarantine bool) ([]Branch, error) {
	b.forkSource = source
	b.forkDestinations = append([]string(nil), destinations...)
	b.forkDestination = ""
	if len(destinations) != 0 {
		b.forkDestination = destinations[len(destinations)-1]
	}
	b.forkPaused = paused
	if b.err != nil {
		return nil, b.err
	}
	branches := make([]Branch, len(destinations))
	for i, destination := range destinations {
		branches[i] = Branch{ID: destination, ParentID: source.branch, State: "paused"}
	}
	return branches, nil
}

func (b *fakeBackend) publishPort(target selector, binding ttcbundle.PortBinding) error {
	b.publishedTarget = target
	b.publishedBinding = binding
	return b.err
}

func (b *fakeBackend) listPorts(container string) ([]ttcbundle.PortBinding, error) {
	b.portListContainer = container
	return append([]ttcbundle.PortBinding(nil), b.portList...), b.err
}

func (b *fakeBackend) unpublishPort(target selector, binding ttcbundle.PortBinding) error {
	b.unpublishedTarget = target
	b.unpublishedBinding = binding
	return b.err
}

func (b *fakeBackend) pause(target selector) error {
	b.paused = target
	return b.err
}

func (b *fakeBackend) resume(target selector) error {
	b.resumed = target
	return b.err
}

func (b *fakeBackend) setQuarantine(target selector, enabled bool) error {
	b.quarantineTarget = target
	b.quarantineEnabled = enabled
	return b.err
}

func (b *fakeBackend) renameBranch(target selector, name string) error {
	b.renamed = append(b.renamed, target.String()+"->"+name)
	return b.err
}

func (b *fakeBackend) prepareStop(target selector) error {
	b.preparedStop = target
	b.preparedStops = append(b.preparedStops, target)
	return b.err
}

func (b *fakeBackend) discard(target selector) error {
	b.discarded = target
	b.discards = append(b.discards, target)
	return b.err
}

func (b *fakeBackend) dehydrate(target selector) error {
	b.dehydratedTargets = append(b.dehydratedTargets, target)
	return b.err
}

func (b *fakeBackend) rehydrate(target selector) error {
	b.rehydratedTargets = append(b.rehydratedTargets, target)
	return b.err
}

func (b *fakeBackend) destroy(target selector, force bool) error {
	b.destroyed = target
	b.destroyForce = force
	return b.err
}

func (b *fakeBackend) signal(target selector, signal int, all bool) error {
	b.signals = append(b.signals, signalCall{target: target, signal: signal, all: all})
	if signal == 9 {
		if b.waitRelease != nil {
			close(b.waitRelease)
		}
		if b.killErr != nil {
			return b.killErr
		}
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

func (b *fakeBackend) copyInput() io.Reader {
	return b.copyStdin
}

func (b *fakeBackend) exportContainerPath(target selector, source string, followLink bool, output io.Writer) error {
	b.copyExportTarget = target
	b.copyExportPath = source
	b.copyExportFollow = followLink
	if _, err := output.Write(b.copyExportData); err != nil {
		return err
	}
	return b.err
}

func (b *fakeBackend) importContainerPath(target selector, destination string, preserveOwner, rawArchive bool, input io.Reader) error {
	b.copyImportTarget = target
	b.copyImportPath = destination
	b.copyImportArchive = preserveOwner
	b.copyImportRaw = rawArchive
	data, err := io.ReadAll(input)
	b.copyImportData = data
	if err != nil {
		return err
	}
	return b.err
}

func (b *fakeBackend) checkpoint(request checkpointRequest) error {
	b.checkpointRequest = request
	return b.err
}

func (b *fakeBackend) restoreCheckpoint(source, name string) (string, error) {
	b.restoreSource = source
	b.restoreName = name
	return b.containerID, b.err
}

func TestCheckpointAndRestoreCommands(t *testing.T) {
	b := &fakeBackend{containerID: "restored-id"}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"checkpoint", "--compression=none", "web@child", "/tmp/web.ttc"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("checkpoint status=%d, stderr=%q", status, stderr.String())
	}
	if b.checkpointRequest.target != (selector{container: "web", branch: "child"}) ||
		b.checkpointRequest.destination != "/tmp/web.ttc" ||
		b.checkpointRequest.compression != "none" {
		t.Fatalf("checkpoint request=%+v", b.checkpointRequest)
	}
	stdout.Reset()
	stderr.Reset()
	if status := runCommand([]string{"restore", "--name", "recovered", "/tmp/web.ttc"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("restore status=%d, stderr=%q", status, stderr.String())
	}
	if b.restoreSource != "/tmp/web.ttc" || b.restoreName != "recovered" {
		t.Fatalf("restore=(%q, %q)", b.restoreSource, b.restoreName)
	}
	if got := strings.TrimSpace(stdout.String()); got != "restored-id" {
		t.Fatalf("restore stdout=%q", got)
	}
}

func TestNormalizeCheckpointCommandArgs(t *testing.T) {
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		args []string
		path string
	}{
		{args: []string{"checkpoint", "--compression=none", "web", "snapshot.ttc"}, path: "snapshot.ttc"},
		{args: []string{"restore", "--name", "web", "snapshot.ttc"}, path: "snapshot.ttc"},
		{args: []string{"restore", "--node", "node-b", "--name", "web", "snapshot.ttc"}, path: "snapshot.ttc"},
	}
	for _, test := range tests {
		got, err := normalizeCheckpointCommandArgs(test.args)
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(workingDirectory, test.path)
		if got[len(got)-1] != want {
			t.Errorf("normalizeCheckpointCommandArgs(%q)=%q, want final path %q", test.args, got, want)
		}
	}
}

func TestCopyCheckpointHostDirectory(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	if err := os.MkdirAll(filepath.Join(source, "nested"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "data"), []byte("value"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nested/data", filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "snapshot")
	if err := copyCheckpointHostPath(source, destination); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(destination, "nested", "data"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "value" {
		t.Fatalf("copied data=%q", data)
	}
	target, err := os.Readlink(filepath.Join(destination, "link"))
	if err != nil {
		t.Fatal(err)
	}
	if target != "nested/data" {
		t.Fatalf("copied symlink=%q", target)
	}
}

func TestCopyCheckpointHostFileCreatesParent(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(source, []byte("value"), 0640); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "host-mounts", "5")
	if err := copyCheckpointHostPath(source, destination); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "value" {
		t.Fatalf("copied data=%q", data)
	}
}

func TestManagedVolumeMountParsing(t *testing.T) {
	request, err := parseMountRequests(
		[]string{"/anonymous", "named:/cache:ro,nocopy"},
		[]string{"type=volume,source=long,target=/long,readonly"},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(request.managed) != 3 {
		t.Fatalf("managed volumes=%+v, want three", request.managed)
	}
	if got := request.managed[0]; !got.Anonymous || got.Destination != "/anonymous" {
		t.Errorf("anonymous short volume=%+v", got)
	}
	if got := request.managed[1]; got.Name != "named" || got.Destination != "/cache" || !got.Readonly || !got.NoCopy {
		t.Errorf("named short volume=%+v", got)
	}
	if got := request.managed[2]; got.Name != "long" || got.Destination != "/long" || !got.Readonly {
		t.Errorf("long volume=%+v", got)
	}
}

func TestMergeBranchHistoryUsesPersistedReparenting(t *testing.T) {
	now := time.Now().UTC()
	merged := mergeBranchHistory(
		[]Branch{{ID: "child", ParentID: "", Quarantined: true, State: "running", CreatedAt: now, SnapshotID: 41, MemoryRoots: []string{"memory/41/root"}, MemoryRootObjects: []string{"0000000000000029-03-0000000000000001"}}},
		[]Branch{{ID: "child", ParentID: "removed-parent", State: "paused", CreatedAt: now}},
	)
	if len(merged) != 1 || merged[0].ParentID != "" || !merged[0].Quarantined || merged[0].State != "paused" ||
		merged[0].SnapshotID != 41 || !slices.Equal(merged[0].MemoryRoots, []string{"memory/41/root"}) || !slices.Equal(merged[0].MemoryRootObjects, []string{"0000000000000029-03-0000000000000001"}) {
		t.Fatalf("merged branch=%+v, want persisted parent and live state", merged)
	}
}

func TestLocalGraphCaptureRejectsStaleSnapshot(t *testing.T) {
	dir := t.TempDir()
	control := `{"snapshot_id":8,"memory_roots":[{"name":"memory/8/root","root":"0000000000000008-03-0000000000000001"}]}`
	if err := os.WriteFile(filepath.Join(dir, "control.json"), []byte(control), 0600); err != nil {
		t.Fatal(err)
	}
	branch := ttcbundle.BranchInfo{SnapshotID: 8, MemoryRoots: []string{"memory/8/root"}, MemoryRootObjects: []string{"0000000000000008-03-0000000000000001"}}
	if !localGraphCaptureMatches(dir, branch) {
		t.Fatal("matching local graph capture was rejected")
	}
	branch.SnapshotID = 9
	if localGraphCaptureMatches(dir, branch) {
		t.Fatal("stale local graph capture was accepted")
	}
	if err := os.WriteFile(filepath.Join(dir, "control.json"), []byte("not-json"), 0600); err != nil {
		t.Fatal(err)
	}
	if localGraphCaptureMatches(dir, branch) {
		t.Fatal("malformed local graph capture was accepted")
	}
}

func TestValidateGraphRootObjectsAllowsInheritedOrigin(t *testing.T) {
	if err := validateGraphRootObjects(9, []string{"memory/branch/main/resource-0000"}, []string{"0000000000000008-03-0000000000000001"}); err != nil {
		t.Fatalf("inherited immutable origin was rejected: %v", err)
	}
	for _, test := range []struct {
		name    string
		objects []string
	}{
		{name: "missing", objects: nil},
		{name: "wrong kind", objects: []string{"0000000000000008-02-0000000000000001"}},
		{name: "malformed", objects: []string{"not-an-object"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateGraphRootObjects(9, []string{"memory/branch/main/resource-0000"}, test.objects); err == nil {
				t.Fatal("invalid immutable root-object metadata was accepted")
			}
		})
	}
}

func TestGraphReferenceRootsPreserveLogicalSlots(t *testing.T) {
	bindings := []cluster.SnapshotRootBinding{
		{Slot: 7, Object: "000000000000002a-03-0000000000000001"},
		{Slot: 11, Object: "000000000000002a-03-0000000000000001"},
	}
	roots, err := graphReferenceRoots(42, "tx-reference", bindings)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 2 || roots["remote/42/tx-reference/00000007"] != roots["remote/42/tx-reference/0000000b"] {
		t.Fatalf("reference roots=%+v", roots)
	}
	if _, err := graphReferenceRoots(42, "tx-reference", append(bindings, bindings[0])); err == nil {
		t.Fatal("duplicate logical slot was accepted")
	}
	if _, err := graphReferenceRoots(42, "tx-reference", []cluster.SnapshotRootBinding{{Slot: 1, Object: "0000000000000029-03-0000000000000001"}}); err == nil {
		t.Fatal("root from another origin was accepted")
	}
}

func TestGroupGraphRootBindingsByInheritedOrigin(t *testing.T) {
	groups, err := groupGraphRootBindings([]string{
		"000000000000002a-03-0000000000000001",
		"0000000000000011-03-0000000000000009",
		"000000000000002a-03-0000000000000007",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 {
		t.Fatalf("origin groups=%+v, want two", groups)
	}
	if got := groups[42]; len(got) != 2 || got[0].Slot != 0 || got[1].Slot != 2 {
		t.Fatalf("origin 42 bindings=%+v", got)
	}
	if got := groups[17]; len(got) != 1 || got[0].Slot != 1 {
		t.Fatalf("origin 17 bindings=%+v", got)
	}
	if _, err := groupGraphRootBindings([]string{"000000000000002a-01-0000000000000001"}); err == nil {
		t.Fatal("non-memory root was accepted")
	}
}

func TestCompletionScripts(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		t.Run(shell, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if status := runCommand([]string{"completion", shell}, &stdout, &stderr, &fakeBackend{}); status != 0 {
				t.Fatalf("status=%d stderr=%q", status, stderr.String())
			}
			if !strings.Contains(stdout.String(), "command ttc") || !strings.Contains(stdout.String(), "_complete") {
				t.Errorf("completion script=%q does not invoke dynamic completion", stdout.String())
			}
		})
	}
}

func TestDynamicCompletionUsesTTCObjects(t *testing.T) {
	b := &fakeBackend{
		families: []Family{{Name: "demo", Branches: []Branch{
			{ID: "main", State: "running"}, {ID: "child", State: "paused"},
		}}},
		imageList:       []ImageInfo{{Reference: "docker.io/library/alpine:3.20", Digest: "sha256:abc"}},
		execSessionList: []runscExecSession{{ID: "exec-123", State: "running"}},
	}
	tests := []struct {
		args    []string
		want    []string
		notWant string
	}{
		{args: []string{"_complete", "pa"}, want: []string{"pause"}},
		{args: []string{"_complete", "daemon", "start", ""}, want: []string{"--platform", "--unsafe-no-host-isolation"}},
		{args: []string{"_complete", "daemon", "start", "--platform", ""}, want: []string{"kvm", "systrap"}},
		{args: []string{"_complete", "unp"}, want: []string{"unpause"}},
		{args: []string{"_complete", "unpause", "demo@"}, want: []string{"demo@child", "demo@main"}, notWant: "demo@old"},
		{args: []string{"_complete", "pause", "demo@"}, want: []string{"demo@child", "demo@main"}, notWant: "demo@old"},
		{args: []string{"_complete", "cp", "demo@"}, want: []string{"demo@child", "demo@main"}, notWant: "demo@old"},
		{args: []string{"_complete", "rm", ""}, want: []string{"-f", "--force", "demo", "demo@child", "demo@main"}, notWant: "demo@old"},
		{args: []string{"_complete", "image", "inspect", "docker"}, want: []string{"docker.io/library/alpine:3.20"}},
		{args: []string{"_complete", "exec", "wait", "demo@main", "exec-"}, want: []string{"exec-123"}},
	}
	for _, test := range tests {
		var stdout, stderr bytes.Buffer
		if status := runCommand(test.args, &stdout, &stderr, b); status != 0 {
			t.Fatalf("%v status=%d stderr=%q", test.args, status, stderr.String())
		}
		lines := strings.Fields(stdout.String())
		for _, want := range test.want {
			if !slices.Contains(lines, want) {
				t.Errorf("%v output=%q, want %q", test.args, stdout.String(), want)
			}
		}
		if test.notWant != "" && slices.Contains(lines, test.notWant) {
			t.Errorf("%v output=%q unexpectedly contains %q", test.args, stdout.String(), test.notWant)
		}
	}
	if b.execControlTarget != (selector{container: "demo", branch: "main"}) {
		t.Errorf("exec completion target=%+v", b.execControlTarget)
	}
}

func TestParseManagedCoWHostMount(t *testing.T) {
	mounts, err := parseHostMounts(
		[]string{"/host/a:/data:cow,immutable-source"},
		[]string{"type=bind,source=/host/b,target=/config,managed-cow=true,immutable-source"},
	)
	if err != nil {
		t.Fatalf("parseHostMounts: %v", err)
	}
	if len(mounts) != 2 || !mounts[0].ManagedCoW || !mounts[1].ManagedCoW || mounts[0].Readonly || mounts[1].Readonly || !mounts[0].ImmutableSource || !mounts[1].ImmutableSource {
		t.Errorf("managed CoW host mounts=%+v", mounts)
	}
	for _, value := range []string{"/host:/data:ro,cow,immutable-source", "/host:/data:cow,copy"} {
		if _, err := parseHostMounts([]string{value}, nil); err == nil {
			t.Errorf("parseHostMounts accepted invalid managed CoW mount %q", value)
		}
	}
}

func TestParseCopiedHostMount(t *testing.T) {
	mounts, err := parseHostMounts(
		[]string{"/host/a:/data:copy"},
		[]string{"type=bind,source=/host/b,target=/config,copy=true"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 2 || !mounts[0].Copy || !mounts[1].Copy || mounts[0].ManagedCoW || mounts[1].ManagedCoW {
		t.Errorf("copied host mounts=%+v", mounts)
	}
}

func TestInspectManagedCoWHostMount(t *testing.T) {
	spec := &specs.Spec{
		Process: &specs.Process{},
		Mounts:  []specs.Mount{{Type: "bind", Source: "/host/lower", Destination: "/data", Options: []string{"rbind", "rw"}}},
		Annotations: map[string]string{
			ttcbundle.ImmutableHostMountsAnnotation:  `["/data"]`,
			ttcbundle.ManagedCoWHostMountsAnnotation: `["/data"]`,
		},
	}
	info := buildInspectInfo(ttcbundle.ContainerInfo{Name: "demo"}, spec, nil, "")
	if len(info.Mounts) != 1 || !info.Mounts[0].RW || info.Mounts[0].Mode != "rw,cow" {
		t.Errorf("managed CoW inspect mounts=%+v", info.Mounts)
	}
	if got, want := info.HostConfig.Binds, []string{"/host/lower:/data:rw,cow"}; !reflect.DeepEqual(got, want) {
		t.Errorf("managed CoW binds=%q, want %q", got, want)
	}
	if got := info.TTC.ExternalViews; len(got) != 1 || !got[0].ManagedCoW || !got[0].ImmutableSourceAsserted || got[0].Readonly || got[0].Semantics != "branch-local-persistent-cow-over-user-asserted-immutable-host-lower" {
		t.Errorf("managed CoW external view=%+v", got)
	}
}

func TestInspectCopiedHostMount(t *testing.T) {
	spec := &specs.Spec{
		Process: &specs.Process{},
		Mounts: []specs.Mount{{
			Type: "ttcvol", Source: "/run/ttc/host-mounts/id", Destination: "/data", Options: []string{"rw"},
		}},
		Annotations: map[string]string{
			ttcbundle.CopiedHostMountsAnnotation: `{"/data":"/host/source"}`,
		},
	}
	info := buildInspectInfo(ttcbundle.ContainerInfo{Name: "demo"}, spec, nil, "")
	if len(info.Mounts) != 1 || info.Mounts[0].Type != "bind" || info.Mounts[0].Source != "/host/source" || info.Mounts[0].Mode != "rw,copy" {
		t.Errorf("copied inspect mounts=%+v", info.Mounts)
	}
	if got, want := info.HostConfig.Binds, []string{"/host/source:/data:rw,copy"}; !reflect.DeepEqual(got, want) {
		t.Errorf("copied binds=%q, want %q", got, want)
	}
	if len(info.TTC.ExternalViews) != 0 {
		t.Errorf("copied mount reported as external view: %+v", info.TTC.ExternalViews)
	}
}

func TestColdBranchManifestRequiresGraphFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	legacy := coldBranchManifest{Version: 1, Container: "family", Branch: "main", Generation: 1, Kind: "stop"}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readColdBranchManifest(path); err == nil || !strings.Contains(err.Error(), "snapshot-graph format 3") {
		t.Fatalf("legacy cold manifest error=%v", err)
	}
	root := ttcsnapshot.ObjectID{Snapshot: 17, Ordinal: 1, Kind: ttcsnapshot.ObjectKindMetadata}
	current := coldBranchManifest{
		Version: 3, Container: "family", Branch: "main", Generation: 1, Kind: "stop",
		Filesystems: []coldFilesystemSnapshot{{Root: cluster.FilesystemRootDescriptor{
			Source: "rootfs-upper", RepositoryID: "0123456789abcdef0123456789abcdef",
			RestoreID: "0123456789abcdef0123456789abcdef",
			BranchID:  "main", Root: root.String(), NextInode: 2,
		}, Origins: []uint64{17}}},
	}
	data, err = json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := readColdBranchManifest(path); err != nil || len(got.Filesystems) != 1 {
		t.Fatalf("current cold manifest=%+v, %v", got, err)
	}
}

func TestColdStorageReclaimsRetiredGraphHandoffManifest(t *testing.T) {
	shared, local := t.TempDir(), t.TempDir()
	root := filepath.Join(shared, "cluster", "memory-manifests")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	retired := filepath.Join(root, "fork-retired.manifest")
	live := filepath.Join(root, "fork-live.manifest")
	unknown := filepath.Join(root, "operator-note.manifest")
	for _, path := range []string{retired, live, unknown} {
		if err := os.WriteFile(path, []byte("descriptor"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	catalog := transactionListCatalog{records: []cluster.Transaction{{
		Identity: cluster.ForkIdentity{TransactionID: "fork-live"},
		State:    cluster.TransactionPublished,
	}}}
	if err := collectColdStorage(shared, local, nil, catalog); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(retired); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired graph handoff manifest remains: %v", err)
	}
	if _, err := os.Stat(unknown); err != nil {
		t.Fatalf("unknown file was not left untouched: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("nonterminal graph handoff manifest was reclaimed: %v", err)
	}
}

func TestEmitCapacityRequestCoalescesUntilAcknowledged(t *testing.T) {
	dataRoot := t.TempDir()
	backend := &runtimeBackend{
		dataRoot: dataRoot,
		distributedSource: &distributedSourceRuntime{capabilities: cluster.Capabilities{
			Architecture: "amd64",
			RepositoryID: "shared-repository",
		}},
	}
	source := cluster.NodeRecord{
		NodeID:             "node-a",
		MemoryTargetBytes:  8 << 30,
		MemoryCurrentBytes: 9 << 30,
	}
	path, err := backend.emitCapacityRequest(source, "memory-pressure")
	if err != nil {
		t.Fatal(err)
	}
	read := func() elasticCapacityRequest {
		t.Helper()
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var request elasticCapacityRequest
		if err := json.Unmarshal(contents, &request); err != nil {
			t.Fatal(err)
		}
		return request
	}
	first := read()
	if first.Version != 1 || first.Operation != "acquire" || first.Nodes != 1 || first.MemoryBytesEach != source.MemoryTargetBytes || first.Architecture != "amd64" || first.RepositoryID != "shared-repository" || first.SourceNode != source.NodeID || first.Reason != "memory-pressure" || first.RequestID == "" || !first.Deadline.After(first.CreatedAt) {
		t.Fatalf("capacity request=%+v", first)
	}
	coalescedPath, err := backend.emitCapacityRequest(source, "node-drain")
	if err != nil {
		t.Fatal(err)
	}
	if coalescedPath != path {
		t.Fatalf("coalesced path=%q, want %q", coalescedPath, path)
	}
	if got := read(); got.RequestID != first.RequestID || got.Reason != first.Reason {
		t.Fatalf("coalesced request=%+v, want original ID and reason", got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	newPath, err := backend.emitCapacityRequest(source, "node-drain")
	if err != nil {
		t.Fatal(err)
	}
	if newPath != path {
		t.Fatalf("replacement path=%q, want %q", newPath, path)
	}
	if got := read(); got.RequestID == first.RequestID || got.Reason != "node-drain" {
		t.Fatalf("replacement request=%+v, original=%+v", got, first)
	}
}

func TestElasticDrainIntentSurvivesRetryAndIsConsumedAfterCompletion(t *testing.T) {
	dataRoot := t.TempDir()
	first, err := beginElasticDrainIntent(dataRoot, "node-a", "node-b")
	if err != nil {
		t.Fatal(err)
	}
	if first.State != elasticDrainActive || first.Attempts != 1 || first.OperationID == "" {
		t.Fatalf("first intent=%+v", first)
	}
	retry, err := beginElasticDrainIntent(dataRoot, "node-a", "node-b")
	if err != nil {
		t.Fatal(err)
	}
	if retry.OperationID != first.OperationID || retry.Attempts != 2 {
		t.Fatalf("retry intent=%+v, first=%+v", retry, first)
	}
	retargeted, err := beginElasticDrainIntent(dataRoot, "node-a", "node-c")
	if err != nil || retargeted.OperationID != first.OperationID || retargeted.TargetNode != "node-c" || retargeted.Attempts != 3 {
		t.Fatalf("retargeted intent=(%+v, %v)", retargeted, err)
	}
	attemptErr := errors.New("target temporarily unavailable")
	if err := finishElasticDrainAttempt(dataRoot, "node-a", first.OperationID, attemptErr); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadElasticDrainIntent(dataRoot, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil || loaded.State != elasticDrainActive || loaded.LastError != attemptErr.Error() || loaded.CompletedAt != nil {
		t.Fatalf("failed intent=%+v", loaded)
	}
	if err := finishElasticDrainAttempt(dataRoot, "node-a", first.OperationID, nil); err != nil {
		t.Fatal(err)
	}
	loaded, err = loadElasticDrainIntent(dataRoot, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil || loaded.State != elasticDrainComplete || loaded.LastError != "" || loaded.CompletedAt == nil {
		t.Fatalf("completed intent=%+v", loaded)
	}
	if err := removeCompletedElasticDrainIntent(dataRoot, "node-a"); err != nil {
		t.Fatal(err)
	}
	loaded, err = loadElasticDrainIntent(dataRoot, "node-a")
	if err != nil || loaded != nil {
		t.Fatalf("consumed intent=(%+v, %v), want nil", loaded, err)
	}
}

func TestElasticCommandsTakeMutationLock(t *testing.T) {
	for _, args := range [][]string{{"migrate", "--node", "node-b", "demo@main"}, {"node", "stop", "--migrate-all", "node-a"}, {"node", "rebalance", "node-a"}} {
		if !daemonCommandNeedsMutationLock(args) {
			t.Errorf("%v does not take the mutation lock", args)
		}
	}
	if daemonCommandNeedsMutationLock([]string{"node", "ls", "--json"}) {
		t.Error("node ls unnecessarily takes the mutation lock")
	}
}

func TestPersistentFamilyRequiredRuntime(t *testing.T) {
	container := ttcbundle.ContainerInfo{
		NodeID: "node-a",
		Branches: []ttcbundle.BranchInfo{
			{ID: "main", NodeID: "node-a", State: "created"},
			{ID: "cold", NodeID: "node-a", State: "dehydrated"},
			{ID: "remote", NodeID: "node-b", State: "running"},
		},
	}
	if persistentFamilyRequiredRuntime(container, "node-a") {
		t.Fatal("process-free local branches unexpectedly require a runtime")
	}
	if !persistentFamilyRequiredRuntime(container, "node-b") {
		t.Fatal("running remote branch did not require its node runtime")
	}
	container.Branches[0].State = "paused"
	if !persistentFamilyRequiredRuntime(container, "node-a") {
		t.Fatal("paused local branch did not require its runtime")
	}
}

func TestPendingDistributedTargetProtectsHiddenRuntime(t *testing.T) {
	container := ttcbundle.ContainerInfo{
		Branches: []ttcbundle.BranchInfo{{ID: "main", NodeID: "node-a", State: "running"}},
		DistributedForkImports: map[string]ttcbundle.DistributedForkImport{
			"tx-migrate": {
				BranchID: "main", TargetNode: "node-b", SourceNode: "node-a",
				Migration: true, State: "paused",
			},
			"tx-fork": {
				BranchID: "child", TargetNode: "node-b", SourceNode: "node-a",
				Migration: false, State: "paused",
			},
		},
	}
	if !pendingDistributedTarget(container, "main", "node-b") {
		t.Fatal("hidden migration target was not protected on its target node")
	}
	if pendingDistributedTarget(container, "main", "node-a") {
		t.Fatal("hidden migration target protected the source node")
	}
	if !pendingDistributedTarget(container, "child", "node-b") {
		t.Fatal("ordinary remote fork target was not protected before publication")
	}
	container.DistributedForkImports = nil
	container.Branches = append(container.Branches, ttcbundle.BranchInfo{
		ID: "child", NodeID: "node-b", State: "running", DistributedForkTransactionID: "tx-fork",
	})
	if !pendingDistributedTarget(container, "child", "node-b") {
		t.Fatal("visible target was not protected before Raft placement publication")
	}
}

func TestLocalFamilyStateNeeded(t *testing.T) {
	info := ttcbundle.ContainerInfo{NodeID: "node-a", Branches: []ttcbundle.BranchInfo{{ID: "main", State: "running"}}}
	if localFamilyStateNeeded(info, "node-b") {
		t.Fatal("remote-only family retained node-local state")
	}
	info.Branches = append(info.Branches, ttcbundle.BranchInfo{ID: "child", NodeID: "node-b", State: "running"})
	if !localFamilyStateNeeded(info, "node-b") {
		t.Fatal("locally owned branch did not retain node-local state")
	}
	info.Branches = info.Branches[:1]
	info.DistributedForkImports = map[string]ttcbundle.DistributedForkImport{
		"tx": {BranchID: "child", TargetNode: "node-b"},
	}
	if !localFamilyStateNeeded(info, "node-b") {
		t.Fatal("provisional import did not retain node-local state")
	}
}

func TestElasticNodeMutationRoutesToSourceBeforeLock(t *testing.T) {
	for _, args := range [][]string{
		{"node", "stop", "--migrate-all", "node-a"},
		{"node", "stop", "--_local", "--dehydrate-all", "node-a"},
		{"node", "rebalance", "node-a"},
	} {
		owner, err := daemonCommandOwner(t.TempDir(), args)
		if err != nil {
			t.Fatalf("daemonCommandOwner(%v): %v", args, err)
		}
		if owner != "node-a" {
			t.Errorf("daemonCommandOwner(%v)=%q, want node-a", args, owner)
		}
	}
}

func TestSelectAdmissionNodeUsesCompatibleLeastLoadedCapacity(t *testing.T) {
	capabilities := func(node string) cluster.Capabilities {
		return cluster.Capabilities{
			NodeID: node, ProtocolMin: 1, ProtocolMax: 1, BuildID: "test", Architecture: "amd64",
			BasePageSize: 4096, MaxTransferSize: 2 << 20, RepositoryID: "shared",
			Features: []string{cluster.FeatureRemotePull, cluster.FeatureNetworkRecreate, cluster.FeatureFilesystemBranch, cluster.FeatureSharedMemorySnapshot},
		}
	}
	now := time.Unix(7000, 0).UTC()
	record := func(node string, current, target, reserved uint64) cluster.NodeRecord {
		return cluster.NodeRecord{
			NodeID: node, Incarnation: "boot-" + node, State: cluster.NodeReady,
			Capabilities: capabilities(node), MemoryCurrentBytes: current, MemoryTargetBytes: target,
			IncomingReservedBytes: reserved, LeaseExpiresAt: now.Add(time.Minute),
		}
	}
	local := capabilities("node-a")
	incompatible := record("node-c", 1<<30, 8<<30, 0)
	incompatible.Capabilities.Architecture = "arm64"
	selected, err := selectAdmissionNode([]cluster.NodeRecord{
		record("node-a", 6<<30, 8<<30, 0),
		record("node-b", 2<<30, 8<<30, 1<<30),
		incompatible,
	}, local, now)
	if err != nil {
		t.Fatal(err)
	}
	if selected.NodeID != "node-b" {
		t.Fatalf("selected %q, want least-loaded compatible node-b", selected.NodeID)
	}
	full := record("node-b", 7<<30, 8<<30, 1<<30)
	if selected, err := selectAdmissionNode([]cluster.NodeRecord{full}, local, now); err == nil {
		t.Fatalf("selected full node %+v", selected)
	}
	selected, err = selectAdmissionNodeExcluding([]cluster.NodeRecord{
		record("node-a", 1<<30, 8<<30, 0),
		record("node-b", 2<<30, 8<<30, 0),
	}, local, now, map[string]struct{}{"node-a": {}})
	if err != nil {
		t.Fatal(err)
	}
	if selected.NodeID != "node-b" {
		t.Fatalf("selected excluded node %q, want node-b", selected.NodeID)
	}
}

func TestParseDistributedFamilyMutation(t *testing.T) {
	for _, test := range []struct {
		args    []string
		prefix  []string
		targets []string
		handle  bool
	}{
		{args: []string{"rm", "-fv", "demo"}, prefix: []string{"rm", "-fv"}, targets: []string{"demo"}, handle: true},
		{args: []string{"stop", "--time", "2", "demo", "peer@main"}, prefix: []string{"stop", "--time", "2"}, targets: []string{"demo", "peer@main"}, handle: true},
		{args: []string{"dehydrate", "demo"}, prefix: []string{"dehydrate"}, targets: []string{"demo"}, handle: true},
		{args: []string{"stop", "demo@main"}, handle: false},
		{args: []string{"pause", "demo"}, handle: false},
		{args: []string{"rm", "--unknown", "demo"}, handle: false},
	} {
		got, ok := parseDistributedFamilyMutation(test.args)
		if ok != test.handle {
			t.Errorf("parseDistributedFamilyMutation(%v) handled=%v, want %v", test.args, ok, test.handle)
			continue
		}
		if ok && (!slices.Equal(got.prefix, test.prefix) || !slices.Equal(got.targets, test.targets)) {
			t.Errorf("parseDistributedFamilyMutation(%v)=%+v, want prefix=%v targets=%v", test.args, got, test.prefix, test.targets)
		}
	}
	first := distributedChildRequestID("parent", 2, []string{"stop", "demo@main"})
	if _, err := daemonMutationPath(t.TempDir(), first); err != nil {
		t.Fatalf("child request ID %q is not a valid daemon mutation ID: %v", first, err)
	}
	if second := distributedChildRequestID("parent", 2, []string{"stop", "demo@main"}); first != second {
		t.Fatalf("child request ID is not deterministic: %q != %q", first, second)
	}
	if other := distributedChildRequestID("parent", 3, []string{"stop", "demo@main"}); first == other {
		t.Fatalf("different child operations collided at %q", first)
	}
}

func TestRunRenameNamesTheBranchAfterItsNewName(t *testing.T) {
	backend := &fakeBackend{}
	var out bytes.Buffer
	if err := runRename([]string{"demo@try-a", "main"}, &out, backend); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if len(backend.renamed) != 1 || backend.renamed[0] != "demo@try-a->main" {
		t.Fatalf("backend saw %v", backend.renamed)
	}
	if got := strings.TrimSpace(out.String()); got != "demo@main" {
		t.Fatalf("printed %q, want the branch under its new name", got)
	}
	if err := runRename([]string{"demo@try-a"}, &out, backend); err == nil {
		t.Fatal("a rename with no new name was accepted")
	}
}

func TestReconcileStandaloneMemoryGraphReclaimsCrashResidue(t *testing.T) {
	root := t.TempDir()
	graph, err := ttcsnapshot.Open(filepath.Join(root, "catalog", "snapshot-graph"))
	if err != nil {
		t.Fatal(err)
	}
	defer graph.Close()
	data, err := ttcsnapshot.OpenLocalDataStore(filepath.Join(root, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	origin, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	rootObject, err := graph.CreateObject(origin, "memory/crash/root-pin", ttcsnapshot.ObjectKindMetadata, []byte("root"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.SetRoot("memory/crash/resource-0000", rootObject); err != nil {
		t.Fatal(err)
	}
	if err := graph.DeletePin("memory/crash/root-pin"); err != nil {
		t.Fatal(err)
	}
	if _, err := graph.CreateObject(origin, "memory/crash/construction-pin", ttcsnapshot.ObjectKindMetadata, []byte("partial"), nil); err != nil {
		t.Fatal(err)
	}
	backend := &runtimeBackend{dataRoot: root, localRoot: root, snapshotGraph: graph, snapshotData: data}
	if err := backend.reconcileStandaloneMemoryGraph(); err != nil {
		t.Fatal(err)
	}
	stats := graph.Stats()
	if stats.Snapshots != 0 || stats.Objects != 0 || stats.Roots != 0 || stats.Pins != 0 || stats.Reclaiming != 0 {
		t.Fatalf("graph after reconciliation = %+v, want empty", stats)
	}
	if got := graph.JournalBytes(); got > 1024 {
		t.Fatalf("compacted journal has %d bytes, want at most 1024", got)
	}
}
