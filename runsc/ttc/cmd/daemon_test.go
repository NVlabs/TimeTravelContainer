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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	ttcpolicy "gvisor.dev/gvisor/pkg/ttcremote"
	"gvisor.dev/gvisor/pkg/ttcvolume"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
	"gvisor.dev/gvisor/runsc/ttc/cluster"
	"gvisor.dev/gvisor/runsc/version"
)

type transactionLoadCatalog struct {
	cluster.TransactionCatalog
	load func(string) (*cluster.Transaction, error)
}

func (c transactionLoadCatalog) Load(id string) (*cluster.Transaction, error) {
	return c.load(id)
}

type transactionListCatalog struct {
	cluster.TransactionCatalog
	records []cluster.Transaction
}

func TestDependencyMonitorIntervalBoundsAggregatePolling(t *testing.T) {
	for _, test := range []struct {
		watchers int64
		want     time.Duration
	}{
		{watchers: 1, want: 250 * time.Millisecond},
		{watchers: 4, want: 250 * time.Millisecond},
		{watchers: 16, want: time.Second},
		{watchers: 80, want: 5 * time.Second},
		{watchers: 1000, want: 5 * time.Second},
	} {
		if got := dependencyMonitorInterval(test.watchers); got != test.want {
			t.Errorf("dependencyMonitorInterval(%d)=%s, want %s", test.watchers, got, test.want)
		}
	}
}

func TestReusablePhysicalTarget(t *testing.T) {
	records := []cluster.PlacementRecord{
		{FamilyID: "family", BranchID: "source", OwnerNode: "source-node", PhysicalContainer: "source-runtime", State: cluster.PlacementRunning},
		{FamilyID: "family", BranchID: "sibling", OwnerNode: "target-node", PhysicalContainer: "target-runtime", State: cluster.PlacementPaused},
		{FamilyID: "other", BranchID: "main", OwnerNode: "target-node", PhysicalContainer: "other-runtime", State: cluster.PlacementRunning},
	}
	got, found, err := reusablePhysicalTarget(records, "family", "target-node")
	if err != nil || !found || got != "target-runtime" {
		t.Fatalf("reusablePhysicalTarget=%q,%t,%v, want target-runtime,true,nil", got, found, err)
	}

	records = append(records, cluster.PlacementRecord{
		FamilyID: "family", BranchID: "split", OwnerNode: "target-node", PhysicalContainer: "second-runtime", State: cluster.PlacementRunning,
	})
	if _, _, err := reusablePhysicalTarget(records, "family", "target-node"); err == nil {
		t.Fatal("reusablePhysicalTarget accepted multiple physical runtimes")
	}
}

func TestRemoteForkTargetUsesImmutableFamilyID(t *testing.T) {
	// A new branch is named as FAMILY@BRANCH by the client, so its transaction
	// may carry the user-facing family name even though placement authority is
	// keyed by the immutable ID from authenticated family metadata.
	source := ttcbundle.ContainerInfo{ID: "family-id", Name: "friendly-name"}
	physicalFamily := source.DistributedFamilyID
	if physicalFamily == "" {
		physicalFamily = source.ID
	}
	records := []cluster.PlacementRecord{{
		FamilyID: "family-id", BranchID: "first", OwnerNode: "target-node",
		PhysicalContainer: "target-runtime", State: cluster.PlacementRunning,
	}}
	got, found, err := reusablePhysicalTarget(records, physicalFamily, "target-node")
	if err != nil || !found || got != "target-runtime" {
		t.Fatalf("reusablePhysicalTarget=%q,%t,%v, want target-runtime,true,nil", got, found, err)
	}
}

func TestDependencyMonitorPollAdmissionIsNonBlocking(t *testing.T) {
	runtime := &daemonTargetRuntime{}
	releases := make([]func(), 0, dependencyMonitorConcurrency)
	for range dependencyMonitorConcurrency {
		release, ok := runtime.tryDependencyPoll()
		if !ok {
			t.Fatal("poll slot rejected below the concurrency limit")
		}
		releases = append(releases, release)
	}
	if _, ok := runtime.tryDependencyPoll(); ok {
		t.Fatal("poll slot admitted work above the concurrency limit")
	}
	releases[0]()
	if release, ok := runtime.tryDependencyPoll(); !ok {
		t.Fatal("released poll slot was not reusable")
	} else {
		release()
	}
	for _, release := range releases[1:] {
		release()
	}
}

func (c transactionListCatalog) List() ([]cluster.Transaction, error) {
	return append([]cluster.Transaction(nil), c.records...), nil
}

func TestDistributedSourceNoLongerNeeded(t *testing.T) {
	for _, test := range []struct {
		name string
		tx   *cluster.Transaction
		err  error
		want bool
	}{
		{name: "published", tx: &cluster.Transaction{State: cluster.TransactionPublished, SourceNeeded: true}},
		{name: "draining", tx: &cluster.Transaction{State: cluster.TransactionDraining, SourceNeeded: true}},
		{name: "source released", tx: &cluster.Transaction{State: cluster.TransactionPublished}, want: true},
		{name: "independent", tx: &cluster.Transaction{State: cluster.TransactionIndependent}, want: true},
		{name: "retired", err: os.ErrNotExist, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			catalog := transactionLoadCatalog{load: func(string) (*cluster.Transaction, error) { return test.tx, test.err }}
			got, err := distributedSourceNoLongerNeeded(catalog, "fork-race")
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("distributedSourceNoLongerNeeded=%t, want %t", got, test.want)
			}
		})
	}
}

func TestDistributedDrainRetryable(t *testing.T) {
	for _, test := range []struct {
		err  error
		want bool
	}{
		{err: context.DeadlineExceeded, want: true},
		{err: io.EOF, want: true},
		{err: errors.New("cluster peer: distributed memory drain is still in progress"), want: true},
		{err: errors.New("graph memory handoff was reinstalled and is still in progress"), want: true},
		{err: errors.New("distributed memory drain failed: remote page server: I/O error")},
		{err: errors.New("distributed filesystem drain failed: object is missing")},
	} {
		if got := distributedDrainRetryable(test.err); got != test.want {
			t.Errorf("distributedDrainRetryable(%q)=%t, want %t", test.err, got, test.want)
		}
	}
}

func TestDistributedDrainStepAdvanced(t *testing.T) {
	for _, test := range []struct {
		previous cluster.TransactionState
		current  cluster.TransactionState
		want     bool
	}{
		{cluster.TransactionPublished, cluster.TransactionDraining, true},
		{cluster.TransactionPublished, cluster.TransactionIndependent, true},
		{cluster.TransactionDraining, cluster.TransactionIndependent, true},
		{cluster.TransactionPublished, cluster.TransactionPublished, false},
		{cluster.TransactionDraining, cluster.TransactionDraining, false},
		{cluster.TransactionDraining, cluster.TransactionFailed, false},
	} {
		if got := distributedDrainStepAdvanced(test.previous, test.current); got != test.want {
			t.Errorf("distributedDrainStepAdvanced(%q, %q)=%t, want %t", test.previous, test.current, got, test.want)
		}
	}
}

func TestLocalNodeIncarnationPersistsForRootEpoch(t *testing.T) {
	root := t.TempDir()
	first, err := localNodeIncarnation(root, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := localNodeIncarnation(root, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if first == "" || second != first {
		t.Fatalf("local incarnation changed across reopen: first=%q second=%q", first, second)
	}
	if _, err := localNodeIncarnation(root, "node-b"); err == nil {
		t.Fatal("local root accepted a different node identity")
	}
}

func TestCleanRetirementRotatesLocalNodeIncarnation(t *testing.T) {
	root := t.TempDir()
	first, err := localNodeIncarnation(root, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := removeLocalNodeIncarnation(root, "node-a", first); err != nil {
		t.Fatal(err)
	}
	second, err := localNodeIncarnation(root, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatalf("cleanly retired incarnation was reused: %q", first)
	}
}

func TestLocalNodeIncarnationRejectsMalformedRecord(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "cluster", "node-incarnation.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"node_id":"node-a","incarnation":"not-hex"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := localNodeIncarnation(root, "node-a"); err == nil {
		t.Fatal("malformed local incarnation was accepted")
	}
}

func TestDaemonRequestOptionsUseServerDefaults(t *testing.T) {
	server := &daemonServer{
		socket:                "/run/ttc/ttcd.sock",
		root:                  "/run/ttc/runsc",
		runsc:                 "/opt/ttc/bin/ttcd",
		dataRoot:              "/var/lib/ttc",
		runtimePlatform:       "kvm",
		unsafeNoHostIsolation: true,
		disableHostGSO:        true,
	}
	opts := server.optionsFromRequest()
	if got, want := opts.root, server.root; got != want {
		t.Errorf("root=%q, want %q", got, want)
	}
	if got, want := opts.runsc, server.runsc; got != want {
		t.Errorf("runsc=%q, want %q", got, want)
	}
	if got, want := opts.dataRoot, server.dataRoot; got != want {
		t.Errorf("data root=%q, want %q", got, want)
	}
	if got, want := opts.daemonSocket, server.socket; got != want {
		t.Errorf("daemon socket=%q, want %q", got, want)
	}
	if got, want := opts.runtimePlatform, "kvm"; got != want {
		t.Errorf("runtime platform=%q, want %q", got, want)
	}
	if !opts.unsafeNoHostIsolation {
		t.Error("unsafe-no-host-isolation daemon mode was not applied")
	}
	if !opts.disableHostGSO {
		t.Error("daemon host-GSO compatibility mode was not applied")
	}
}

func TestDaemonRequestOptionsCannotOverrideServerRuntime(t *testing.T) {
	server := &daemonServer{
		root:     "/default/root",
		runsc:    "/default/ttcd",
		dataRoot: "/daemon/data",
	}
	opts := server.optionsFromRequest()
	if got, want := opts.root, server.root; got != want {
		t.Errorf("root=%q, want %q", got, want)
	}
	if got, want := opts.runsc, server.runsc; got != want {
		t.Errorf("runsc=%q, want %q", got, want)
	}
	if got, want := opts.dataRoot, server.dataRoot; got != want {
		t.Errorf("data root=%q, want daemon-owned %q", got, want)
	}
}

func TestHostKernelCompatibility(t *testing.T) {
	for _, test := range []struct {
		release        string
		version        hostKernelVersion
		disableHostGSO bool
		wantError      bool
	}{
		{release: "3.16.99", wantError: true},
		{release: "3.17", version: hostKernelVersion{major: 3, minor: 17}, disableHostGSO: true},
		{release: "3.17.0-custom", version: hostKernelVersion{major: 3, minor: 17}, disableHostGSO: true},
		{release: "4.14-legacy", version: hostKernelVersion{major: 4, minor: 14}, disableHostGSO: true},
		{release: "4.14.76-generic", version: hostKernelVersion{major: 4, minor: 14, patch: 76}, disableHostGSO: true},
		{release: "4.14.77-generic", version: hostKernelVersion{major: 4, minor: 14, patch: 77}},
		{release: "4.15.0", version: hostKernelVersion{major: 4, minor: 15}},
		{release: "6.8.0-60-generic", version: hostKernelVersion{major: 6, minor: 8}},
		{release: "", wantError: true},
		{release: "not-a-kernel", wantError: true},
		{release: "4", wantError: true},
		{release: "4.x", wantError: true},
	} {
		t.Run(test.release, func(t *testing.T) {
			compatibility, err := evaluateHostKernelCompatibility(test.release)
			if test.wantError {
				if err == nil {
					t.Fatalf("evaluateHostKernelCompatibility(%q) succeeded: %+v", test.release, compatibility)
				}
				return
			}
			if err != nil {
				t.Fatalf("evaluateHostKernelCompatibility(%q): %v", test.release, err)
			}
			if compatibility.version != test.version || compatibility.release != test.release || compatibility.disableHostGSO != test.disableHostGSO {
				t.Errorf("evaluateHostKernelCompatibility(%q)=%+v, want version=%+v disableHostGSO=%t", test.release, compatibility, test.version, test.disableHostGSO)
			}
		})
	}
}

func TestRuntimePlatformValidation(t *testing.T) {
	for _, test := range []struct {
		value string
		want  string
		ok    bool
	}{
		{value: "systrap", want: "systrap", ok: true},
		{value: "KVM", want: "kvm", ok: true},
		{value: "ptrace"},
		{value: ""},
	} {
		got, err := normalizeRuntimePlatform(test.value)
		if test.ok {
			if err != nil || got != test.want {
				t.Errorf("normalizeRuntimePlatform(%q)=(%q, %v), want %q", test.value, got, err, test.want)
			}
		} else if err == nil {
			t.Errorf("normalizeRuntimePlatform(%q)=%q, want error", test.value, got)
		}
	}
}

func TestDaemonKVMPreflightFailure(t *testing.T) {
	t.Setenv("TTC_ROOT", t.TempDir())
	original := daemonPlatformPreflight
	defer func() { daemonPlatformPreflight = original }()
	var gotPlatform string
	daemonPlatformPreflight = func(platform string) error {
		gotPlatform = platform
		return errors.New("test KVM unavailable")
	}
	var stdout, stderr bytes.Buffer
	status := RunDaemon([]string{"--platform=kvm", "--socket", filepath.Join(t.TempDir(), "ttcd.sock")}, &stdout, &stderr)
	if status != 1 || gotPlatform != "kvm" || !strings.Contains(stderr.String(), "test KVM unavailable") {
		t.Fatalf("KVM preflight: status=%d platform=%q stdout=%q stderr=%q", status, gotPlatform, stdout.String(), stderr.String())
	}
}

func TestDaemonRejectsUnsupportedRuntimePlatform(t *testing.T) {
	t.Setenv("TTC_ROOT", t.TempDir())
	var stdout, stderr bytes.Buffer
	status := RunDaemon([]string{"--platform=ptrace"}, &stdout, &stderr)
	if status != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "supported platforms are systrap and kvm") {
		t.Fatalf("unsupported platform: status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
}

func TestPrepareDaemonRuntimeStorageRejectsNFS(t *testing.T) {
	original := daemonStatfs
	t.Cleanup(func() { daemonStatfs = original })
	daemonStatfs = func(_ string, statfs *unix.Statfs_t) error {
		statfs.Type = unix.NFS_SUPER_MAGIC
		return nil
	}
	dir := t.TempDir()
	err := prepareDaemonRuntimeStorage(filepath.Join(dir, "runtime", "ttcd.sock"), filepath.Join(dir, "runsc"))
	if err == nil || !strings.Contains(err.Error(), "node-local") || !strings.Contains(err.Error(), "TTC_ROOT") {
		t.Fatalf("prepareDaemonRuntimeStorage on NFS: %v", err)
	}
}

func TestPrepareDaemonRuntimeStorageCreatesLocalPaths(t *testing.T) {
	original := daemonStatfs
	t.Cleanup(func() { daemonStatfs = original })
	daemonStatfs = func(_ string, statfs *unix.Statfs_t) error {
		statfs.Type = unix.TMPFS_MAGIC
		return nil
	}
	dir := t.TempDir()
	socketDir := filepath.Join(dir, "runtime")
	root := filepath.Join(dir, "runsc")
	if err := prepareDaemonRuntimeStorage(filepath.Join(socketDir, "ttcd.sock"), root); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{socketDir, root} {
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			t.Fatalf("runtime path %q: info=%v err=%v", path, info, err)
		}
	}
}

func TestDaemonRejectsUnsupportedHostKernel(t *testing.T) {
	t.Setenv("TTC_ROOT", t.TempDir())
	original := daemonKernelRelease
	defer func() {
		daemonKernelRelease = original
	}()
	daemonKernelRelease = func() (string, error) {
		return "3.16.99-legacy", nil
	}

	socket := filepath.Join(t.TempDir(), "runtime", "ttcd.sock")
	var stdout, stderr bytes.Buffer
	status := RunDaemon([]string{
		"--socket", socket,
		"--unsafe-no-host-isolation",
	}, &stdout, &stderr)
	if status != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "requires Linux 3.17 or newer") {
		t.Fatalf("unsupported kernel: status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatalf("unsupported kernel created daemon socket: %v", err)
	}
}

func TestDaemonWarnsWhenHostGSOMustBeDisabled(t *testing.T) {
	original := daemonKernelRelease
	originalProbe := createDaemonRootlessProbe
	originalEUID := daemonEffectiveUID
	defer func() {
		daemonKernelRelease = original
		createDaemonRootlessProbe = originalProbe
		daemonEffectiveUID = originalEUID
	}()
	daemonKernelRelease = func() (string, error) {
		return "4.14.76-legacy", nil
	}
	daemonEffectiveUID = func() int { return 1000 }
	createDaemonRootlessProbe = func() error { return errors.New("fixture stops startup after kernel warning") }

	dataRoot := t.TempDir()
	t.Setenv("TTC_ROOT", dataRoot)
	var stdout, stderr bytes.Buffer
	status := RunDaemon([]string{
		"--socket", filepath.Join(t.TempDir(), "runtime", "ttcd.sock"),
	}, &stdout, &stderr)
	if status != 1 || stdout.Len() != 0 {
		t.Fatalf("old kernel startup fixture: status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
	if diagnostic := stderr.String(); !strings.Contains(diagnostic, "disabling host GSO") || !strings.Contains(diagnostic, "large payloads") {
		t.Fatalf("old kernel startup diagnostic=%q", diagnostic)
	}
}

func TestClientRequiresExplicitDaemonStart(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "runtime", "ttcd.sock")
	var stdout, stderr bytes.Buffer
	status := Run([]string{"--daemon-socket", socket, "ps"}, strings.NewReader(""), &stdout, &stderr)
	if status != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "start it explicitly with 'ttc daemon start'") {
		t.Fatalf("missing daemon command: status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	status = Run([]string{"--daemon-socket", socket, "completion", "bash"}, strings.NewReader(""), &stdout, &stderr)
	if status != 0 || !strings.Contains(stdout.String(), "_ttc_completion") || stderr.Len() != 0 {
		t.Fatalf("offline completion: status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
}

func TestReportDaemonStartupFailureIncludesDaemonDiagnostic(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "ttcd.log")
	if err := os.WriteFile(logPath, []byte("ttcd: volume backing filesystem does not support FICLONE\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	reportDaemonStartupFailure(&stderr, logPath, errors.New("exit status 1"))
	got := stderr.String()
	if !strings.Contains(got, logPath) || !strings.Contains(got, "exit status 1") || !strings.Contains(got, "does not support FICLONE") {
		t.Fatalf("startup diagnostic=%q, want log path, process status, and daemon error", got)
	}
}

func TestReportDaemonStartupWarningsSurfacesHostGSOFallback(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "ttcd.log")
	log := "ttcd: warning: unrelated warning\n" +
		"ttcd: warning: host Linux kernel 4.14.76 predates 4.14.77; disabling host GSO for compatibility; network performance will be reduced\n" +
		"ttcd listening\n"
	if err := os.WriteFile(logPath, []byte(log), 0600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	reportDaemonStartupWarnings(&stderr, logPath)
	if diagnostic := stderr.String(); !strings.Contains(diagnostic, "disabling host GSO") || strings.Contains(diagnostic, "unrelated warning") {
		t.Fatalf("surfaced startup warnings=%q", diagnostic)
	}
}

func TestStopDaemonContainersTreatsLineageAsDisplayOnly(t *testing.T) {
	b := &fakeBackend{
		families: []Family{{
			ID: "web",
			Branches: []Branch{
				{ID: defaultBranchID, State: "running"},
				{ID: "child", ParentID: defaultBranchID, State: "paused"},
				{ID: "grandchild", ParentID: "child", State: "running"},
				{ID: "old", ParentID: defaultBranchID, State: "exited"},
			},
		}},
		inspectInfo: InspectInfo{TTC: TTCInspectInfo{Branches: []InspectBranch{
			{Metadata: Branch{ID: defaultBranchID, State: "running"}},
			{Metadata: Branch{ID: "child", ParentID: defaultBranchID, State: "paused"}},
			{Metadata: Branch{ID: "grandchild", ParentID: "child", State: "running"}},
			{Metadata: Branch{ID: "old", ParentID: defaultBranchID, State: "exited"}},
		}}},
	}
	if err := stopDaemonContainers(b); err != nil {
		t.Fatalf("stopDaemonContainers: %v", err)
	}
	if b.resumed.container != "" {
		t.Errorf("daemon stop resumed paused branch: %+v", b.resumed)
	}
	wantOrder := []selector{
		{container: "web", branch: "grandchild"},
		{container: "web", branch: defaultBranchID},
	}
	if len(b.signals) != len(wantOrder) {
		t.Fatalf("signals=%+v, want targets %+v", b.signals, wantOrder)
	}
	for i, want := range wantOrder {
		if b.signals[i].target != want {
			t.Errorf("signal[%d] target=%+v, want %+v", i, b.signals[i].target, want)
		}
	}
}

func TestMigrationSourceRuntimeUsesPhysicalIdentity(t *testing.T) {
	tx := cluster.Transaction{
		Identity:                cluster.ForkIdentity{SourceContainer: "family", SourceBranch: "test"},
		SourcePhysicalContainer: "fork-original", SourcePhysicalBranch: "test",
	}
	container, branch := migrationSourceRuntime(tx)
	if container != "fork-original" || branch != "test" {
		t.Fatalf("migration source runtime = %s@%s, want fork-original@test", container, branch)
	}
	tx.SourcePhysicalContainer, tx.SourcePhysicalBranch = "", ""
	container, branch = migrationSourceRuntime(tx)
	if container != "family" || branch != "test" {
		t.Fatalf("legacy migration source runtime = %s@%s, want family@test", container, branch)
	}
}

func TestStopDaemonContainersRemovesRuntimeWithoutFamilyMetadata(t *testing.T) {
	dir := t.TempDir()
	runsc := filepath.Join(dir, "runsc")
	script := `#!/bin/sh
if [ "$1" != "--root" ] || [ "$2" != "/runtime" ]; then
	echo "unexpected globals: $*" >&2
	exit 2
fi
case "$3" in
list)
	printf '[{"id":"orphan","pid":%s,"status":"running"}]\n' "$PPID"
	;;
timetravel)
	echo 'connecting to control server: connection refused' >&2
	exit 128
	;;
delete)
	if [ "$4" != "--force" ] || [ "$5" != "orphan" ]; then
		echo "unexpected delete: $*" >&2
		exit 2
	fi
	touch "$0.deleted"
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
	sharedRoot := filepath.Join(dir, "shared")
	localRoot := filepath.Join(dir, "local")
	orphanDir := filepath.Join(localRoot, "containers", "orphan")
	if err := os.MkdirAll(orphanDir, 0700); err != nil {
		t.Fatal(err)
	}
	b := &runtimeBackend{
		root:      "/runtime",
		runsc:     runsc,
		dataRoot:  sharedRoot,
		localRoot: localRoot,
	}
	if err := stopDaemonContainers(b); err != nil {
		t.Fatalf("stopDaemonContainers: %v", err)
	}
	if _, err := os.Stat(runsc + ".deleted"); err != nil {
		t.Errorf("orphan runtime was not force-deleted: %v", err)
	}
	if _, err := os.Stat(orphanDir); !os.IsNotExist(err) {
		t.Errorf("orphan local state still exists or could not be inspected: %v", err)
	}
}

func TestBranchOwnedByBackendRequiresExplicitBranchOwner(t *testing.T) {
	backend := &runtimeBackend{nodeID: "node-a"}
	if branchOwnedByBackend(backend, Family{NodeID: "node-b"}, Branch{ID: "main"}) {
		t.Fatal("branch with no explicit owner inherited a remote family owner")
	}
	if !branchOwnedByBackend(backend, Family{NodeID: "node-b"}, Branch{ID: "main", NodeID: "node-a"}) {
		t.Fatal("explicit branch owner did not override the family owner")
	}
	if branchOwnedByBackend(backend, Family{}, Branch{ID: "main"}) {
		t.Fatal("unowned clustered branch was treated as locally owned")
	}
}

func TestConfigureCommandBackendIncludesDistributedRuntime(t *testing.T) {
	source := &distributedSourceRuntime{}
	capabilities := &cluster.Capabilities{NodeID: "node-a"}
	server := &daemonServer{
		distributedSource:   source,
		clusterCapabilities: capabilities,
	}
	backend := &runtimeBackend{}
	server.configureCommandBackend(backend)
	if backend.distributedSource != source {
		t.Fatal("command backend omitted distributed source runtime")
	}
	if backend.nodeID != capabilities.NodeID {
		t.Fatalf("command backend node=%q, want %q", backend.nodeID, capabilities.NodeID)
	}
	if backend.consoleSidecars != server {
		t.Fatal("command backend omitted daemon sidecar manager")
	}
}

func TestClientCannotSelectDaemonIsolationMode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	status := Run([]string{"--unsafe-no-host-isolation", "completion", "bash"}, strings.NewReader(""), &stdout, &stderr)
	if status != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "ordinary commands inherit the running daemon's mode") {
		t.Fatalf("per-command mode: status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	status = Run([]string{"--unsafe-no-host-isolation", "daemon", "status"}, strings.NewReader(""), &stdout, &stderr)
	if status != 2 || !strings.Contains(stderr.String(), "valid with 'daemon start' only") {
		t.Fatalf("daemon status mode: status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
}

func TestRootlessDaemonStartupPreflight(t *testing.T) {
	t.Setenv("TTC_ROOT", t.TempDir())
	original := createDaemonRootlessProbe
	originalEUID := daemonEffectiveUID
	defer func() {
		createDaemonRootlessProbe = original
		daemonEffectiveUID = originalEUID
	}()
	daemonEffectiveUID = func() int { return 1000 }
	calls := 0
	createDaemonRootlessProbe = func() error {
		calls++
		return errors.New("permission denied")
	}

	if err := preflightDaemonIsolation(true); err != nil {
		t.Fatalf("unsafe preflight: %v", err)
	}
	if calls != 0 {
		t.Fatalf("unsafe mode ran %d rootless probes, want zero", calls)
	}

	socket := filepath.Join(t.TempDir(), "runtime", "ttcd.sock")
	var stdout, stderr bytes.Buffer
	status := RunDaemon([]string{"--socket", socket}, &stdout, &stderr)
	if status != 1 || !strings.Contains(stderr.String(), "rootless mode is unavailable") {
		t.Fatalf("ttcd preflight: status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
	if calls != 1 {
		t.Fatalf("ttcd startup ran %d rootless probes, want one", calls)
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatalf("failed preflight created daemon socket: %v", err)
	}
}

func TestDaemonRootsRequireExplicitFreshEnvironment(t *testing.T) {
	t.Setenv("TTC_ROOT", "")
	t.Setenv("TTC_LOCAL_ROOT", "")
	t.Setenv("TTC_DATA_ROOT", "")
	if _, err := daemonRootsFromEnvironment(false); err == nil || !strings.Contains(err.Error(), "TTC_ROOT") {
		t.Fatalf("missing TTC_ROOT error=%v", err)
	}
	t.Setenv("TTC_DATA_ROOT", "/legacy")
	if _, err := daemonRootsFromEnvironment(false); err == nil || !strings.Contains(err.Error(), "no longer supported") {
		t.Fatalf("legacy root error=%v", err)
	}
	t.Setenv("TTC_DATA_ROOT", "")
	t.Setenv("TTC_ROOT", "relative")
	if _, err := daemonRootsFromEnvironment(false); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative root error=%v", err)
	}
	shared := filepath.Join(t.TempDir(), "shared")
	t.Setenv("TTC_ROOT", shared)
	if roots, err := daemonRootsFromEnvironment(false); err != nil || roots.shared != shared || roots.local != shared || roots.runtime != filepath.Join(shared, "runtime", "runsc") {
		t.Fatalf("standalone roots=%+v err=%v", roots, err)
	}
	if _, err := daemonRootsFromEnvironment(true); err == nil || !strings.Contains(err.Error(), "TTC_LOCAL_ROOT") {
		t.Fatalf("missing cluster local root error=%v", err)
	}
	t.Setenv("TTC_LOCAL_ROOT", shared)
	if _, err := daemonRootsFromEnvironment(true); err == nil || !strings.Contains(err.Error(), "distinct") {
		t.Fatalf("aliased cluster roots error=%v", err)
	}
	local := filepath.Join(t.TempDir(), "local")
	t.Setenv("TTC_LOCAL_ROOT", local)
	if roots, err := daemonRootsFromEnvironment(true); err != nil || roots.shared != shared || roots.local != local || roots.runtime != filepath.Join(local, "runtime", "runsc") {
		t.Fatalf("cluster roots=%+v err=%v", roots, err)
	}
	if err := os.MkdirAll(shared, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "shared-alias")
	if err := os.Symlink(shared, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TTC_LOCAL_ROOT", alias)
	if _, err := daemonRootsFromEnvironment(true); err == nil || !strings.Contains(err.Error(), "distinct") {
		t.Fatalf("symlink-aliased cluster roots error=%v", err)
	}
}

func TestDaemonStorageRootRejectsOldLayoutAndLocksLocalRoot(t *testing.T) {
	old := t.TempDir()
	if err := os.WriteFile(filepath.Join(old, "containers"), []byte("legacy"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := prepareDaemonStorageRoot(old, daemonRootRoleStandaloneAuthority, "", ""); err == nil || !strings.Contains(err.Error(), "not a fresh TTC root") {
		t.Fatalf("old root error=%v", err)
	}
	fresh := filepath.Join(t.TempDir(), "fresh")
	if err := prepareDaemonStorageRoot(fresh, daemonRootRoleClusterLocal, "repo", "node-a"); err != nil {
		t.Fatal(err)
	}
	if err := prepareDaemonStorageRoot(fresh, daemonRootRoleClusterLocal, "repo", "node-a"); err != nil {
		t.Fatalf("reopening matching root: %v", err)
	}
	if err := prepareDaemonStorageRoot(fresh, daemonRootRoleClusterLocal, "repo", "node-b"); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched node marker error=%v", err)
	}
	markerData, err := os.ReadFile(filepath.Join(fresh, ".ttc-root-format"))
	if err != nil {
		t.Fatal(err)
	}
	var marker daemonRootMarker
	if err := json.Unmarshal(markerData, &marker); err != nil {
		t.Fatal(err)
	}
	want := daemonRootMarker{Version: daemonClusterRootFormatVersion, Schema: daemonRootSchema, Role: daemonRootRoleClusterLocal, RepositoryID: "repo", NodeID: "node-a"}
	if marker != want {
		t.Fatalf("root marker=%+v, want %+v", marker, want)
	}
	first, err := lockDaemonLocalRoot(fresh)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := lockDaemonLocalRoot(fresh); err == nil {
		second.Close()
		t.Fatal("local root was locked by two daemons")
	}
}

func TestFreshDaemonRootAllowsOnlyActiveStartupLog(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "ttcd.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	logInfo, err := logFile.Stat()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 {
		t.Fatalf("reading startup root: entries=%v err=%v", entries, err)
	}
	if !freshDaemonRootEntryAllowed(entries[0], logInfo) {
		t.Fatal("active startup log was not admitted")
	}
	other, err := os.CreateTemp(t.TempDir(), "other-log-")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	otherInfo, err := other.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if freshDaemonRootEntryAllowed(entries[0], otherInfo) {
		t.Fatal("stale startup log was admitted")
	}
}

func TestDaemonStorageRootRejectsOldMarkerWithoutMutation(t *testing.T) {
	for _, test := range []struct {
		name    string
		version int
		role    string
	}{
		{name: "standalone-v1", version: 1, role: daemonRootRoleStandaloneAuthority},
		{name: "cluster-v4", version: 4, role: daemonRootRoleClusterShared},
		{name: "future", version: daemonClusterRootFormatVersion + 1, role: daemonRootRoleClusterShared},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			markerPath := filepath.Join(root, ".ttc-root-format")
			original := []byte(fmt.Sprintf("{\"version\":%d,\"kind\":\"shared\"}\n", test.version))
			if err := os.WriteFile(markerPath, original, 0600); err != nil {
				t.Fatal(err)
			}
			err := inspectDaemonStorageRoot(root, test.role)
			if err == nil || !strings.Contains(err.Error(), "does not migrate existing roots") {
				t.Fatalf("inspect old root error=%v", err)
			}
			after, readErr := os.ReadFile(markerPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(after, original) {
				t.Fatalf("old marker changed from %q to %q", original, after)
			}
		})
	}
}

func TestDaemonStorageRootRejectsMalformedMarkerWithoutMutation(t *testing.T) {
	root := t.TempDir()
	markerPath := filepath.Join(root, ".ttc-root-format")
	original := []byte("{not-json\n")
	if err := os.WriteFile(markerPath, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := inspectDaemonStorageRoot(root, daemonRootRoleStandaloneAuthority); err == nil || !strings.Contains(err.Error(), "invalid root marker") {
		t.Fatalf("malformed root marker error=%v", err)
	}
	after, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatalf("malformed marker changed from %q to %q", original, after)
	}
}

func TestDaemonStorageRootRejectsPreviousSnapshotGraphSchema(t *testing.T) {
	root := t.TempDir()
	markerPath := filepath.Join(root, ".ttc-root-format")
	old := daemonRootMarker{
		Version: daemonClusterRootFormatVersion, Schema: "snapshot-graph-v1",
		Role: daemonRootRoleClusterShared, RepositoryID: "repo",
	}
	data, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := inspectDaemonStorageRoot(root, daemonRootRoleClusterShared); err == nil || !strings.Contains(err.Error(), "schema/role") {
		t.Fatalf("previous snapshot-graph schema error=%v", err)
	}
	after, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, data) {
		t.Fatalf("previous-schema marker was modified: got %q, want %q", after, data)
	}
}

func TestPrepareDaemonStorageRootConcurrentInitialization(t *testing.T) {
	root := filepath.Join(t.TempDir(), "shared")
	const initializers = 32
	errorsByInitializer := make([]error, initializers)
	start := make(chan struct{})
	var wait sync.WaitGroup
	for i := range errorsByInitializer {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			errorsByInitializer[index] = prepareDaemonStorageRoot(root, daemonRootRoleClusterShared, "repository", "")
		}(i)
	}
	close(start)
	wait.Wait()
	for i, err := range errorsByInitializer {
		if err != nil {
			t.Fatalf("initializer %d: %v", i, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(root, ".ttc-root-format"))
	if err != nil {
		t.Fatal(err)
	}
	var got daemonRootMarker
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	want, err := daemonRootMarkerFor(daemonRootRoleClusterShared, "repository", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("concurrent marker=%+v, want %+v", got, want)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != ".ttc-root-format" {
		t.Fatalf("concurrent initialization residue=%v", entries)
	}
}

func TestPrepareDaemonStorageRootConcurrentConflictDoesNotReplaceWinner(t *testing.T) {
	root := filepath.Join(t.TempDir(), "shared")
	if err := prepareDaemonStorageRoot(root, daemonRootRoleClusterShared, "winner", ""); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, ".ttc-root-format"))
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareDaemonStorageRoot(root, daemonRootRoleClusterShared, "loser", ""); err == nil || !strings.Contains(err.Error(), "does not match requested") {
		t.Fatalf("conflicting initialization error=%v", err)
	}
	after, err := os.ReadFile(filepath.Join(root, ".ttc-root-format"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("conflicting initializer changed marker from %q to %q", before, after)
	}
}

func TestDaemonStorageRootRejectsWrongRole(t *testing.T) {
	root := t.TempDir()
	if err := prepareDaemonStorageRoot(root, daemonRootRoleStandaloneAuthority, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := inspectDaemonStorageRoot(root, daemonRootRoleClusterShared); err == nil || !strings.Contains(err.Error(), "incompatible format") {
		t.Fatalf("wrong root role error=%v", err)
	}
}

func TestStrictSnapshotGraphRootRejectsLegacyArtifacts(t *testing.T) {
	root := t.TempDir()
	current := filepath.Join(root, "containers", "family", "memory", "application.mem")
	if err := os.MkdirAll(filepath.Dir(current), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(current, []byte("legacy"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateStrictSnapshotGraphRoot(root); err == nil || !strings.Contains(err.Error(), "removed legacy storage artifact") {
		t.Fatalf("strict validation error=%v", err)
	}
	if err := os.Remove(current); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".ttc-root-format"), []byte("current"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateStrictSnapshotGraphRoot(root); err != nil {
		t.Fatalf("strict current root: %v", err)
	}
}

func TestDaemonRejectsOldRootBeforePlatformProbe(t *testing.T) {
	root := t.TempDir()
	markerPath := filepath.Join(root, ".ttc-root-format")
	original := []byte("{\"version\":1,\"kind\":\"shared\"}\n")
	if err := os.WriteFile(markerPath, original, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TTC_ROOT", root)
	oldPreflight := daemonPlatformPreflight
	t.Cleanup(func() { daemonPlatformPreflight = oldPreflight })
	preflightCalls := 0
	daemonPlatformPreflight = func(string) error {
		preflightCalls++
		return nil
	}
	var stdout, stderr bytes.Buffer
	status := RunDaemon([]string{"--socket", filepath.Join(t.TempDir(), "ttcd.sock"), "--unsafe-no-host-isolation"}, &stdout, &stderr)
	if status != 1 || !strings.Contains(stderr.String(), "does not migrate existing roots") {
		t.Fatalf("old-root startup: status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
	if preflightCalls != 0 {
		t.Fatalf("old-root startup ran %d platform probes, want zero", preflightCalls)
	}
	after, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatalf("old marker changed from %q to %q", original, after)
	}
}

func TestDaemonOwnsAndReapsConsoleSidecars(t *testing.T) {
	command := exec.Command("sleep", "30")
	if err := command.Start(); err != nil {
		t.Fatalf("starting sidecar fixture: %v", err)
	}
	server := &daemonServer{}
	if err := server.OwnConsoleSidecar("console-lock", command); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("OwnConsoleSidecar: %v", err)
	}
	if got := server.ownedConsoleSidecars(); got != 1 {
		t.Fatalf("owned sidecars=%d, want 1", got)
	}
	if err := server.OwnConsoleSidecar("console-lock", exec.Command("true")); err == nil {
		t.Fatal("duplicate console sidecar ownership succeeded")
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatalf("stopping sidecar fixture: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for server.ownedConsoleSidecars() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := server.ownedConsoleSidecars(); got != 0 {
		t.Fatalf("owned sidecars after exit=%d, want 0", got)
	}
	if command.ProcessState == nil {
		t.Fatalf("sidecar was not reaped: state=%v", command.ProcessState)
	}
}

func TestDaemonQuiescesConsoleClientsBeforeArtifactRemoval(t *testing.T) {
	server := &daemonServer{}
	target := selector{container: "demo", branch: "main"}
	canceled := make(chan struct{})
	releaseClient, err := server.RegisterConsoleClient(target, func() { close(canceled) })
	if err != nil {
		t.Fatalf("RegisterConsoleClient: %v", err)
	}
	type quiesceResult struct {
		release func()
		err     error
	}
	result := make(chan quiesceResult, 1)
	go func() {
		release, err := server.QuiesceConsoleClients(selector{container: target.container})
		result <- quiesceResult{release: release, err: err}
	}()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("family quiescing did not cancel the active console client")
	}
	select {
	case got := <-result:
		if got.release != nil {
			got.release()
		}
		t.Fatalf("quiescing returned before the console reader closed: %v", got.err)
	case <-time.After(20 * time.Millisecond):
	}
	releaseClient()
	var got quiesceResult
	select {
	case got = <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("quiescing did not finish after the console reader closed")
	}
	if got.err != nil {
		t.Fatalf("QuiesceConsoleClients: %v", got.err)
	}
	if release, err := server.RegisterConsoleClient(target, func() {}); err == nil {
		release()
		t.Fatal("console reader registered while artifact removal was fenced")
	}
	got.release()
	release, err := server.RegisterConsoleClient(target, func() {})
	if err != nil {
		t.Fatalf("console reader remained fenced after removal: %v", err)
	}
	release()
}

func TestDaemonStopsOwnedPortSidecar(t *testing.T) {
	command := exec.Command("sleep", "30")
	if err := command.Start(); err != nil {
		t.Fatalf("starting port sidecar fixture: %v", err)
	}
	server := &daemonServer{}
	if err := server.OwnPortSidecar("port-lock", command); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("OwnPortSidecar: %v", err)
	}
	stopped, err := server.StopPortSidecar("port-lock")
	if err != nil || !stopped {
		t.Fatalf("StopPortSidecar=(%t, %v), want true, nil", stopped, err)
	}
	if command.ProcessState == nil {
		t.Fatal("port sidecar was not reaped")
	}
	if stopped, err := server.StopPortSidecar("missing"); err != nil || stopped {
		t.Fatalf("StopPortSidecar missing=(%t, %v), want false, nil", stopped, err)
	}
}

func TestDaemonCommandUsesStdin(t *testing.T) {
	for _, test := range []struct {
		args []string
		want bool
	}{
		{[]string{"attach", "-i", "demo"}, true},
		{[]string{"container", "attach", "-i", "demo"}, true},
		{[]string{"exec", "-dit", "demo@main", "sh"}, true},
		{[]string{"run", "--interactive", "alpine"}, true},
		{[]string{"start", "-ai", "demo"}, true},
		{[]string{"cp", "-", "demo@main:/tmp"}, true},
		{[]string{"cp", "local", "demo@main:/tmp"}, false},
		{[]string{"cp", "demo@main:/tmp", "-"}, false},
		{[]string{"load"}, true},
		{[]string{"load", "--platform", "linux/amd64"}, true},
		{[]string{"load", "-i", "/tmp/images.tar"}, false},
		{[]string{"image", "load"}, true},
		{[]string{"image", "load", "--input=/tmp/images.tar"}, false},
		{[]string{"exec", "ls", "demo"}, false},
		{[]string{"exec", "demo@main", "true"}, false},
		{[]string{"logs", "-f", "demo"}, false},
	} {
		if got := daemonCommandUsesStdin(test.args); got != test.want {
			t.Errorf("daemonCommandUsesStdin(%q)=%t, want %t", test.args, got, test.want)
		}
	}
}

func TestNormalizeDaemonImageArchivePaths(t *testing.T) {
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		args []string
		want []string
	}{
		{[]string{"load", "-i", "images.tar"}, []string{"load", "-i", filepath.Join(workingDirectory, "images.tar")}},
		{[]string{"image", "load", "--input=images.tar.gz"}, []string{"image", "load", "--input=" + filepath.Join(workingDirectory, "images.tar.gz")}},
		{[]string{"save", "-o=out.tar", "alpine"}, []string{"save", "-o=" + filepath.Join(workingDirectory, "out.tar"), "alpine"}},
		{[]string{"image", "save", "--output", "/tmp/out.tar", "alpine"}, []string{"image", "save", "--output", "/tmp/out.tar", "alpine"}},
		{[]string{"load"}, []string{"load"}},
		{[]string{"pull", "alpine"}, []string{"pull", "alpine"}},
	} {
		got, err := normalizeDaemonImageArchivePaths(test.args)
		if err != nil {
			t.Errorf("normalizeDaemonImageArchivePaths(%q): %v", test.args, err)
			continue
		}
		if !reflect.DeepEqual(got, test.want) {
			t.Errorf("normalizeDaemonImageArchivePaths(%q)=%q, want %q", test.args, got, test.want)
		}
	}
}

func TestDaemonCommandNeedsMutationLock(t *testing.T) {
	for _, test := range []struct {
		args []string
		want bool
	}{
		{[]string{"fork", "--check", "demo"}, true},
		{[]string{"pause", "demo"}, true},
		{[]string{"container", "restart", "demo"}, true},
		{[]string{"image", "rm", "alpine"}, true},
		{[]string{"image", "pull", "alpine"}, true},
		{[]string{"load"}, true},
		{[]string{"image", "load"}, true},
		{[]string{"save", "alpine"}, true},
		{[]string{"image", "save", "alpine"}, true},
		{[]string{"commit", "demo@main", "local/demo:latest"}, true},
		{[]string{"rm", "demo@child"}, true},
		{[]string{"port", "publish", "demo@main", "8080:80"}, true},
		{[]string{"port", "unpublish", "demo@main", "8080:80"}, true},
		{[]string{"port", "ls", "demo"}, false},
		{[]string{"run", "-d", "alpine"}, true},
		{[]string{"run", "--detach=false", "alpine"}, false},
		{[]string{"run", "alpine"}, false},
		{[]string{"start", "demo"}, true},
		{[]string{"start", "-ai", "demo"}, false},
		{[]string{"start", "--attach=false", "demo"}, true},
		{[]string{"cp", "local", "demo@main:/tmp"}, true},
		{[]string{"cp", "demo@main:/tmp", "local"}, true},
		{[]string{"checkpoint", "demo@main", "/tmp/demo.ttc"}, true},
		{[]string{"restore", "--name", "recovered", "/tmp/demo.ttc"}, true},
		{[]string{"exec", "-d", "demo@main", "true"}, false},
		{[]string{"logs", "-f", "demo@main"}, false},
		{[]string{"wait", "demo@main"}, false},
	} {
		if got := daemonCommandNeedsMutationLock(test.args); got != test.want {
			t.Errorf("daemonCommandNeedsMutationLock(%q)=%t, want %t", test.args, got, test.want)
		}
	}
}

func TestDaemonCommandNeedsMutationJournal(t *testing.T) {
	for _, test := range []struct {
		args []string
		want bool
	}{
		{[]string{"cp", "local", "demo@main:/tmp"}, true},
		{[]string{"cp", "-", "demo@main:/tmp"}, true},
		{[]string{"cp", "demo@main:/tmp", "local"}, false},
		{[]string{"cp", "demo@main:/tmp", "-"}, false},
		{[]string{"fork", "demo@main", "demo@child"}, true},
		{[]string{"checkpoint", "demo@main", "/tmp/demo.ttc"}, false},
		{[]string{"restore", "--name", "recovered", "/tmp/demo.ttc"}, true},
		{[]string{"pull", "alpine"}, false},
		{[]string{"image", "pull", "alpine"}, false},
		{[]string{"load"}, false},
		{[]string{"image", "load"}, false},
		{[]string{"save", "alpine"}, false},
		{[]string{"image", "save", "alpine"}, false},
		{[]string{"commit", "demo@main", "local/demo:latest"}, false},
		{[]string{"logs", "demo@main"}, false},
	} {
		if got := daemonCommandNeedsMutationJournal(test.args); got != test.want {
			t.Errorf("daemonCommandNeedsMutationJournal(%q)=%t, want %t", test.args, got, test.want)
		}
	}
}

func TestDaemonMutationLocksAreScopedByDataRoot(t *testing.T) {
	server := &daemonServer{}
	root := filepath.Join(t.TempDir(), "state")
	unlockFirst := server.lockMutation(root, []string{"pause", "demo"})

	secondAcquired := make(chan func(), 1)
	go func() {
		secondAcquired <- server.lockMutation(root, []string{"stop", "demo"})
	}()
	select {
	case unlock := <-secondAcquired:
		unlock()
		t.Fatal("conflicting mutation acquired the same data-root lock")
	case <-time.After(50 * time.Millisecond):
	}

	readAcquired := make(chan func(), 1)
	go func() {
		readAcquired <- server.lockMutation(root, []string{"inspect", "demo"})
	}()
	select {
	case unlock := <-readAcquired:
		unlock()
		t.Fatal("lifecycle read acquired the lock during a conflicting mutation")
	case <-time.After(50 * time.Millisecond):
	}
	unlockOther := server.lockMutation(root+"-other", []string{"stop", "demo"})
	unlockOther()

	unlockFirst()
	remaining := 2
	for remaining != 0 {
		select {
		case unlock := <-secondAcquired:
			secondAcquired = nil
			unlock()
			remaining--
		case unlock := <-readAcquired:
			readAcquired = nil
			unlock()
			remaining--
		case <-time.After(5 * time.Second):
			t.Fatal("lifecycle operation remained blocked after unlock")
		}
	}
}

func TestDaemonLifecycleReadsDoNotEnterMutationJournal(t *testing.T) {
	for _, args := range [][]string{{"ps", "-a"}, {"ls", "--json"}, {"inspect", "demo@main"}, {"container", "stats", "demo@main"}, {"run", "alpine"}, {"start", "-a", "demo@main"}} {
		if !daemonCommandReadsLifecycle(args) {
			t.Errorf("%v does not take the lifecycle lock", args)
		}
		if daemonCommandNeedsMutationJournal(args) {
			t.Errorf("%v unnecessarily enters the mutation journal", args)
		}
	}
	for _, args := range [][]string{{"run", "-d", "alpine"}, {"start", "demo@main"}, {"ps", "-a"}} {
		if got := daemonCommandReleasesLifecycleAfterStart(args); got {
			t.Errorf("%v unexpectedly releases an attached lifecycle lock", args)
		}
	}
}

func TestDaemonStdinLeaseRejectsAndTakesOver(t *testing.T) {
	server := &daemonServer{}
	cancelOld := make(chan struct{})
	releaseOld, err := server.acquireStdinLease("root\x00demo", false, func() { close(cancelOld) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.acquireStdinLease("root\x00demo", false, func() {}); err == nil || !strings.Contains(err.Error(), "--takeover") {
		t.Fatalf("second lease error=%v", err)
	}

	released := make(chan struct{})
	go func() {
		<-cancelOld
		releaseOld()
		close(released)
	}()
	releaseSecond, err := server.acquireStdinLease("root\x00demo", true, func() {})
	if err != nil {
		t.Fatalf("takeover: %v", err)
	}
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("old stdin owner was not detached")
	}
	releaseSecond()
}

func TestDaemonAttachTakeoverParsing(t *testing.T) {
	clean, key, takeover, err := daemonAttachStdinLease(options{}, []string{"attach", "-i", "--takeover", "--no-stdout"}, true)
	if err != nil || key != "" || !takeover || strings.Contains(strings.Join(clean, " "), "takeover") {
		t.Fatalf("takeover parse=(%q, %q, %t, %v)", clean, key, takeover, err)
	}
	if _, _, _, err := daemonAttachStdinLease(options{}, []string{"attach", "--takeover", "demo"}, false); err == nil {
		t.Fatal("takeover without interactive stdin was accepted")
	}
	clean, key, takeover, err = daemonAttachStdinLease(options{}, []string{"run", "image", "--takeover"}, false)
	if err != nil || key != "" || takeover || !strings.Contains(strings.Join(clean, " "), "--takeover") {
		t.Fatalf("non-attach args were modified: (%q, %q, %t, %v)", clean, key, takeover, err)
	}
}

func TestDaemonVersionedCommandTransport(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "runtime", "ttcd.sock")
	dataRoot := t.TempDir()
	done := make(chan error, 1)
	go func() {
		done <- serveDaemon(socket, "kvm", true, hostKernelCompatibility{release: "6.8.0"}, dataRoot, "/runtime", "/runsc", ttcvolume.Capability{Mode: ttcvolume.CopyModeFull}, io.Discard, io.Discard)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := connectDaemon(socket)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon did not become ready: %v", err)
		}
		time.Sleep(time.Millisecond)
	}

	exchange := func(request daemonRequest) (int, string, string) {
		t.Helper()
		conn, err := connectDaemon(socket)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		var stdout, stderr bytes.Buffer
		status, err := exchangeDaemon(conn, request, strings.NewReader(""), &stdout, &stderr)
		if err != nil {
			t.Fatalf("exchangeDaemon: %v", err)
		}
		return status, stdout.String(), stderr.String()
	}

	status, stdout, stderr := exchange(daemonRequest{
		Version: daemonAPIVersion,
		Action:  "command",
		Args:    []string{"completion", "bash"},
	})
	if status != 0 || !strings.Contains(stdout, "_ttc_completion()") || stderr != "" {
		t.Fatalf("completion response: status=%d stdout=%q stderr=%q", status, stdout, stderr)
	}

	status, stdout, stderr = exchange(daemonRequest{Version: daemonAPIVersion, Action: "ping"})
	if status != 0 ||
		!strings.Contains(stdout, `"build_id":"`+version.Version()+`"`) ||
		!strings.Contains(stdout, `"features":["batch-fork"`) ||
		!strings.Contains(stdout, `"filesystem-object-paging"`) ||
		strings.Contains(stdout, `"filesystem-range-paging"`) ||
		!strings.Contains(stdout, `"runtime_platform":"kvm"`) ||
		!strings.Contains(stdout, `"isolation_mode":"unsafe-no-host-isolation"`) ||
		!strings.Contains(stdout, `"unsafe_no_host_isolation":true`) ||
		!strings.Contains(stdout, `"host_kernel_release":"6.8.0"`) ||
		!strings.Contains(stdout, `"host_gso_enabled":true`) ||
		!strings.Contains(stdout, `"data_root":"`+dataRoot+`"`) ||
		!strings.Contains(stdout, `"volume_copy_mode":"full-copy"`) ||
		stderr != "" {
		t.Fatalf("daemon mode status: status=%d stdout=%q stderr=%q", status, stdout, stderr)
	}
	status, _, stderr = exchange(daemonRequest{Version: daemonAPIVersion, ClientBuildID: "different-build", Action: "ping"})
	if status == 0 || !strings.Contains(stderr, "does not match daemon build") {
		t.Fatalf("build mismatch response: status=%d stderr=%q", status, stderr)
	}
	requestID, err := newDaemonRequestID()
	if err != nil {
		t.Fatal(err)
	}
	mutation := daemonRequest{
		Version: daemonAPIVersion, Action: "command", RequestID: requestID,
		Args: []string{"image", "rm", "missing"},
	}
	status, firstStdout, stderr := exchange(mutation)
	if status == 0 || firstStdout != "" || stderr == "" {
		t.Fatalf("first mutation: status=%d stdout=%q stderr=%q", status, firstStdout, stderr)
	}
	firstStatus, firstStderr := status, stderr
	status, replayStdout, stderr := exchange(mutation)
	if status != firstStatus || replayStdout != firstStdout || stderr != firstStderr {
		t.Fatalf("replayed mutation: status=%d stdout=%q stderr=%q, want status=%d stdout=%q stderr=%q", status, replayStdout, stderr, firstStatus, firstStdout, firstStderr)
	}
	records, err := listDaemonMutations(dataRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("idempotent replay recorded %d mutations, want 1", len(records))
	}

	status, _, stderr = exchange(daemonRequest{Version: daemonAPIVersion + 1, Action: "ping"})
	if status != 1 || !strings.Contains(stderr, "unsupported API version") {
		t.Fatalf("version rejection: status=%d stderr=%q", status, stderr)
	}

	status, _, stderr = exchange(daemonRequest{Version: daemonAPIVersion + 100, ClientBuildID: "different-build", Action: "stop"})
	if status != 0 || stderr != "" {
		t.Fatalf("stop response: status=%d stderr=%q", status, stderr)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveDaemon: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop")
	}
}

func TestDefaultDaemonSocketHonorsTTCSocket(t *testing.T) {
	t.Setenv("TTC_SOCKET", "/var/run/ttc-test/daemon.sock")
	if got, want := defaultDaemonSocket(), "/var/run/ttc-test/daemon.sock"; got != want {
		t.Fatalf("defaultDaemonSocket() = %q, want %q", got, want)
	}
}

func TestExtractPlacementNode(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want []string
		node string
	}{
		{name: "run", args: []string{"run", "--node", "node-b", "--name", "demo", "alpine", "echo"}, want: []string{"run", "--name", "demo", "alpine", "echo"}, node: "node-b"},
		{name: "container start", args: []string{"container", "start", "--node=node-c", "demo@main"}, want: []string{"container", "start", "demo@main"}, node: "node-c"},
		{name: "restore", args: []string{"restore", "--name", "copy", "--node", "node-b", "/tmp/state.ttc"}, want: []string{"restore", "--name", "copy", "/tmp/state.ttc"}, node: "node-b"},
		{name: "guest argument", args: []string{"run", "alpine", "tool", "--node", "guest-value"}, want: []string{"run", "alpine", "tool", "--node", "guest-value"}},
		{name: "explicit separator", args: []string{"run", "--node", "node-b", "alpine", "--", "--node", "guest-value"}, want: []string{"run", "alpine", "--", "--node", "guest-value"}, node: "node-b"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, node, err := extractPlacementNode(test.args)
			if err != nil || node != test.node || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("extractPlacementNode(%q)=(%q, %q, %v), want (%q, %q, nil)", test.args, got, node, err, test.want, test.node)
			}
		})
	}
	if _, _, err := extractPlacementNode([]string{"start", "--node", "a", "--node", "b", "demo@main"}); err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("conflicting placement error=%v", err)
	}
}

func TestForkPlacementIsSelectedByForkCommand(t *testing.T) {
	if daemonCommandNeedsPlacementAdmission([]string{"fork", "--node", "node-b", "demo@main", "child"}) {
		t.Fatal("fork was pre-routed as placement admission; --node is its destination")
	}
	if !daemonCommandNeedsPlacementAdmission([]string{"rehydrate", "--node", "node-b", "demo@main"}) {
		t.Fatal("rehydrate must retain placement admission routing")
	}
}

func TestConsumeDaemonClusterOptions(t *testing.T) {
	args := []string{
		"--cluster-listen=127.0.0.1:7443",
		"--cluster-node-id", "node-a",
		"--cluster-repository-id", "repo",
		"--cluster-advertise", "node-a.example:0",
		"--cluster-memory-target", "8GiB",
		"--cluster-auto-rebalance",
		"--cluster-capacity-command", "/usr/bin/provision-ttc",
		"--cluster-lease", "30s",
		"--cluster-max-transactions", "17",
		"--cluster-page-listen", "0.0.0.0:0",
		"--cluster-page-advertise", "node-a.example:0",
		"--cluster-transfer-bytes", "131072",
		"--cluster-max-outstanding-bytes", "8388608",
		"--cluster-max-parallel", "8",
		"--cluster-ack-batch-pages", "128",
		"--cluster-ack-interval", "250us",
		"--cluster-prefetch", "adaptive",
		"--cluster-push", "eager-drain",
		"--cluster-peer", "node-b=10.0.0.2:7443",
	}
	opts := defaultDaemonClusterOptions()
	for len(args) != 0 {
		consumed, err := consumeDaemonClusterOption(args, &opts)
		if err != nil {
			t.Fatal(err)
		}
		if consumed == 0 {
			t.Fatalf("option was not consumed: %q", args[0])
		}
		args = args[consumed:]
	}
	if !opts.Configured || opts.Listen != "127.0.0.1:7443" || opts.NodeID != "node-a" ||
		opts.RepositoryID != "repo" || opts.Advertise != "node-a.example:0" ||
		opts.MemoryTarget != 8<<30 || !opts.AutoRebalance || opts.CapacityCommand != "/usr/bin/provision-ttc" || opts.LeaseDuration != 30*time.Second || opts.MaxTransactions != 17 || opts.PageListen != "0.0.0.0:0" ||
		opts.PageAdvertise != "node-a.example:0" || opts.PagingPolicy.TransferBytes != 128<<10 ||
		opts.PagingPolicy.MaxOutstandingBytes != 8<<20 || opts.PagingPolicy.MaxConcurrency != 8 ||
		opts.PagingPolicy.AckBatchPages != 128 || opts.PagingPolicy.AckInterval != 250*time.Microsecond ||
		opts.PagingPolicy.Prefetch != ttcpolicy.PrefetchAdaptive || !opts.PagingPolicy.Adaptive ||
		opts.PagingPolicy.Push != ttcpolicy.PushEagerDrain ||
		!reflect.DeepEqual(opts.Peers, []string{"node-b=10.0.0.2:7443"}) {
		t.Fatalf("cluster options=%+v", opts)
	}
	if node, url, name, err := parseClusterPeer(opts.Peers[0]); err != nil || node != "node-b" || url != "http://10.0.0.2:7443" || name != "" {
		t.Fatalf("parseClusterPeer=(%q, %q, %q, %v)", node, url, name, err)
	}
}

func TestDefaultClusterTransactionCapacitySupportsHighFanout(t *testing.T) {
	if got := defaultDaemonClusterOptions().MaxTransactions; got < 1024 {
		t.Fatalf("default max transactions=%d, want at least 1024 for batch remote fork", got)
	}
}

func TestSimpleClusterOptions(t *testing.T) {
	opts := defaultDaemonClusterOptions()
	args := []string{"--cluster-init", "--node", "node-a", "--listen", "127.0.0.1:7443", "--advertise", "node-a.example:7443", "--memory-target", "8GiB"}
	for len(args) != 0 {
		consumed, err := consumeDaemonClusterOption(args, &opts)
		if err != nil || consumed == 0 {
			t.Fatalf("consuming %q: consumed=%d err=%v", args[0], consumed, err)
		}
		args = args[consumed:]
	}
	roots := daemonRoots{shared: t.TempDir(), local: t.TempDir()}
	if err := resolveSimpleClusterOptions(&opts, roots); err != nil {
		t.Fatal(err)
	}
	if !opts.RaftBootstrap || opts.NodeID != "node-a" || opts.MemoryTarget != 8<<30 || opts.Listen != "127.0.0.1:7443" || opts.Advertise != "node-a.example:7443" || opts.RepositoryID == "" {
		t.Fatalf("simple cluster options=%+v", opts)
	}

	conflict := defaultDaemonClusterOptions()
	conflict.RaftBootstrap = true
	conflict.Peers = []string{"seed=node-a.example:7443"}
	if err := resolveSimpleClusterOptions(&conflict, roots); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("bootstrap/join conflict error=%v", err)
	}

	join := defaultDaemonClusterOptions()
	if consumed, err := consumeDaemonClusterOption([]string{"--cluster-join=node-a.example:7443"}, &join); err != nil || consumed != 1 {
		t.Fatalf("consuming equals-form join: consumed=%d err=%v", consumed, err)
	}
	if !reflect.DeepEqual(join.Peers, []string{"seed=node-a.example:7443"}) {
		t.Fatalf("equals-form join peers=%q", join.Peers)
	}
}

func TestControlOnlyClusterOptions(t *testing.T) {
	shared := t.TempDir()
	if err := prepareDaemonStorageRoot(shared, daemonRootRoleClusterShared, "repo-control", ""); err != nil {
		t.Fatal(err)
	}
	roots := daemonRoots{shared: shared, local: shared}
	opts := defaultDaemonClusterOptions()
	args := []string{"--control-only", "--cluster-join", "compute-a.example:7443", "--node", "login-gateway"}
	for len(args) != 0 {
		consumed, err := consumeDaemonClusterOption(args, &opts)
		if err != nil || consumed == 0 {
			t.Fatalf("consuming %q: consumed=%d err=%v", args[0], consumed, err)
		}
		args = args[consumed:]
	}
	if err := resolveSimpleClusterOptions(&opts, roots); err != nil {
		t.Fatal(err)
	}
	if !opts.ControlOnly || opts.NodeID != "login-gateway" || opts.RepositoryID != "repo-control" || opts.Listen != "" || opts.Advertise != "" || opts.PageListen != "" || opts.PageAdvertise != "" {
		t.Fatalf("control-only options=%+v", opts)
	}

	for _, test := range []struct {
		name string
		edit func(*daemonClusterOptions)
		want string
	}{
		{name: "bootstrap", edit: func(o *daemonClusterOptions) { o.RaftBootstrap = true }, want: "cluster-init"},
		{name: "listen", edit: func(o *daemonClusterOptions) { o.Listen = "0.0.0.0:7443" }, want: "outbound-only"},
		{name: "advertise", edit: func(o *daemonClusterOptions) { o.Advertise = "login.example:7443" }, want: "outbound-only"},
		{name: "capacity", edit: func(o *daemonClusterOptions) { o.MemoryTarget = 1 << 30 }, want: "capacity"},
	} {
		t.Run(test.name, func(t *testing.T) {
			bad := defaultDaemonClusterOptions()
			bad.ControlOnly, bad.Configured = true, true
			bad.Peers = []string{"seed=compute-a.example:7443"}
			bad.NodeID = "login-gateway"
			test.edit(&bad)
			if err := resolveSimpleClusterOptions(&bad, roots); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("resolve error=%v, want substring %q", err, test.want)
			}
		})
	}

	missingSeed := defaultDaemonClusterOptions()
	missingSeed.ControlOnly, missingSeed.Configured = true, true
	if err := resolveSimpleClusterOptions(&missingSeed, roots); err == nil || !strings.Contains(err.Error(), "cluster-join") {
		t.Fatalf("missing seed error=%v", err)
	}
}

func TestPrepareControlOnlyClusterProbesComputeSeed(t *testing.T) {
	var gotNode string
	seed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotNode = r.Header.Get("X-TTC-Node")
		if r.URL.Path != "/v1/daemon" {
			http.NotFound(w, r)
			return
		}
		var request daemonRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if request.Action != "ping" || request.ClientBuildID != version.Version() {
			http.Error(w, "unexpected probe", http.StatusBadRequest)
			return
		}
		payload, _ := json.Marshal(map[string]any{
			"build_id": version.Version(),
			"cluster": map[string]any{"capabilities": cluster.Capabilities{
				NodeID: "compute-a", RepositoryID: "repo-control",
			}},
		})
		var mu sync.Mutex
		_ = writeDaemonFrame(w, &mu, daemonFrameStdout, append(payload, '\n'))
		_ = writeDaemonStatus(w, &mu, 0)
	}))
	defer seed.Close()

	opts := defaultDaemonClusterOptions()
	opts.Configured, opts.ControlOnly = true, true
	opts.NodeID, opts.RepositoryID = "login-gateway", "repo-control"
	opts.Peers = []string{"seed=" + strings.TrimPrefix(seed.URL, "http://")}
	control, err := prepareControlOnlyCluster(opts, "/shared/ttc")
	if err != nil {
		t.Fatal(err)
	}
	if gotNode != "login-gateway" || len(control.clients) != 1 || control.repositoryID != "repo-control" {
		t.Fatalf("probe node=%q control=%+v", gotNode, control)
	}
	opts.RepositoryID = "another-repository"
	if _, err := prepareControlOnlyCluster(opts, "/shared/ttc"); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched repository error=%v", err)
	}
}

func TestPrepareDaemonClusterRejectsPartialConfiguration(t *testing.T) {
	_, err := prepareDaemonCluster(daemonClusterOptions{Configured: true, Listen: "127.0.0.1:0", MaxTransactions: 1}, t.TempDir(), t.TempDir(), "systrap")
	if err == nil || !strings.Contains(err.Error(), "requires") {
		t.Fatalf("partial cluster configuration error=%v", err)
	}
}

func TestValidateClusterCPUEnforcement(t *testing.T) {
	if err := validateClusterCPUEnforcement("systrap", false); err == nil || !strings.Contains(err.Error(), "CPUID faulting") {
		t.Fatalf("systrap without CPUID faulting: %v", err)
	}
	if err := validateClusterCPUEnforcement("systrap", true); err != nil {
		t.Fatalf("systrap with CPUID faulting: %v", err)
	}
	if err := validateClusterCPUEnforcement("kvm", false); err != nil {
		t.Fatalf("KVM without host CPUID faulting: %v", err)
	}
}

func TestImmutableClusterConfigCreatedAndAdopted(t *testing.T) {
	root := t.TempDir()
	bootstrap := defaultDaemonClusterOptions()
	bootstrap.PagingPolicy.MaxConcurrency = 7
	bootstrap.LeaseDuration = 23 * time.Second
	bootstrap.MaxTransactions = 41
	bootstrap.AutoRebalance = true
	bootstrap.ResolvedCPU = daemonCPUProfile{Version: daemonCPUProfileVersion, Mode: "portable", Features: []string{"sse", "sse2"}}
	if pending, err := applyImmutableClusterConfig(root, true, false, &bootstrap); err != nil || pending != nil {
		t.Fatal(err)
	}
	join := defaultDaemonClusterOptions()
	if pending, err := applyImmutableClusterConfig(root, false, false, &join); err != nil || pending != nil {
		t.Fatal(err)
	}
	if join.PagingPolicy != bootstrap.PagingPolicy || join.LeaseDuration != bootstrap.LeaseDuration || join.MaxTransactions != bootstrap.MaxTransactions || join.AutoRebalance != bootstrap.AutoRebalance {
		t.Fatalf("joining policy=%+v, want bootstrap policy=%+v", join, bootstrap)
	}

	mismatch := defaultDaemonClusterOptions()
	pending, err := applyImmutableClusterConfig(root, true, true, &mismatch)
	if err != nil || pending == nil {
		t.Fatalf("bootstrap policy change=(%+v, %v), want pending change", pending, err)
	}
}

func TestStandaloneCPUProfileIsImmutable(t *testing.T) {
	root := t.TempDir()
	profile, err := applyStandaloneCPUProfile(root, "portable", "", "systrap", false)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Mode != "portable" || len(profile.Features) == 0 {
		t.Fatalf("profile=%+v", profile)
	}
	for _, feature := range profile.Features {
		if strings.HasPrefix(feature, "avx512") || feature == "fsgsbase" {
			t.Fatalf("portable systrap profile exposed %q", feature)
		}
	}
	if _, err := applyStandaloneCPUProfile(root, "host", "", "systrap", true); err == nil || !strings.Contains(err.Error(), "differs") {
		t.Fatalf("changed immutable CPU profile error=%v", err)
	}
	adopted, err := applyStandaloneCPUProfile(root, "host", "", "systrap", false)
	if err != nil || !reflect.DeepEqual(adopted, profile) {
		t.Fatalf("implicit restart profile=(%+v, %v), want %+v", adopted, err, profile)
	}
}

func TestPortableCPUProfileIsHostIndependent(t *testing.T) {
	profile, err := resolveDaemonCPUProfile("portable", "", "systrap")
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOARCH == "amd64" && !slices.Equal(profile.Features, portableAMD64Features) {
		t.Fatalf("portable features=%v, want fixed baseline %v", profile.Features, portableAMD64Features)
	}
	for _, feature := range profile.Features {
		if strings.HasPrefix(feature, "avx") || strings.HasPrefix(feature, "amx_") || strings.Contains(feature, "xsave") {
			t.Fatalf("portable profile exposed optional extended-state feature %q", feature)
		}
	}
}

func TestExplicitCPUProfileXsaveABI(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("x86 XSAVE contract")
	}
	if _, err := resolveDaemonCPUProfile("explicit", "fpu,sse,sse2", "systrap"); err != nil {
		t.Fatalf("FXSAVE profile rejected: %v", err)
	}
	profile, err := resolveDaemonCPUProfile("explicit", "fpu,sse,sse2,xsave,osxsave", "systrap")
	if err != nil {
		t.Fatalf("complete XSAVE contract rejected: %v", err)
	}
	if profile.XsaveABI == "" {
		t.Fatal("XSAVE profile has no physical ABI")
	}
	if err := validateDaemonCPUProfile(profile, "systrap"); err != nil {
		t.Fatalf("local XSAVE ABI rejected: %v", err)
	}
	profile.XsaveABI += ";different-node"
	if err := validateDaemonCPUProfile(profile, "systrap"); err == nil || !strings.Contains(err.Error(), "XSAVE ABI differs") {
		t.Fatalf("incompatible native XSAVE ABI error=%v", err)
	}
}

func TestImmutableClusterConfigRequiresBootstrap(t *testing.T) {
	opts := defaultDaemonClusterOptions()
	if _, err := applyImmutableClusterConfig(t.TempDir(), false, false, &opts); err == nil || !strings.Contains(err.Error(), "fresh first node") {
		t.Fatalf("missing config error=%v", err)
	}
}

func TestClusterPageAddressesRequireEphemeralPorts(t *testing.T) {
	for _, address := range []string{"127.0.0.1:0", "[::1]:0", "node-a.example:0"} {
		if err := requireEphemeralClusterPagePort("--cluster-page-listen", address); err != nil {
			t.Errorf("ephemeral address %q rejected: %v", address, err)
		}
	}
	if err := requireEphemeralClusterPagePort("--cluster-page-listen", "127.0.0.1:7444"); err == nil || !strings.Contains(err.Error(), "port 0") {
		t.Fatalf("fixed cluster page port error=%v, want port 0 guidance", err)
	}
}

func TestRaftClusterIdentityPreventsSecondBootstrap(t *testing.T) {
	shared := t.TempDir()
	localA := filepath.Join(t.TempDir(), "raft")
	clusterID, err := prepareClusterRaftIdentity(shared, localA, "repository", "node-a", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if clusterID == "" {
		t.Fatal("bootstrap produced an empty cluster ID")
	}
	// Retrying startup with the same local root is safe even if Raft failed
	// before creating its WAL.
	if retry, err := prepareClusterRaftIdentity(shared, localA, "repository", "node-a", true, false); err != nil || retry != clusterID {
		t.Fatalf("same-root bootstrap retry=(%q, %v), want %q", retry, err, clusterID)
	}
	if _, err := prepareClusterRaftIdentity(shared, filepath.Join(t.TempDir(), "raft"), "repository", "node-b", true, false); err == nil || !strings.Contains(err.Error(), "second Raft group") {
		t.Fatalf("second bootstrap error=%v", err)
	}
	if joined, err := prepareClusterRaftIdentity(shared, filepath.Join(t.TempDir(), "raft"), "repository", "node-b", false, false); err != nil || joined != clusterID {
		t.Fatalf("join identity=(%q, %v), want %q", joined, err, clusterID)
	}
	if _, err := prepareClusterRaftIdentity(shared, filepath.Join(t.TempDir(), "raft"), "another-repository", "node-c", false, false); err == nil {
		t.Fatal("mismatched repository joined Raft cluster")
	}
}

func TestAdaptivePrefetchControllerHysteresis(t *testing.T) {
	policy := ttcpolicy.DefaultPolicy()
	policy.MaxConcurrency = 4
	policy.MaxOutstandingBytes = 8 << 20
	controller := newAdaptivePrefetchController(policy)
	now := time.Unix(1000, 0)
	status := pgalloc.AsyncPagesFileLoadStats{PrefetchBytes: policy.TransferBytes - policy.PageSize}
	if _, _, changed := controller.observe(status, now); changed {
		t.Fatal("initial observation changed the prefetch window")
	}
	status.BytesLoaded = policy.TransferBytes
	status.BytesWaited = policy.PageSize
	status.TotalWaiters = 1
	status.Faults = 4
	status.SequentialFaults = 3
	behind, ahead, changed := controller.observe(status, now.Add(600*time.Millisecond))
	if !changed || behind != 0 || ahead != controller.maxAhead {
		t.Fatalf("blocking-fault update=(%d,%d,%t)", behind, ahead, changed)
	}
	grownAhead := ahead
	for i := 0; i < 8; i++ {
		status.BytesLoaded += 4 * (grownAhead + policy.PageSize)
		behind, ahead, changed = controller.observe(status, now.Add(3*time.Second+time.Duration(i)*250*time.Millisecond))
	}
	wantShrunk := (grownAhead+policy.PageSize)/2 - policy.PageSize
	if !changed || behind != 0 || ahead != wantShrunk {
		t.Fatalf("quiet-throughput update=(%d,%d,%t)", behind, ahead, changed)
	}
}

func TestAdaptivePrefetchControllerShrinksRandomFaults(t *testing.T) {
	policy := ttcpolicy.DefaultPolicy()
	policy.MaxConcurrency = 4
	controller := newAdaptivePrefetchController(policy)
	now := time.Unix(2000, 0)
	status := pgalloc.AsyncPagesFileLoadStats{PrefetchBytes: policy.TransferBytes - policy.PageSize}
	if _, _, changed := controller.observe(status, now); changed {
		t.Fatal("initial observation changed the prefetch window")
	}
	status.BytesLoaded = 8 * policy.TransferBytes
	status.BytesWaited = 8 * policy.PageSize
	status.TotalWaiters = 8
	status.Faults = 8
	behind, ahead, changed := controller.observe(status, now.Add(300*time.Millisecond))
	if !changed || behind != 0 || ahead != policy.TransferBytes/2-policy.PageSize {
		t.Fatalf("random-fault update=(%d,%d,%t)", behind, ahead, changed)
	}
}

func TestAdaptivePrefetchControllerAccumulatesLocalitySamples(t *testing.T) {
	policy := ttcpolicy.DefaultPolicy()
	policy.MaxConcurrency = 4
	controller := newAdaptivePrefetchController(policy)
	now := time.Unix(3000, 0)
	status := pgalloc.AsyncPagesFileLoadStats{PrefetchBytes: policy.TransferBytes - policy.PageSize}
	controller.observe(status, now)
	status.BytesWaited = 2 * policy.PageSize
	status.TotalWaiters = 2
	status.Faults = 2
	status.SequentialFaults = 2
	if _, _, changed := controller.observe(status, now.Add(60*time.Millisecond)); changed {
		t.Fatal("two locality samples changed the window")
	}
	status.BytesWaited += 2 * policy.PageSize
	status.TotalWaiters += 2
	status.Faults += 2
	status.SequentialFaults += 2
	behind, ahead, changed := controller.observe(status, now.Add(120*time.Millisecond))
	if !changed || behind != 0 || ahead != controller.maxAhead {
		t.Fatalf("accumulated sequential update=(%d,%d,%t)", behind, ahead, changed)
	}
}
