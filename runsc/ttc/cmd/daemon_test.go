// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/cpuid"
	"gvisor.dev/gvisor/pkg/cputopology"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	ttcpolicy "gvisor.dev/gvisor/pkg/ttcremote"
	"gvisor.dev/gvisor/pkg/ttcvolume"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
	"gvisor.dev/gvisor/runsc/ttc/cluster"
	"gvisor.dev/gvisor/runsc/ttc/protocol"
	ttcstorage "gvisor.dev/gvisor/runsc/ttc/storage"
	"gvisor.dev/gvisor/runsc/version"
)

// shortDaemonTestRoot returns a new daemon root short enough for the paths of
// the daemon's Unix sockets.
func shortDaemonTestRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ttcr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func shortDaemonTestSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ttc-daemon-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "ttcd.sock")
}

type transactionLoadCatalog struct {
	cluster.TransactionCatalog
	load func(string) (*cluster.Transaction, error)
}

type failFirstRoundTripper struct {
	remaining int
	next      http.RoundTripper
}

func (r *failFirstRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if r.remaining > 0 {
		r.remaining--
		return nil, errors.New("seed listener is not ready")
	}
	return r.next.RoundTrip(request)
}

func (c transactionLoadCatalog) Load(id string) (*cluster.Transaction, error) {
	return c.load(id)
}

type transactionListCatalog struct {
	cluster.TransactionCatalog
	records []cluster.Transaction
}

func TestMigrationSourceIncarnationFenced(t *testing.T) {
	now := time.Unix(1000, 0)
	tx := cluster.Transaction{
		Identity:          cluster.ForkIdentity{SourceNodeID: "source"},
		SourceIncarnation: "old",
	}
	tests := []struct {
		name  string
		state *cluster.AuthorityState
		want  bool
	}{
		{name: "missing state", state: nil},
		{name: "missing node", state: cluster.NewAuthorityState()},
		{name: "gracefully retired", state: func() *cluster.AuthorityState {
			state := cluster.NewAuthorityState()
			state.RetiredNodes["source"] = "old"
			return state
		}(), want: true},
		{name: "replacement incarnation", state: func() *cluster.AuthorityState {
			state := cluster.NewAuthorityState()
			state.Nodes["source"] = cluster.NodeRecord{NodeID: "source", Incarnation: "new", State: cluster.NodeReady, LeaseExpiresAt: now.Add(time.Minute)}
			return state
		}(), want: true},
		{name: "exact incarnation live", state: func() *cluster.AuthorityState {
			state := cluster.NewAuthorityState()
			state.Nodes["source"] = cluster.NodeRecord{NodeID: "source", Incarnation: "old", State: cluster.NodeReady, LeaseExpiresAt: now.Add(time.Minute)}
			return state
		}()},
		{name: "exact incarnation expired", state: func() *cluster.AuthorityState {
			state := cluster.NewAuthorityState()
			state.Nodes["source"] = cluster.NodeRecord{NodeID: "source", Incarnation: "old", State: cluster.NodeReady, LeaseExpiresAt: now.Add(-time.Second)}
			return state
		}(), want: true},
		{name: "exact incarnation offline", state: func() *cluster.AuthorityState {
			state := cluster.NewAuthorityState()
			state.Nodes["source"] = cluster.NodeRecord{NodeID: "source", Incarnation: "old", State: cluster.NodeOffline, LeaseExpiresAt: now.Add(time.Minute)}
			return state
		}(), want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := migrationSourceIncarnationFenced(test.state, tx, now); got != test.want {
				t.Fatalf("migrationSourceIncarnationFenced() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestCanceledForkOwnsPlacement(t *testing.T) {
	tx := cluster.Transaction{Identity: cluster.ForkIdentity{
		SourceNodeID: "node-a", TargetNodeID: "node-b",
		SourceContainer: "family", SourceBranch: "main",
		TargetContainer: "family", TargetVisibleBranch: "child",
	}}
	placement := cluster.PlacementRecord{
		FamilyID: "family", BranchID: "child", OwnerNode: "node-b",
		PhysicalContainer: "fork-runtime", PhysicalBranch: "private-branch",
	}
	if !canceledForkOwnsPlacement(tx, placement, "node-b") {
		t.Fatal("ordinary fork did not own its authoritative target placement")
	}
	for name, mutate := range map[string]func(*cluster.Transaction, *cluster.PlacementRecord){
		"migration":       func(tx *cluster.Transaction, _ *cluster.PlacementRecord) { tx.Identity.Migration = true },
		"family":          func(_ *cluster.Transaction, p *cluster.PlacementRecord) { p.FamilyID = "other" },
		"branch":          func(_ *cluster.Transaction, p *cluster.PlacementRecord) { p.BranchID = "other" },
		"target node":     func(tx *cluster.Transaction, _ *cluster.PlacementRecord) { tx.Identity.TargetNodeID = "node-c" },
		"placement owner": func(_ *cluster.Transaction, p *cluster.PlacementRecord) { p.OwnerNode = "node-c" },
	} {
		t.Run(name, func(t *testing.T) {
			changedTx, changedPlacement := tx, placement
			mutate(&changedTx, &changedPlacement)
			if canceledForkOwnsPlacement(changedTx, changedPlacement, "node-b") {
				t.Fatal("mismatched transaction retained placement authority")
			}
		})
	}
	renamedTx, renamedPlacement := tx, placement
	renamedTx.Identity.TransactionID = "fork-rename"
	renamedPlacement.BranchID = "renamed-child"
	renamedPlacement.LastOperationID = "fork-rename"
	if !canceledForkOwnsPlacement(renamedTx, renamedPlacement, "node-b") {
		t.Fatal("operation-fenced renamed target placement lost cancellation ownership")
	}
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
	got, found, err = reusablePhysicalTarget(records, "family", "target-node")
	if err != nil || !found || got != "second-runtime" {
		t.Fatalf("reusablePhysicalTarget split tie=%q,%t,%v, want second-runtime,true,nil", got, found, err)
	}
	records = append(records,
		cluster.PlacementRecord{FamilyID: "family", BranchID: "split-2", OwnerNode: "target-node", PhysicalContainer: "second-runtime", State: cluster.PlacementPaused},
		cluster.PlacementRecord{FamilyID: "family", BranchID: "canonical", OwnerNode: "target-node", PhysicalContainer: "family", State: cluster.PlacementRunning},
	)
	got, found, err = reusablePhysicalTarget(records, "family", "target-node")
	if err != nil || !found || got != "family" {
		t.Fatalf("reusablePhysicalTarget canonical=%q,%t,%v, want family,true,nil", got, found, err)
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

func TestOpenDistributedSharedArtifactRetriesVisibilityErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "artifact")
	contents := []byte("published")
	if err := os.WriteFile(path, contents, 0600); err != nil {
		t.Fatal(err)
	}
	wantBytes := uint64(len(contents))
	attempts := 0
	file, err := openDistributedSharedArtifactWith(context.Background(), path, &wantBytes, func(path string) (*os.File, error) {
		attempts++
		switch attempts {
		case 1:
			return nil, os.ErrNotExist
		case 2:
			return nil, unix.ESTALE
		default:
			return os.Open(path)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	if attempts != 3 {
		t.Fatalf("open attempts=%d, want 3", attempts)
	}
}

func TestOpenDistributedSharedArtifactDoesNotHideProtocolErrors(t *testing.T) {
	permanent := errors.New("permission denied by protocol")
	attempts := 0
	_, err := openDistributedSharedArtifactWith(context.Background(), "unused", nil, func(string) (*os.File, error) {
		attempts++
		return nil, permanent
	})
	if !errors.Is(err, permanent) {
		t.Fatalf("open error=%v, want %v", err, permanent)
	}
	if attempts != 1 {
		t.Fatalf("open attempts=%d, want 1", attempts)
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

// A removal of a remote fork takes its target's branch and cancels its
// transaction, in either order: a drain of the fork's source, as a
// dehydration of the source makes, waits out the gap between the two and goes
// on once the transaction is canceled, its source session canceled too.
func TestSourceDrainSettlesAForkRemovedMeanwhile(t *testing.T) {
	const id = "fork-0123456789abcdef0123456789abcdef"
	var statuses atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/forks/"+id+"/drain":
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": `distributed fork transaction "` + id + `" target branch is unavailable`})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/forks/"+id:
			tx := cluster.Transaction{Identity: cluster.ForkIdentity{TransactionID: id, SourceNodeID: "node-a", TargetNodeID: "node-b", SourceContainer: "web", SourceBranch: "main", TargetContainer: id}, State: cluster.TransactionPublished, Revision: 6}
			if statuses.Add(1) >= 3 {
				tx.State, tx.Revision = cluster.TransactionCanceled, 7
			}
			_ = json.NewEncoder(w).Encode(&tx)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	calls := filepath.Join(t.TempDir(), "calls")
	runsc := filepath.Join(t.TempDir(), "runsc")
	if err := os.WriteFile(runsc, []byte("#!/bin/sh\necho \"$@\" >> "+calls+"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	backend := &runtimeBackend{root: t.TempDir(), runsc: runsc, stdin: strings.NewReader(""), stdout: io.Discard, stderr: io.Discard}
	source := &distributedSourceRuntime{peers: map[string]*cluster.Client{"node-b": {BaseURL: server.URL, HTTP: server.Client()}}}
	tx := cluster.Transaction{
		Identity: cluster.ForkIdentity{TransactionID: id, SourceNodeID: "node-a", TargetNodeID: "node-b", SourceContainer: "web", SourceBranch: "main", TargetContainer: id},
		State:    cluster.TransactionPublished, Revision: 6, SourceNeeded: true,
	}
	if err := drainSourceDependency(backend, source, nil, tx); err != nil {
		t.Fatalf("drainSourceDependency: %v", err)
	}
	data, err := os.ReadFile(calls)
	if err != nil || !strings.Contains(string(data), "timetravel web distributed-cancel "+id) {
		t.Fatalf("the source's session was not canceled: %q, %v", data, err)
	}
}

func TestDistributedTargetGone(t *testing.T) {
	for _, test := range []struct {
		err  error
		want bool
	}{
		{err: errors.New(`cluster peer: distributed fork transaction "fork-1" target branch is unavailable`), want: true},
		{err: errors.New("cluster peer: loading container: file does not exist"), want: true},
		{err: errors.New("distributed memory drain failed: remote page server: I/O error")},
		{err: errors.New("cluster peer: distributed memory drain is still in progress")},
	} {
		if got := distributedTargetGone(test.err); got != test.want {
			t.Errorf("distributedTargetGone(%q)=%t, want %t", test.err, got, test.want)
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
		noCPUIDFaulting:       true,
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
	if !opts.disableSyscallPatching {
		t.Error("a cluster without CPUID faulting kept syscall patching")
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
	tests := []struct {
		value string
		want  string
		ok    bool
	}{
		{value: defaultRuntimePlatform, want: defaultRuntimePlatform, ok: true},
		{value: "ptrace"},
		{value: ""},
	}
	if testLinuxHost {
		tests = append(tests, struct {
			value string
			want  string
			ok    bool
		}{value: "KVM", want: "kvm", ok: true})
	}
	for _, test := range tests {
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
	if !testLinuxHost {
		t.Skip("KVM preflight is Linux-specific")
	}
	t.Setenv("TTC_ROOT", shortDaemonTestRoot(t))
	original := daemonPlatformPreflight
	defer func() { daemonPlatformPreflight = original }()
	var gotPlatform string
	daemonPlatformPreflight = func(platform string) error {
		gotPlatform = platform
		return errors.New("test KVM unavailable")
	}
	var stdout, stderr bytes.Buffer
	status := RunDaemon([]string{"--platform=kvm"}, &stdout, &stderr)
	if status != 1 || gotPlatform != "kvm" || !strings.Contains(stderr.String(), "test KVM unavailable") {
		t.Fatalf("KVM preflight: status=%d platform=%q stdout=%q stderr=%q", status, gotPlatform, stdout.String(), stderr.String())
	}
}

func TestDaemonRejectsUnsupportedRuntimePlatform(t *testing.T) {
	t.Setenv("TTC_ROOT", shortDaemonTestRoot(t))
	var stdout, stderr bytes.Buffer
	status := RunDaemon([]string{"--platform=ptrace"}, &stdout, &stderr)
	if status != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "unsupported runtime platform") {
		t.Fatalf("unsupported platform: status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
}

// A graph handoff that found an inherited load not yet durable is reinstalled
// until it succeeds, but not more often than once an interval: each attempt
// starts a relay. Any other failure is final.
func TestGraphHandoffAwaitingAnInheritedLoadIsReinstalled(t *testing.T) {
	if !graphHandoffAwaitsInheritedLoad("inherited distributed memory load is not durable yet") {
		t.Error("a handoff awaiting an inherited load is not retried")
	}
	if graphHandoffAwaitsInheritedLoad("inherited distributed memory load failed before graph handoff: EIO") {
		t.Error("a handoff whose inherited load failed is retried")
	}
	r := &daemonTargetRuntime{backend: &runtimeBackend{dataRoot: t.TempDir()}}
	tx := &cluster.Transaction{
		Identity:     cluster.ForkIdentity{TransactionID: "fork-1", TargetContainer: "family"},
		GraphHandoff: &cluster.GraphHandoffDescriptor{GraphSnapshot: 1},
	}
	// The target is missing, so an attempt fails.
	if err := r.reinstallGraphHandoffEventually(context.Background(), tx); err == nil {
		t.Fatal("reinstalling the handoff of a missing target succeeded")
	}
	if err := r.reinstallGraphHandoffEventually(context.Background(), tx); err != nil {
		t.Fatalf("a handoff was reinstalled again within the interval: %v", err)
	}
	r.handoffReinstalls.Store(tx.Identity.TransactionID, time.Now().Add(-graphHandoffReinstallInterval))
	if err := r.reinstallGraphHandoffEventually(context.Background(), tx); err == nil {
		t.Fatal("a handoff was not reinstalled once the interval passed")
	}
}

func TestDaemonRootMustBeOnALocalFilesystem(t *testing.T) {
	original := daemonStatfs
	t.Cleanup(func() { daemonStatfs = original })
	root := filepath.Join(t.TempDir(), "not", "yet", "made")
	for _, test := range []struct {
		name    string
		statfs  func(*unix.Statfs_t)
		allow   bool
		wantErr string
		wantFS  bool
	}{
		{name: "local", statfs: setTestLocalStatfs},
		{name: "NFS", statfs: setTestNFSStatfs, wantErr: "network filesystem"},
		{name: "Lustre", statfs: setTestLustreStatfs, wantErr: "network filesystem"},
		{name: "allowed NFS", statfs: setTestNFSStatfs, allow: true, wantFS: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var checked []string
			daemonStatfs = func(path string, statfs *unix.Statfs_t) error {
				checked = append(checked, path)
				if _, err := os.Stat(path); err != nil {
					return unix.ENOENT
				}
				*statfs = unix.Statfs_t{}
				test.statfs(statfs)
				return nil
			}
			name, err := checkDaemonRootFilesystem("TTC_ROOT", root, test.allow)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) || !strings.Contains(err.Error(), "--allow-network-filesystem-root") {
					t.Fatalf("checkDaemonRootFilesystem = %q, %v; want an error containing %q", name, err, test.wantErr)
				}
				return
			}
			if err != nil || (name != "") != test.wantFS {
				t.Fatalf("checkDaemonRootFilesystem = %q, %v", name, err)
			}
			// A root not yet made is on the filesystem of its nearest ancestor.
			if last := checked[len(checked)-1]; last != filepath.Dir(filepath.Dir(filepath.Dir(root))) {
				t.Errorf("checked %q last, want the root's nearest existing ancestor", checked)
			}
		})
	}
}

func TestDaemonRootMustFitUnixSocketPaths(t *testing.T) {
	short := newDaemonLayout("/ttc")
	if err := checkDaemonSocketPaths(short); err != nil {
		t.Fatalf("checkDaemonSocketPaths(%q): %v", short.root, err)
	}
	long := newDaemonLayout("/" + strings.Repeat("r", daemonUnixPathMax))
	if err := checkDaemonSocketPaths(long); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("checkDaemonSocketPaths(%d-byte root) = %v, want an error", len(long.root), err)
	}
	// The longest root admitted still fits every socket.
	longest := newDaemonLayout("/" + strings.Repeat("r", daemonUnixPathMax-len("/run/")-daemonRunNameMax-2))
	if err := checkDaemonSocketPaths(longest); err != nil {
		t.Fatalf("checkDaemonSocketPaths(%d-byte root): %v", len(longest.root), err)
	}
	if got := len(filepath.Join(longest.run, daemonRunSocketName("f", "any console directory"))); got >= daemonUnixPathMax {
		t.Fatalf("socket path of %d bytes in an admitted root", got)
	}
}

func TestControlOnlyDaemonLayoutHasNoRuntimeRoot(t *testing.T) {
	// prepareDaemonLayout sets both for the processes the daemon starts.
	t.Setenv("TMPDIR", os.Getenv("TMPDIR"))
	t.Setenv(daemonRunDirEnv, os.Getenv(daemonRunDirEnv))
	layout := newDaemonLayout(shortDaemonTestRoot(t))
	if err := prepareDaemonLayout(layout, ""); err != nil {
		t.Fatalf("prepareDaemonLayout without a runtime root: %v", err)
	}
	for _, dir := range []string{layout.run, layout.tmp} {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Errorf("stat %q: %v", dir, err)
		}
	}
}

func TestControlOnlyDaemonRootMustFitItsHoldersSockets(t *testing.T) {
	// A gateway's root that fits its own sockets, but not those of its
	// control holder, whose root is in it.
	root := "/" + strings.Repeat("r", daemonUnixPathMax-len("/run/")-daemonRunNameMax-2)
	if err := checkDaemonSocketPaths(newDaemonLayout(root)); err != nil {
		t.Fatalf("checkDaemonSocketPaths(%d-byte root): %v", len(root), err)
	}
	t.Setenv("TTC_ROOT", shortDaemonTestRoot(t))
	t.Setenv("TTC_LOCAL_ROOT", root)
	var stdout, stderr bytes.Buffer
	if status := RunDaemon([]string{"--control-only"}, &stdout, &stderr); status != 2 || !strings.Contains(stderr.String(), "too long") {
		t.Fatalf("control-only daemon whose holder's sockets do not fit: status=%d stderr=%q", status, stderr.String())
	}
}

func TestDaemonSocketIsItsRoots(t *testing.T) {
	root := shortDaemonTestRoot(t)
	t.Setenv("TTC_ROOT", root)
	t.Setenv("TTC_LOCAL_ROOT", "")
	var stdout, stderr bytes.Buffer
	status := RunDaemon([]string{"--socket", filepath.Join(t.TempDir(), "ttcd.sock")}, &stdout, &stderr)
	if status != 2 || !strings.Contains(stderr.String(), "is not the socket of the daemon's root") {
		t.Fatalf("socket outside the root: status=%d stderr=%q", status, stderr.String())
	}
	if got, want := daemonSocketFromEnvironment(), filepath.Join(root, "ttcd.sock"); got != want {
		t.Fatalf("daemonSocketFromEnvironment() = %q, want %q", got, want)
	}
}

func TestDaemonRefusesNetworkRootUnlessAllowed(t *testing.T) {
	t.Setenv("TTC_ROOT", shortDaemonTestRoot(t))
	t.Setenv("TTC_LOCAL_ROOT", "")
	originalStatfs, originalPreflight := daemonStatfs, daemonPlatformPreflight
	t.Cleanup(func() { daemonStatfs, daemonPlatformPreflight = originalStatfs, originalPreflight })
	daemonStatfs = func(_ string, statfs *unix.Statfs_t) error {
		setTestNFSStatfs(statfs)
		return nil
	}
	daemonPlatformPreflight = func(string) error { return errors.New("fixture stops startup after the root checks") }
	var stdout, stderr bytes.Buffer
	if status := RunDaemon(nil, &stdout, &stderr); status != 1 || !strings.Contains(stderr.String(), "network filesystem") {
		t.Fatalf("network root: status=%d stderr=%q", status, stderr.String())
	}
	stderr.Reset()
	status := RunDaemon([]string{"--allow-network-filesystem-root"}, &stdout, &stderr)
	if status != 1 || !strings.Contains(stderr.String(), "warning") || !strings.Contains(stderr.String(), "fixture stops startup") {
		t.Fatalf("allowed network root: status=%d stderr=%q", status, stderr.String())
	}
}

func TestSweepDaemonFilesAfterRestartOfTheHost(t *testing.T) {
	originalBoot := hostBootID
	t.Cleanup(func() { hostBootID = originalBoot })
	hostBootID = func() (string, error) { return "this-boot", nil }
	layout := newDaemonLayout(shortDaemonTestRoot(t))
	runtimeRoot := filepath.Join(layout.root, "runtime", "runsc")
	for _, dir := range []string{layout.run, layout.tmp, filepath.Join(runtimeRoot, transientBundleDirName, ".branch-start-1")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(layout.run, daemonBootName):       "an-earlier-boot\n",
		filepath.Join(layout.run, "c0123.sock"):         "",
		filepath.Join(layout.tmp, "leftover"):           "",
		filepath.Join(runtimeRoot, "a_sandbox:a.state"): `{"id":"a","sandbox":{"pid":1}}`,
		filepath.Join(runtimeRoot, "a_sandbox:a.lock"):  "",
		filepath.Join(runtimeRoot, "sentry-panic.log"):  "kept",
	}
	for path, data := range files {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	sweepDaemonFiles(layout, runtimeRoot, func(id string) error {
		t.Errorf("deleted sandbox %q of an earlier boot through runsc, whose PIDs may be others' now", id)
		return nil
	}, nil)
	for path := range files {
		_, err := os.Stat(path)
		switch filepath.Base(path) {
		case daemonBootName, "sentry-panic.log":
			if err != nil {
				t.Errorf("%s: %v, want it kept", path, err)
			}
		default:
			if !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s: %v, want it removed", path, err)
			}
		}
	}
	if data, _ := os.ReadFile(filepath.Join(layout.run, daemonBootName)); string(data) != "this-boot\n" {
		t.Errorf("recorded boot %q, want this one", data)
	}
	if _, err := os.Stat(filepath.Join(runtimeRoot, transientBundleDirName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("transient bundles of an earlier boot: %v, want them removed", err)
	}
}

func TestSweepDaemonFilesRemovesWhatServesNoProcess(t *testing.T) {
	originalBoot := hostBootID
	t.Cleanup(func() { hostBootID = originalBoot })
	hostBootID = func() (string, error) { return "this-boot", nil }
	layout := newDaemonLayout(shortDaemonTestRoot(t))
	runtimeRoot := filepath.Join(layout.root, "runtime", "runsc")
	for _, dir := range []string{layout.run, layout.tmp, runtimeRoot} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(layout.run, daemonBootName), []byte("this-boot\n"), 0600); err != nil {
		t.Fatal(err)
	}
	live, err := net.Listen("unix", filepath.Join(layout.run, "flive.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	dead, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(layout.run, "cdead.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	dead.SetUnlinkOnClose(false)
	dead.Close()
	// A reaped child's PID names no process.
	child := exec.Command("true")
	if err := child.Run(); err != nil {
		t.Fatal(err)
	}
	states := map[string]int{"gone": child.Process.Pid, "alive": os.Getpid()}
	for id, pid := range states {
		state := fmt.Sprintf(`{"id":%q,"sandbox":{"pid":%d}}`, id, pid)
		if err := os.WriteFile(filepath.Join(runtimeRoot, id+"_sandbox:"+id+".state"), []byte(state), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(runtimeRoot, id+"_sandbox:"+id+".lock"), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(runtimeRoot, "deleted_sandbox:deleted.lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	old, recent := filepath.Join(layout.tmp, "old"), filepath.Join(layout.tmp, "recent")
	for _, path := range []string{old, recent} {
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	long := time.Now().Add(-2 * daemonTmpStaleAge)
	if err := os.Chtimes(old, long, long); err != nil {
		t.Fatal(err)
	}
	var deleted []string
	sweepDaemonFiles(layout, runtimeRoot, func(id string) error {
		deleted = append(deleted, id)
		return nil
	}, nil)
	if !reflect.DeepEqual(deleted, []string{"gone"}) {
		t.Errorf("deleted sandboxes %q, want only the one whose Sentry is gone", deleted)
	}
	for path, want := range map[string]bool{
		filepath.Join(layout.run, "flive.sock"):                    true,
		filepath.Join(layout.run, "cdead.sock"):                    false,
		filepath.Join(runtimeRoot, "alive_sandbox:alive.lock"):     true,
		filepath.Join(runtimeRoot, "deleted_sandbox:deleted.lock"): false,
		old:    false,
		recent: true,
	} {
		if _, err := os.Stat(path); (err == nil) != want {
			t.Errorf("%s: stat error %v, want kept=%t", path, err, want)
		}
	}
}

func TestDaemonRejectsUnsupportedHostKernel(t *testing.T) {
	if !testLinuxHost {
		t.Skip("Linux host-kernel compatibility is Linux-specific")
	}
	original := daemonKernelRelease
	defer func() {
		daemonKernelRelease = original
	}()
	daemonKernelRelease = func() (string, error) {
		return "3.16.99-legacy", nil
	}

	root := shortDaemonTestRoot(t)
	t.Setenv("TTC_ROOT", root)
	socket := filepath.Join(root, "ttcd.sock")
	var stdout, stderr bytes.Buffer
	status := RunDaemon([]string{
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
	if !testLinuxHost {
		t.Skip("Linux host-GSO fallback is Linux-specific")
	}
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

	dataRoot := shortDaemonTestRoot(t)
	t.Setenv("TTC_ROOT", dataRoot)
	var stdout, stderr bytes.Buffer
	status := RunDaemon(nil, &stdout, &stderr)
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

func TestReportDaemonStartupWarningsSurfacesCPUTopologyWarning(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "ttcd.log")
	log := "ttcd: warning: unrelated warning\n" +
		"ttcd: warning: " + daemonCPUTopologyWarning + "\n" +
		"ttcd listening\n"
	if err := os.WriteFile(logPath, []byte(log), 0600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	reportDaemonStartupWarnings(&stderr, logPath)
	if diagnostic := stderr.String(); diagnostic != "ttcd: warning: "+daemonCPUTopologyWarning+"\n" {
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

func TestTransactionSourcedFromMatchesFamilyAndRuntime(t *testing.T) {
	tx := cluster.Transaction{
		Identity:                cluster.ForkIdentity{SourceContainer: "family", SourceBranch: "visible"},
		SourcePhysicalContainer: "fork-target", SourcePhysicalBranch: "physical",
	}
	for _, tc := range []struct {
		container, branch string
		want              bool
	}{
		{"family", "visible", true},
		{"family", "", true},
		{"fork-target", "physical", true},
		{"fork-target", "", true},
		{"", "physical", true},
		{"", "", true},
		{"family", "physical", false},
		{"fork-target", "visible", false},
		{"other", "", false},
		{"fork-target", "other", false},
	} {
		if got := transactionSourcedFrom(tx, tc.container, tc.branch); got != tc.want {
			t.Errorf("transactionSourcedFrom(%s@%s) = %t, want %t", tc.container, tc.branch, got, tc.want)
		}
	}
	// A transaction recorded before physical sources were is matched by
	// its family alone.
	tx.SourcePhysicalContainer, tx.SourcePhysicalBranch = "", ""
	if !transactionSourcedFrom(tx, "family", "visible") || transactionSourcedFrom(tx, "fork-target", "physical") {
		t.Errorf("a transaction without a physical source is not matched by its family alone")
	}
}

// A command a standalone daemon runs on each of its targets in turn splits
// into its options and its targets, the signal kill names last among the
// options.
func TestEachTargetCommand(t *testing.T) {
	for _, test := range []struct {
		args            []string
		prefix, targets []string
		ok              bool
	}{
		{args: []string{"pause", "f@a", "g"}, prefix: []string{"pause"}, targets: []string{"f@a", "g"}, ok: true},
		{args: []string{"kill", "--all", "-s", "TERM", "f@a", "f@b"}, prefix: []string{"kill", "--all", "-s", "TERM"}, targets: []string{"f@a", "f@b"}, ok: true},
		{args: []string{"kill", "f@a", "f@b", "TERM"}, prefix: []string{"kill", "--signal", "TERM"}, targets: []string{"f@a", "f@b"}, ok: true},
		{args: []string{"restart", "-t", "0", "--force", "f@a"}, prefix: []string{"restart", "-t", "0", "--force"}, targets: []string{"f@a"}, ok: true},
		{args: []string{"restart", "--bogus", "f@a"}},
		{args: []string{"stop", "f@a"}},
		{args: []string{"pause"}},
	} {
		prefix, targets, ok := eachTargetCommand(test.args)
		if ok != test.ok || !slices.Equal(prefix, test.prefix) || !slices.Equal(targets, test.targets) {
			t.Errorf("eachTargetCommand(%q) = %q, %q, %t; want %q, %q, %t", test.args, prefix, targets, ok, test.prefix, test.targets, test.ok)
		}
	}
}

// A start's or a restart's container operand names its main branch.
func TestMainBranchOperands(t *testing.T) {
	for _, test := range []struct{ args, want []string }{
		{args: []string{"restart", "-t", "3", "fam", "fam@a"}, want: []string{"restart", "-t", "3", "fam@main", "fam@a"}},
		{args: []string{"start", "-ai", "fam"}, want: []string{"start", "-ai", "fam@main"}},
		{args: []string{"start", "fam@b"}, want: []string{"start", "fam@b"}},
		{args: []string{"kill", "fam"}, want: []string{"kill", "fam"}},
		{args: []string{"restart", "--bogus", "fam"}, want: []string{"restart", "--bogus", "fam"}},
	} {
		if got := mainBranchOperands(test.args); !slices.Equal(got, test.want) {
			t.Errorf("mainBranchOperands(%q) = %q, want %q", test.args, got, test.want)
		}
	}
}

// An import whose runtime runs no branch replaces the runtime with its own,
// under the lifecycle lock; one that runs a branch it goes beside; and either
// way the import's mark on the runtime is held once the replacement returns,
// unless the import gave up first, when nothing is done.
func TestReplaceRuntimeForImport(t *testing.T) {
	for _, test := range []struct {
		name, branches string
		gaveUp         bool
		wantCreated    bool
		wantDeleted    bool
	}{
		{name: "stopped", branches: `[{"ID":"x","State":"exited"},{"ID":"d","State":"dehydrated"}]`, wantCreated: true, wantDeleted: true},
		{name: "live", branches: `[{"ID":"x","State":"running"}]`},
		{name: "gave up", branches: `[{"ID":"x","State":"exited"}]`, gaveUp: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			runsc := filepath.Join(dir, "runsc")
			script := `#!/bin/sh
case "$3" in
list)
	if [ -f "$0.deleted" ]; then echo '[]'; else printf '[{"id":"family","pid":%s,"status":"stopped"}]\n' "$PPID"; fi
	;;
timetravel)
	cat "$0.branches"
	;;
delete)
	touch "$0.deleted"
	;;
*)
	echo "unexpected command: $*" >&2
	exit 2
	;;
esac
`
			if err := os.WriteFile(runsc, []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(runsc+".branches", []byte(test.branches), 0644); err != nil {
				t.Fatal(err)
			}
			b := &runtimeBackend{root: "/runtime", runsc: runsc, dataRoot: filepath.Join(dir, "shared"), localRoot: filepath.Join(dir, "local"), operations: &familyOperations{}}
			handover := &importReplacement{}
			if test.gaveUp && !handover.giveUp() {
				t.Fatal("an import could not give up before its replacement began")
			}
			created := false
			replaced, making, err := b.replaceRuntimeForImport("family", "y", func() error {
				created = true
				return nil
			}, handover)
			if test.gaveUp {
				if !errors.Is(err, errImportGaveUp) || created || replaced {
					t.Fatalf("replacement for an import that gave up = %t, %v, create called %t", replaced, err, created)
				}
				if b.operations.beingMade("family", "y") {
					t.Fatal("a replacement for an import that gave up left its mark")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			_, statErr := os.Stat(runsc + ".deleted")
			if replaced != test.wantCreated || created != test.wantCreated || (statErr == nil) != test.wantDeleted {
				t.Fatalf("replaced %t, create called %t, deleted %t; want %t, %t, %t", replaced, created, statErr == nil, test.wantCreated, test.wantCreated, test.wantDeleted)
			}
			if !b.operations.beingMade("family", "y") {
				t.Fatal("the import's mark is not held after the replacement")
			}
			making()
			if b.operations.beingMade("family", "y") {
				t.Fatal("the import's mark outlived its end")
			}
		})
	}
}

// A replacement begins only if the import still waits, and an import gives
// up only before the replacement begins.
func TestImportReplacementHandover(t *testing.T) {
	waiting := &importReplacement{}
	if !waiting.begin() || waiting.giveUp() || waiting.abandoned() {
		t.Fatal("an import gave up after its replacement began")
	}
	gone := &importReplacement{}
	if !gone.giveUp() || gone.begin() || !gone.abandoned() {
		t.Fatal("a replacement began after its import gave up")
	}
}

func TestRestoreLostItsPeer(t *testing.T) {
	for message, want := range map[string]bool{
		"timetravel distributed-restore failed: restoring distributed fork \"t\": no active peer is available for dehydration restore": true,
		"sandbox is stopped, not available for distributed peer import":                                                                true,
		"dehydration restore peer \"x\" became exited":                                                                                 true,
		"connecting donated distributed page stream 1/4: connection refused":                                                           false,
	} {
		if got := restoreLostItsPeer(errors.New(message)); got != want {
			t.Errorf("restoreLostItsPeer(%q) = %t, want %t", message, got, want)
		}
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
	if !testLinuxHost {
		t.Skip("Linux rootless namespace preflight is Linux-specific")
	}
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

	root := shortDaemonTestRoot(t)
	t.Setenv("TTC_ROOT", root)
	socket := filepath.Join(root, "ttcd.sock")
	var stdout, stderr bytes.Buffer
	status := RunDaemon(nil, &stdout, &stderr)
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
	// A client would look for a standalone daemon in its TTC_LOCAL_ROOT.
	t.Setenv("TTC_LOCAL_ROOT", filepath.Join(t.TempDir(), "local"))
	if _, err := daemonRootsFromEnvironment(false); err == nil || !strings.Contains(err.Error(), "TTC_LOCAL_ROOT") {
		t.Fatalf("standalone daemon with TTC_LOCAL_ROOT: err=%v", err)
	}
	t.Setenv("TTC_LOCAL_ROOT", "")
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

func TestFreshDaemonRootRejectsNestedStartupLog(t *testing.T) {
	// A daemon's log is in its root; a directory holding one is another's.
	root := t.TempDir()
	run := filepath.Join(root, "run", "daemon")
	if err := os.MkdirAll(run, 0700); err != nil {
		t.Fatal(err)
	}
	logFile, err := os.OpenFile(filepath.Join(run, "ttcd.log"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	logInfo, err := logFile.Stat()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || freshDaemonRootEntryAllowed(entries[0], logInfo) {
		t.Fatalf("a directory holding the startup log was admitted: entries=%v err=%v", entries, err)
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
	root := shortDaemonTestRoot(t)
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
	status := RunDaemon([]string{"--unsafe-no-host-isolation"}, &stdout, &stderr)
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
	if err := server.OwnConsoleSidecar("console-lock", command, selector{}, false); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("OwnConsoleSidecar: %v", err)
	}
	if got := server.ownedConsoleSidecars(); got != 1 {
		t.Fatalf("owned sidecars=%d, want 1", got)
	}
	if err := server.OwnConsoleSidecar("console-lock", exec.Command("true"), selector{}, false); err == nil {
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

func TestDaemonSuppressesExplicitConsoleExitOnce(t *testing.T) {
	command := exec.Command("sleep", "30")
	if err := command.Start(); err != nil {
		t.Fatalf("starting sidecar fixture: %v", err)
	}
	server := &daemonServer{}
	const key = "console-lock"
	if err := server.OwnConsoleSidecar(key, command, selector{}, false); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("OwnConsoleSidecar: %v", err)
	}

	finish := server.SuppressConsoleExitOnce(key)
	finish(true)
	if !server.consumeSuppressedConsoleExit(key) {
		t.Fatal("committed explicit stop fence was not consumed")
	}
	if server.consumeSuppressedConsoleExit(key) {
		t.Fatal("one-shot explicit stop fence was consumed twice")
	}

	finish = server.SuppressConsoleExitOnce(key)
	finish(false)
	if server.consumeSuppressedConsoleExit(key) {
		t.Fatal("cancelled explicit stop fence remained active")
	}

	if err := command.Process.Kill(); err != nil {
		t.Fatalf("stopping sidecar fixture: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for server.ownedConsoleSidecars() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
}

func TestDaemonObservesInheritedConsoleSidecarOnce(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "console.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lockFile.Close()
	if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	server := &daemonServer{}
	target := selector{container: "demo", branch: "main"}
	server.ObserveInheritedConsoleSidecar(lockPath, target, false)
	server.ObserveInheritedConsoleSidecar(lockPath, target, false)
	server.sidecarMu.Lock()
	watchers := len(server.inheritedConsole)
	server.sidecarMu.Unlock()
	if watchers != 1 {
		t.Fatalf("inherited watcher count=%d, want 1", watchers)
	}
	if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		server.sidecarMu.Lock()
		watchers = len(server.inheritedConsole)
		server.sidecarMu.Unlock()
		if watchers == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("inherited watcher did not observe lock release")
}

func TestDaemonSidecarRunsExitCallbackAfterReaping(t *testing.T) {
	command := exec.Command("true")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	server := &daemonServer{}
	called := make(chan struct{})
	if err := server.ownSidecar("console", "console-lock", command, func() {
		if got := server.ownedConsoleSidecars(); got != 0 {
			t.Errorf("callback observed %d owned sidecars, want 0", got)
		}
		close(called)
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-called:
	case <-time.After(5 * time.Second):
		t.Fatal("sidecar exit callback did not run")
	}
	if command.ProcessState == nil {
		t.Fatal("callback ran before child was reaped")
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
		{[]string{"restore", "--name", "copy", "-"}, true},
		{[]string{"restore", "--name", "copy", "/tmp/state.ttc"}, false},
		{[]string{"exec", "ls", "demo"}, false},
		{[]string{"exec", "demo@main", "true"}, false},
		{[]string{"logs", "-f", "demo"}, false},
	} {
		if got := daemonCommandUsesStdin(test.args); got != test.want {
			t.Errorf("daemonCommandUsesStdin(%q)=%t, want %t", test.args, got, test.want)
		}
	}
}

func TestDaemonCommandUsesTerminalControl(t *testing.T) {
	for _, test := range []struct {
		args []string
		want bool
	}{
		{[]string{"attach", "demo@main"}, true},
		{[]string{"container", "attach", "demo@main"}, true},
		{[]string{"exec", "-td", "demo@main", "sh"}, true},
		{[]string{"exec", "--tty", "demo@main", "sh"}, true},
		{[]string{"exec", "ls", "demo@main"}, false},
		{[]string{"run", "-itd", "alpine"}, true},
		{[]string{"run", "--detach", "alpine"}, false},
		{[]string{"start", "-a", "demo@main"}, true},
		{[]string{"start", "demo@main"}, false},
		{[]string{"logs", "-f", "demo@main"}, false},
	} {
		if got := daemonCommandUsesTerminalControl(test.args); got != test.want {
			t.Errorf("daemonCommandUsesTerminalControl(%q)=%t, want %t", test.args, got, test.want)
		}
	}
}

func TestDaemonTerminalControlStreamsWithoutGuestStdin(t *testing.T) {
	request := daemonRequest{TerminalControl: true}
	if !request.streamsClientFrames() {
		t.Fatal("terminal control did not keep client frames routable")
	}
	if request.Stdin {
		t.Fatal("terminal control unexpectedly enabled guest stdin")
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
	unlockFirst := server.lockDataRoot(root)

	secondAcquired := make(chan func(), 1)
	go func() {
		secondAcquired <- server.lockDataRoot(root)
	}()
	select {
	case unlock := <-secondAcquired:
		unlock()
		t.Fatal("a second request acquired the same data-root lock")
	case <-time.After(50 * time.Millisecond):
	}
	unlockOther := server.lockDataRoot(root + "-other")
	unlockOther()

	unlockFirst()
	select {
	case unlock := <-secondAcquired:
		unlock()
	case <-time.After(5 * time.Second):
		t.Fatal("lifecycle operation remained blocked after unlock")
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

// Listings may run without the lifecycle lock, rather than wait for a long
// mutation to release it; sizing, and anything that mutates, may not.
func TestDaemonInventoryListingsMayRunWithoutTheLock(t *testing.T) {
	for _, args := range [][]string{{"ps", "-a"}, {"ls", "--json"}, {"inspect", "demo"}, {"top", "demo@main"}, {"container", "ls"}} {
		if !daemonCommandListsInventory(args) {
			t.Errorf("%v must be able to list without the lifecycle lock", args)
		}
	}
	for _, args := range [][]string{{"ps", "--size"}, {"ps", "-as"}, {"stats", "demo@main"}, {"diff", "demo@main"}, {"rm", "demo"}, {"dehydrate", "demo"}} {
		if daemonCommandListsInventory(args) {
			t.Errorf("%v must not run without the lifecycle lock", args)
		}
	}
	server := &daemonServer{}
	root := t.TempDir()
	unlock := server.lockDataRoot(root)
	if _, locked := server.tryLockDataRoot(root); locked {
		t.Fatal("tryLockDataRoot took a held lock")
	}
	unlock()
	unlock, locked := server.tryLockDataRoot(root)
	if !locked {
		t.Fatal("tryLockDataRoot did not take a free lock")
	}
	unlock()
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
	socket := shortDaemonTestSocket(t)
	dataRoot := t.TempDir()
	done := make(chan error, 1)
	go func() {
		done <- serveDaemon(socket, defaultRuntimePlatform, true, hostKernelCompatibility{release: "6.8.0"}, dataRoot, "/runtime", "/runsc", ttcvolume.Capability{Mode: ttcvolume.CopyModeFull}, io.Discard, io.Discard)
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
		!strings.Contains(stdout, `"runtime_platform":"`+defaultRuntimePlatform+`"`) ||
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

func TestExtractPlacementNode(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want []string
		node string
	}{
		{name: "run", args: []string{"run", "--node", "node-b", "--name", "demo", "alpine", "echo"}, want: []string{"run", "--name", "demo", "alpine", "echo"}, node: "node-b"},
		{name: "create", args: []string{"create", "--node", "node-b", "--name", "demo", "alpine", "echo"}, want: []string{"create", "--name", "demo", "alpine", "echo"}, node: "node-b"},
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
		opts.PagingPolicy.Push != ttcpolicy.PushEagerDrain {
		t.Fatalf("cluster options=%+v", opts)
	}
	if node, url, name, err := parseClusterPeer("node-b=10.0.0.2:7443"); err != nil || node != "node-b" || url != "http://10.0.0.2:7443" || name != "" {
		t.Fatalf("parseClusterPeer=(%q, %q, %q, %v)", node, url, name, err)
	}
}

func TestDefaultClusterTransactionCapacitySupportsHighFanout(t *testing.T) {
	if got := defaultDaemonClusterOptions().MaxTransactions; got < 1024 {
		t.Fatalf("default max transactions=%d, want at least 1024 for batch remote fork", got)
	}
}

// Every node of a cluster starts alike: its shared root anchors the cluster.
func TestSimpleClusterOptions(t *testing.T) {
	opts := defaultDaemonClusterOptions()
	args := []string{"--node", "node-a", "--listen", "127.0.0.1:7443", "--advertise", "node-a.example:7443", "--memory-target", "8GiB"}
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
	if !opts.Configured || opts.NodeID != "node-a" || opts.MemoryTarget != 8<<30 || opts.Listen != "127.0.0.1:7443" || opts.Advertise != "node-a.example:7443" || opts.RepositoryID == "" || !opts.RepositoryGenerated {
		t.Fatalf("simple cluster options=%+v", opts)
	}
	// A node finds the repository of a shared root that has one.
	if err := prepareDaemonStorageRoot(roots.shared, daemonRootRoleClusterShared, "repo-found", ""); err != nil {
		t.Fatal(err)
	}
	found := defaultDaemonClusterOptions()
	found.Configured, found.NodeID = true, "node-b"
	if err := resolveSimpleClusterOptions(&found, roots); err != nil || found.RepositoryID != "repo-found" || found.RepositoryGenerated {
		t.Fatalf("options=%+v, %v, want repo-found", found, err)
	}
	for _, gone := range [][]string{{"--cluster-init"}, {"--cluster-join", "node-a.example:7443"}, {"--cluster-join=node-a.example:7443"}, {"--cluster-peer", "node-b=10.0.0.2:7443"}} {
		if _, err := consumeDaemonClusterOption(gone, &found); err == nil || !strings.Contains(err.Error(), "is gone") {
			t.Fatalf("consuming %q: %v, want it gone", gone, err)
		}
	}
}

func TestReadClusterSharedRootMarkerWaitsForInitialization(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "shared")
	created := make(chan error, 1)
	go func() {
		time.Sleep(50 * time.Millisecond)
		created <- prepareDaemonStorageRoot(root, daemonRootRoleClusterShared, "repo-visible", "")
	}()
	started := time.Now()
	marker, err := readClusterSharedRootMarker(root, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-created; err != nil {
		t.Fatal(err)
	}
	if marker.RepositoryID != "repo-visible" {
		t.Fatalf("marker=%+v", marker)
	}
	if elapsed := time.Since(started); elapsed < 40*time.Millisecond {
		t.Fatalf("marker read returned before delayed initialization: %v", elapsed)
	}
}

func TestReadClusterSharedRootMarkerTimeout(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	started := time.Now()
	_, err := readClusterSharedRootMarker(root, 25*time.Millisecond)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read error=%v, want ENOENT", err)
	}
	if elapsed := time.Since(started); elapsed < 20*time.Millisecond {
		t.Fatalf("missing marker returned before visibility grace: %v", elapsed)
	}
}

func TestControlOnlyClusterOptions(t *testing.T) {
	shared := t.TempDir()
	if err := prepareDaemonStorageRoot(shared, daemonRootRoleClusterShared, "repo-control", ""); err != nil {
		t.Fatal(err)
	}
	roots := daemonRoots{shared: shared, local: shared}
	opts := defaultDaemonClusterOptions()
	args := []string{"--control-only", "--node", "login-gateway"}
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
}

// TestClusterAuthToken makes a cluster's token in its shared root once, keeps
// it readable by the root's owner alone, and refuses a token too short.
func TestClusterAuthToken(t *testing.T) {
	root := t.TempDir()
	token, err := clusterAuthToken(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hex.DecodeString(token); err != nil || len(token) != 64 {
		t.Fatalf("token %q, want 32 bytes in hexadecimal", token)
	}
	path := filepath.Join(root, clusterTokenFile)
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v, %v", info, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if again, err := clusterAuthToken(root); err != nil || again != token {
		t.Fatalf("token read again: %q, %v; want %q", again, err, token)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("token file after a read: %v, %v", info, err)
	}
	if err := os.WriteFile(path, []byte("short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := clusterAuthToken(root); err == nil {
		t.Fatal("a short token was taken")
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
	opts.AuthToken = "test-cluster-token"
	opts.Peers = []string{"seed=" + strings.TrimPrefix(seed.URL, "http://")}
	control, err := prepareControlOnlyCluster(opts, "/shared/ttc")
	if err != nil {
		t.Fatal(err)
	}
	if gotNode != "login-gateway" || len(control.peerSnapshot()) != 1 || control.repositoryID != "repo-control" {
		t.Fatalf("probe node=%q control=%+v", gotNode, control)
	}
	opts.RepositoryID = "another-repository"
	if _, err := prepareControlOnlyCluster(opts, "/shared/ttc"); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched repository error=%v", err)
	}
}

// Between holders, a join asks again.
func TestJoinAuthorityWaitsForAHolder(t *testing.T) {
	calls := 0
	seed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/authority/join" {
			http.NotFound(w, r)
			return
		}
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls < 3 {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":      "the TTC cluster authority has no holder",
				"not_holder": true,
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"node_id": "joining"})
	}))
	defer seed.Close()
	client, err := cluster.NewPlainClient(seed.URL, "joining", "test-cluster-token", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	request := cluster.AuthorityJoinRequest{NodeID: "joining"}
	if err := joinAuthorityThroughSeed(ctx, client, request, "joining", "test-cluster-token"); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("join calls=%d, want 3", calls)
	}
}

func TestJoinAuthorityWaitsForSeedListener(t *testing.T) {
	calls := 0
	seed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(map[string]string{"node_id": "joining"})
	}))
	defer seed.Close()
	client, err := cluster.NewPlainClient(seed.URL, "joining", "test-cluster-token", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client.HTTP.Transport = &failFirstRoundTripper{remaining: 2, next: http.DefaultTransport}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	request := cluster.AuthorityJoinRequest{NodeID: "joining"}
	if err := joinAuthorityThroughSeed(ctx, client, request, "joining", "test-cluster-token"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("successful seed calls=%d, want 1", calls)
	}
}

func TestControlOnlyClusterDiscoversComputePeers(t *testing.T) {
	var second *httptest.Server
	second = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/authority/query" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(cluster.AuthorityState{Nodes: map[string]cluster.NodeRecord{
			"compute-a": {NodeID: "compute-a", State: cluster.NodeReady, ControlURL: second.URL},
		}})
	}))
	defer second.Close()

	var seed *httptest.Server
	seed = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/daemon":
			payload, _ := json.Marshal(map[string]any{
				"build_id": version.Version(),
				"cluster": map[string]any{"capabilities": cluster.Capabilities{
					NodeID: "seed", RepositoryID: "repo-control",
				}},
			})
			var mu sync.Mutex
			_ = writeDaemonFrame(w, &mu, daemonFrameStdout, append(payload, '\n'))
			_ = writeDaemonStatus(w, &mu, 0)
		case "/v1/authority/query":
			_ = json.NewEncoder(w).Encode(cluster.AuthorityState{Nodes: map[string]cluster.NodeRecord{
				"seed":      {NodeID: "seed", State: cluster.NodeReady, ControlURL: seed.URL},
				"compute-a": {NodeID: "compute-a", State: cluster.NodeReady, ControlURL: second.URL},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer seed.Close()

	opts := defaultDaemonClusterOptions()
	opts.Configured, opts.ControlOnly = true, true
	opts.NodeID, opts.RepositoryID = "login-gateway", "repo-control"
	opts.AuthToken = "test-cluster-token"
	opts.Peers = []string{"seed=" + strings.TrimPrefix(seed.URL, "http://")}
	control, err := prepareControlOnlyCluster(opts, "/shared/ttc")
	if err != nil {
		t.Fatal(err)
	}
	got := control.seedURLs()
	want := []string{second.URL, seed.URL}
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Fatalf("discovered peers=%v, want %v", got, want)
	}
}

func TestControlOnlyClusterFollowsTheHolderRedirect(t *testing.T) {
	var leader *httptest.Server
	leader = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/authority/query" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(cluster.AuthorityState{Nodes: map[string]cluster.NodeRecord{
			"leader": {NodeID: "leader", State: cluster.NodeReady, ControlURL: leader.URL},
		}})
	}))
	defer leader.Close()

	follower := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/daemon":
			payload, _ := json.Marshal(map[string]any{
				"build_id": version.Version(),
				"cluster": map[string]any{"capabilities": cluster.Capabilities{
					NodeID: "follower", RepositoryID: "repo-control",
				}},
			})
			var mu sync.Mutex
			_ = writeDaemonFrame(w, &mu, daemonFrameStdout, append(payload, '\n'))
			_ = writeDaemonStatus(w, &mu, 0)
		case "/v1/authority/query":
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": "not the holder", "not_holder": true, "holder_id": "leader",
				"holder_control_url": leader.URL,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer follower.Close()

	opts := defaultDaemonClusterOptions()
	opts.Configured, opts.ControlOnly = true, true
	opts.NodeID, opts.RepositoryID = "login-gateway", "repo-control"
	opts.AuthToken = "test-cluster-token"
	opts.Peers = []string{"follower=" + strings.TrimPrefix(follower.URL, "http://")}
	control, err := prepareControlOnlyCluster(opts, "/shared/ttc")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := control.seedURLs(), []string{leader.URL}; !slices.Equal(got, want) {
		t.Fatalf("peers after leader redirect=%v, want %v", got, want)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	state, err := control.authorityState(ctx, cluster.AuthorityQuery{Nodes: cluster.All[string]()})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := state.Nodes["leader"]; !ok {
		t.Fatalf("redirected authority state=%+v", state.Nodes)
	}
}

// An owner refuses a command routed by a placement that another command has
// changed since, before the command does anything. The entry routes it again
// rather than give the client the refusal: it holds back the first frame of
// the answer, however the answer comes in pieces, until it knows. Any other
// answer reaches the client as it came.
func TestStaleRouteFilter(t *testing.T) {
	frames := func(write func(io.Writer, *sync.Mutex)) []byte {
		var buffer bytes.Buffer
		var mu sync.Mutex
		write(&buffer, &mu)
		return buffer.Bytes()
	}
	refusal := frames(func(w io.Writer, mu *sync.Mutex) {
		_ = writeDaemonFrame(w, mu, daemonFrameStderr, []byte("ttcd: stale routed command: branch f@b is unplaced\n"))
		_ = writeDaemonStatus(w, mu, 1)
	})
	other := frames(func(w io.Writer, mu *sync.Mutex) {
		_ = writeDaemonFrame(w, mu, daemonFrameStderr, []byte("ttcd: cannot pause f@b: branch is exited\n"))
		_ = writeDaemonStatus(w, mu, 1)
	})
	output := frames(func(w io.Writer, mu *sync.Mutex) {
		_ = writeDaemonFrame(w, mu, daemonFrameStdout, []byte("f@b\n"))
		_ = writeDaemonStatus(w, mu, 0)
	})
	for _, chunk := range []int{1, 3, 7, len(refusal)} {
		feed := func(filter *staleRouteFilter, data []byte) {
			for len(data) != 0 {
				n := min(chunk, len(data))
				if written, err := filter.Write(data[:n]); err != nil || written != n {
					t.Fatalf("writing %d bytes: %d, %v", n, written, err)
				}
				data = data[n:]
			}
		}
		var client bytes.Buffer
		filter := &staleRouteFilter{next: &client}
		feed(filter, refusal)
		if !filter.refused || client.Len() != 0 {
			t.Fatalf("in pieces of %d, a stale route's refusal: refused %t, %d bytes reached the client", chunk, filter.refused, client.Len())
		}
		if err := filter.release(); err != nil || !bytes.Equal(client.Bytes(), refusal) {
			t.Fatalf("in pieces of %d, the refusal released after the last retry: %q, %v", chunk, client.Bytes(), err)
		}
		if !protocol.StaleRouteAnswer("ttcd: stale routed command: branch f@b is unplaced\n") ||
			protocol.StaleRouteAnswer("ttcd: cannot pause f@b: branch is exited\n") ||
			protocol.StaleRouteAnswer("ttc: warning: x\nttcd: stale routed command: branch f@b is unplaced\n") ||
			protocol.StaleRouteAnswer("ttcd: stale routed command: branch f@b is unplaced\nttc: more\n") {
			t.Fatal("a client's answer was misjudged as, or not as, a stale route's refusal alone")
		}
		for _, answer := range [][]byte{other, output} {
			client.Reset()
			filter := &staleRouteFilter{next: &client}
			feed(filter, answer)
			if err := filter.release(); err != nil {
				t.Fatal(err)
			}
			if filter.refused || !bytes.Equal(client.Bytes(), answer) {
				t.Fatalf("in pieces of %d, answer %q reached the client as %q (refused %t)", chunk, answer, client.Bytes(), filter.refused)
			}
		}
	}
}

// A routed command that its node lost before answering goes to it again
// while the node may run it once: with a request ID, without streamed input,
// for a lost connection, until ownerLossWait passes.
func TestOwnerLossRetry(t *testing.T) {
	withID := daemonRequest{RequestID: "0123456789abcdef0123456789abcdef"}
	lostErr := fmt.Errorf("Post \"http://node/v1/daemon\": %w", io.EOF)
	var loss ownerLoss
	if !ownerLossRetry(withID, false, lostErr, &loss) {
		t.Error("a command with a request ID was not sent again once its node lost it")
	}
	for _, test := range []struct {
		name      string
		request   daemonRequest
		automatic bool
		err       error
	}{
		{"without a request ID", daemonRequest{}, false, lostErr},
		{"with streamed input", daemonRequest{RequestID: withID.RequestID, Stdin: true}, false, lostErr},
		{"answered with an error", withID, false, errors.New("the node refused the command")},
		{"refused by a node chosen for it", withID, true, fmt.Errorf("dial: %w", syscall.ECONNREFUSED)},
	} {
		var loss ownerLoss
		if ownerLossRetry(test.request, test.automatic, test.err, &loss) {
			t.Errorf("a command %s was sent again", test.name)
		}
	}
	loss.deadline = time.Now().Add(-time.Second)
	if ownerLossRetry(withID, false, lostErr, &loss) {
		t.Error("a command was sent again after ownerLossWait")
	}
}

func TestControlOnlyProxyInputDoesNotWaitForUnusedStdin(t *testing.T) {
	blocked := make(chan struct{})
	reader := io.Reader(readerFunc(func([]byte) (int, error) {
		<-blocked
		return 0, io.EOF
	}))
	done := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(controlOnlyProxyInput("header\n", reader, false))
		done <- string(data)
	}()
	select {
	case got := <-done:
		if got != "header\n" {
			t.Fatalf("proxy input=%q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("non-streaming proxy input waited for Unix client EOF")
	}
	close(blocked)
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

func TestPrepareDaemonClusterRejectsPartialConfiguration(t *testing.T) {
	_, err := prepareDaemonCluster(daemonClusterOptions{Configured: true, Listen: "127.0.0.1:0", MaxTransactions: 1}, t.TempDir(), t.TempDir(), "systrap", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "requires") {
		t.Fatalf("partial cluster configuration error=%v", err)
	}
}

func TestValidateClusterCPUEnforcement(t *testing.T) {
	if err := validateClusterCPUEnforcement("systrap", false, false); err == nil || !strings.Contains(err.Error(), "CPUID faulting") || !strings.Contains(err.Error(), "--experimental-no-cpuid-faulting") {
		t.Fatalf("systrap without CPUID faulting: %v", err)
	}
	if err := validateClusterCPUEnforcement("systrap", true, false); err != nil {
		t.Fatalf("systrap with CPUID faulting: %v", err)
	}
	if err := validateClusterCPUEnforcement("systrap", false, true); err != nil {
		t.Fatalf("systrap without CPUID faulting in a cluster that waives it: %v", err)
	}
	if err := validateClusterCPUEnforcement("kvm", false, false); err != nil {
		t.Fatalf("KVM without host CPUID faulting: %v", err)
	}
}

func TestNoCPUIDFaultingOption(t *testing.T) {
	opts := defaultDaemonClusterOptions()
	if consumed, err := consumeDaemonCPUOption([]string{"--experimental-no-cpuid-faulting", "--node", "n1"}, &opts); consumed != 1 || err != nil || !opts.NoCPUIDFaulting {
		t.Fatalf("consumed=%d err=%v waived=%v", consumed, err, opts.NoCPUIDFaulting)
	}
	if err := validateNoCPUIDFaultingOption(opts); err == nil || !strings.Contains(err.Error(), "compute nodes") {
		t.Fatalf("standalone daemon with the option: %v", err)
	}
	opts.Configured = true
	if err := validateNoCPUIDFaultingOption(opts); err != nil {
		t.Fatalf("cluster node with the option: %v", err)
	}
	opts.ControlOnly = true
	if err := validateNoCPUIDFaultingOption(opts); err == nil {
		t.Fatal("control-only daemon accepted the option")
	}
}

func TestCPUProfileWithoutFaultingWarning(t *testing.T) {
	portable := daemonCPUProfile{Mode: "portable"}
	if got := cpuProfileWithoutFaultingWarning(portable, "systrap", false); !strings.Contains(got, "not the portable CPU profile") {
		t.Errorf("portable profile on a systrap host without CPUID faulting: %q", got)
	}
	for _, test := range []struct {
		profile  daemonCPUProfile
		platform string
		faulting bool
	}{
		{profile: portable, platform: "systrap", faulting: true},
		{profile: portable, platform: "kvm", faulting: false},
		{profile: daemonCPUProfile{Mode: "host"}, platform: "systrap", faulting: false},
		{profile: daemonCPUProfile{Mode: "host", NoCPUIDFaulting: true}, platform: "systrap", faulting: false},
	} {
		if got := cpuProfileWithoutFaultingWarning(test.profile, test.platform, test.faulting); got != "" {
			t.Errorf("%+v on %s, faulting %t: %q, want no warning", test.profile, test.platform, test.faulting, got)
		}
	}
}

func TestCPUTopologyOption(t *testing.T) {
	for _, test := range []struct {
		args     []string
		consumed int
		want     string
	}{
		{args: []string{"--cpu-topology", "host", "--node", "n1"}, consumed: 2, want: "host"},
		{args: []string{"--cpu-topology=Single"}, consumed: 1, want: "single"},
	} {
		opts := defaultDaemonClusterOptions()
		if consumed, err := consumeDaemonCPUOption(test.args, &opts); consumed != test.consumed || err != nil || opts.CPUTopology != test.want || !opts.CPUTopologySet {
			t.Errorf("%q: consumed=%d err=%v topology=%q set=%v", test.args, consumed, err, opts.CPUTopology, opts.CPUTopologySet)
		}
	}
	for _, args := range [][]string{{"--cpu-topology", "numa"}, {"--cpu-topology"}, {"--cpu-topology="}} {
		opts := defaultDaemonClusterOptions()
		if _, err := consumeDaemonCPUOption(args, &opts); err == nil {
			t.Errorf("%q accepted", args)
		}
	}
	opts := defaultDaemonClusterOptions()
	opts.CPUTopology, opts.CPUTopologySet = "host", true
	if err := validateCPUTopologyOption(opts); err != nil {
		t.Errorf("a daemon that runs containers: %v", err)
	}
	opts.ControlOnly = true
	if err := validateCPUTopologyOption(opts); err == nil {
		t.Error("a control-only daemon accepted --cpu-topology")
	}
}

func TestResolveDaemonCPUTopology(t *testing.T) {
	for _, mode := range []string{"", cputopology.Single} {
		if got, err := resolveDaemonCPUTopology(mode, ""); err != nil || got.Mode != cputopology.Single || cpuTopologyAnnotation(got) != "" {
			t.Errorf("resolveDaemonCPUTopology(%q) = (%+v, %v), want the single topology, which needs no annotation", mode, got, err)
		}
	}
	if _, err := resolveDaemonCPUTopology("numa", ""); err == nil {
		t.Error("an unknown topology was resolved")
	}
	host, err := resolveDaemonCPUTopology(cputopology.Host, "")
	if err != nil {
		t.Skipf("this host's CPU topology cannot be read: %v", err)
	}
	if parsed, err := cputopology.Parse(cpuTopologyAnnotation(host)); err != nil || parsed.Fingerprint() != host.Fingerprint() {
		t.Errorf("the host topology's annotation parses as (%+v, %v)", parsed, err)
	}
	if _, err := resolveDaemonCPUTopology(cputopology.Host, host.Fingerprint()); err != nil {
		t.Errorf("this host against its own fingerprint: %v", err)
	}
	if _, err := resolveDaemonCPUTopology(cputopology.Host, "host;0:another"); err == nil || !strings.Contains(err.Error(), "differs from the cluster's") {
		t.Errorf("this host against another's fingerprint: %v", err)
	}
}

func TestImmutableClusterConfigRecordsCPUTopology(t *testing.T) {
	root := t.TempDir()
	profile := daemonCPUProfile{Version: daemonCPUProfileVersion, Mode: "host", Features: []string{"sse", "sse2"}, Topology: cputopology.Host, TopologyFingerprint: "host;0:0.0.0.0.0/0/0"}
	bootstrap := defaultDaemonClusterOptions()
	bootstrap.CPUProfileSet, bootstrap.CPUTopology, bootstrap.CPUTopologySet, bootstrap.ResolvedCPU = true, cputopology.Host, true, profile
	if _, err := applyImmutableClusterConfig(root, &bootstrap); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".ttc-cluster-config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var stored daemonImmutableClusterConfig
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	// A build that predates the choice refuses it.
	if stored.Version != daemonImmutableClusterConfigHostTopologyVersion || !reflect.DeepEqual(stored.CPUProfile, profile) {
		t.Fatalf("stored configuration=%+v", stored)
	}
	// A node that does not choose adopts the cluster's topology.
	join := defaultDaemonClusterOptions()
	if _, err := applyImmutableClusterConfig(root, &join); err != nil {
		t.Fatal(err)
	}
	if join.ResolvedCPU.cpuTopologyMode() != cputopology.Host || join.ResolvedCPU.TopologyFingerprint != profile.TopologyFingerprint {
		t.Fatalf("joining node's profile=%+v", join.ResolvedCPU)
	}
	// One that chooses another is refused.
	other := defaultDaemonClusterOptions()
	other.CPUTopology, other.CPUTopologySet = cputopology.Single, true
	if _, err := applyImmutableClusterConfig(root, &other); err == nil || !strings.Contains(err.Error(), "differs from this cluster's CPU topology") {
		t.Fatalf("a node asking for the single topology: %v", err)
	}
	// A cluster recorded without a choice shows the single topology.
	plain := t.TempDir()
	first := defaultDaemonClusterOptions()
	first.CPUProfileSet = true
	first.ResolvedCPU = daemonCPUProfile{Version: daemonCPUProfileVersion, Mode: "host", Features: []string{"sse", "sse2"}}
	if _, err := applyImmutableClusterConfig(plain, &first); err != nil {
		t.Fatal(err)
	}
	host := defaultDaemonClusterOptions()
	host.CPUTopology, host.CPUTopologySet = cputopology.Host, true
	if _, err := applyImmutableClusterConfig(plain, &host); err == nil || !strings.Contains(err.Error(), "this cluster's CPU topology, single") {
		t.Fatalf("a node asking for the host topology of a single-topology cluster: %v", err)
	}
}

func TestCPUIDFaultingWaiverHoldsNodesToTheRecordedCPU(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("x86 CPUID")
	}
	host, err := resolveDaemonCPUProfile("host", "", "systrap")
	if err != nil {
		t.Fatal(err)
	}
	waived, err := waiveCPUIDFaulting(host, "systrap")
	if err != nil {
		t.Fatal(err)
	}
	if !waived.NoCPUIDFaulting || waived.CPUID != cpuid.HostCPUIDFeatures() || !slices.Equal(waived.Features, host.Features) {
		t.Fatalf("waived profile=%+v", waived)
	}
	if err := validateDaemonCPUProfile(waived, "systrap"); err != nil {
		t.Fatalf("the CPU the cluster records was refused: %v", err)
	}
	if err := validateDaemonCPUProfile(waived, "kvm"); err == nil || !strings.Contains(err.Error(), "systrap only") {
		t.Fatalf("KVM node of a cluster without CPUID faulting: %v", err)
	}
	other := waived
	other.CPUID += ";ffffff.0.eax=1"
	if err := validateDaemonCPUProfile(other, "systrap"); err == nil || !strings.Contains(err.Error(), "ffffff.0.eax is 1 in the cluster, 0 here") {
		t.Fatalf("another CPU: %v", err)
	}
	unrecorded := waived
	unrecorded.CPUID = ""
	if err := validateDaemonCPUProfile(unrecorded, "systrap"); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("waiver without a CPU: %v", err)
	}
	portable, err := resolveDaemonCPUProfile("portable", "", "systrap")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := waiveCPUIDFaulting(portable, "systrap"); err == nil || !strings.Contains(err.Error(), "requires --cpu-profile host") {
		t.Fatalf("portable profile: %v", err)
	}
	if _, err := waiveCPUIDFaulting(host, "kvm"); err == nil || !strings.Contains(err.Error(), "applies to systrap nodes") {
		t.Fatalf("KVM node: %v", err)
	}
}

func TestCPUIDDifferences(t *testing.T) {
	got := cpuidDifferences("vendor=AuthenticAMD;1.0.ecx=1;7.0.ebx=20", "vendor=AuthenticAMD;1.0.ecx=3;7.1.eax=10")
	want := []string{"1.0.ecx is 1 in the cluster, 3 here", "7.0.ebx is 20 in the cluster, 0 here", "7.1.eax is 0 in the cluster, 10 here"}
	if !slices.Equal(got, want) {
		t.Fatalf("differences=%q, want %q", got, want)
	}
}

func TestImmutableClusterConfigWaivesCPUIDFaulting(t *testing.T) {
	root := t.TempDir()
	if waived, err := clusterWaivesCPUIDFaulting(root, defaultDaemonClusterOptions()); err != nil || waived {
		t.Fatalf("a cluster not yet created waives CPUID faulting: (%v, %v)", waived, err)
	}
	profile := daemonCPUProfile{Version: daemonCPUProfileVersion, Mode: "host", Features: []string{"sse", "sse2"}, NoCPUIDFaulting: true, CPUID: "vendor=TestVendor;1.0.edx=1"}
	bootstrap := defaultDaemonClusterOptions()
	bootstrap.CPUProfileSet, bootstrap.NoCPUIDFaulting, bootstrap.ResolvedCPU = true, true, profile
	if pending, err := applyImmutableClusterConfig(root, &bootstrap); err != nil || pending != nil {
		t.Fatalf("creating the cluster=(%+v, %v)", pending, err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".ttc-cluster-config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var stored daemonImmutableClusterConfig
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	// A build that predates the waiver refuses all but version 2.
	if stored.Version != daemonImmutableClusterConfigNoCPUIDFaultingVersion || !reflect.DeepEqual(stored.CPUProfile, profile) {
		t.Fatalf("stored configuration=%+v", stored)
	}
	// A node that does not ask for it adopts it, as it adopts the profile.
	join := defaultDaemonClusterOptions()
	if _, err := applyImmutableClusterConfig(root, &join); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(join.ResolvedCPU, profile) {
		t.Fatalf("joining node's profile=%+v, want %+v", join.ResolvedCPU, profile)
	}
	if waived, err := clusterWaivesCPUIDFaulting(root, defaultDaemonClusterOptions()); err != nil || !waived {
		t.Fatalf("the cluster's waiver was not found: (%v, %v)", waived, err)
	}
	// One that asks for it adopts the CPU the cluster records, which
	// validateDaemonCPUProfile holds it to.
	again := bootstrap
	again.ResolvedCPU.CPUID = "vendor=TestVendor;1.0.edx=3"
	if pending, err := applyImmutableClusterConfig(root, &again); err != nil || pending != nil || !reflect.DeepEqual(again.ResolvedCPU, profile) {
		t.Fatalf("node asking for the waiver=(%+v, %v), profile %+v", pending, err, again.ResolvedCPU)
	}

	// A cluster created requiring CPUID faulting cannot waive it later.
	plain := t.TempDir()
	first := defaultDaemonClusterOptions()
	first.CPUProfileSet = true
	first.ResolvedCPU = daemonCPUProfile{Version: daemonCPUProfileVersion, Mode: "host", Features: []string{"sse", "sse2"}}
	if _, err := applyImmutableClusterConfig(plain, &first); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(plain, ".ttc-cluster-config.json")); err != nil || !strings.Contains(string(data), `"version":2,`) || strings.Contains(string(data), "cpuid") {
		t.Fatalf("configuration requiring CPUID faulting=(%s, %v)", data, err)
	}
	late := bootstrap
	if _, err := applyImmutableClusterConfig(plain, &late); err == nil || !strings.Contains(err.Error(), "applies when a cluster is created") {
		t.Fatalf("waiving CPUID faulting in an existing cluster: %v", err)
	}

	// A configuration whose version does not match its waiver is refused.
	mismatched := t.TempDir()
	stored.Version = daemonImmutableClusterConfigVersion
	if err := writeDurableJSON(filepath.Join(mismatched, ".ttc-cluster-config.json"), stored); err != nil {
		t.Fatal(err)
	}
	reread := defaultDaemonClusterOptions()
	if _, err := applyImmutableClusterConfig(mismatched, &reread); err == nil || !strings.Contains(err.Error(), "unsupported immutable cluster configuration version 2") {
		t.Fatalf("version 2 configuration that waives CPUID faulting: %v", err)
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
	if pending, err := applyImmutableClusterConfig(root, &bootstrap); err != nil || pending != nil {
		t.Fatal(err)
	}
	// A later daemon adopts it, and asks the authority for the policy it
	// asks for, if it differs.
	join := defaultDaemonClusterOptions()
	pending, err := applyImmutableClusterConfig(root, &join)
	if err != nil || pending == nil {
		t.Fatalf("policy change=(%+v, %v), want a pending change", pending, err)
	}
	if join.PagingPolicy != bootstrap.PagingPolicy || join.LeaseDuration != bootstrap.LeaseDuration || join.MaxTransactions != bootstrap.MaxTransactions || join.AutoRebalance != bootstrap.AutoRebalance {
		t.Fatalf("joining policy=%+v, want bootstrap policy=%+v", join, bootstrap)
	}
	same := bootstrap
	if pending, err := applyImmutableClusterConfig(root, &same); err != nil || pending != nil {
		t.Fatalf("same policy=(%+v, %v), want no change", pending, err)
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

// TestNamedCPUProfileAcrossBuilds checks that a daemon that names its root's
// CPU profile adopts it as recorded, though this build resolves the name to
// more features, as a later build that knows more of a host's features does,
// and that an explicit profile still asks for its features.
func TestNamedCPUProfileAcrossBuilds(t *testing.T) {
	current, err := resolveDaemonCPUProfile("host", "", "systrap")
	if err != nil {
		t.Fatal(err)
	}
	if len(current.Features) < 2 {
		t.Skipf("host profile has too few features: %v", current.Features)
	}
	older := current
	older.Features = slices.Clone(current.Features[:len(current.Features)-1])

	root := t.TempDir()
	if err := writeDurableJSON(filepath.Join(root, ".ttc-cpu-profile.json"), older); err != nil {
		t.Fatal(err)
	}
	if adopted, err := applyStandaloneCPUProfile(root, "host", "", "systrap", true); err != nil || !reflect.DeepEqual(adopted, older) {
		t.Fatalf("host profile of an older build=(%+v, %v), want %+v", adopted, err, older)
	}
	if _, err := applyStandaloneCPUProfile(root, "portable", "", "systrap", true); err == nil || !strings.Contains(err.Error(), "differs") {
		t.Fatalf("other named profile error=%v", err)
	}

	clusterRoot := t.TempDir()
	first := defaultDaemonClusterOptions()
	first.CPUProfileSet, first.ResolvedCPU = true, older
	if pending, err := applyImmutableClusterConfig(clusterRoot, &first); err != nil || pending != nil {
		t.Fatalf("creating the cluster=(%+v, %v)", pending, err)
	}
	later := defaultDaemonClusterOptions()
	later.CPUProfileSet, later.ResolvedCPU = true, current
	if pending, err := applyImmutableClusterConfig(clusterRoot, &later); err != nil || pending != nil || !reflect.DeepEqual(later.ResolvedCPU, older) {
		t.Fatalf("a later build's node=(%+v, %v), profile %+v, want %+v", pending, err, later.ResolvedCPU, older)
	}

	if runtime.GOARCH != "amd64" {
		return
	}
	explicit := t.TempDir()
	if _, err := applyStandaloneCPUProfile(explicit, "explicit", "fpu,sse,sse2", "systrap", true); err != nil {
		t.Fatal(err)
	}
	if _, err := applyStandaloneCPUProfile(explicit, "explicit", "sse2,fpu,sse", "systrap", true); err != nil {
		t.Fatalf("the same explicit features in another order: %v", err)
	}
	if _, err := applyStandaloneCPUProfile(explicit, "explicit", "fpu,sse", "systrap", true); err == nil || !strings.Contains(err.Error(), "differs") {
		t.Fatalf("other explicit features error=%v", err)
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

// A cluster's first daemon records its identity, which every later one
// shares, and binds its local root to it.
func TestClusterIdentity(t *testing.T) {
	shared := t.TempDir()
	localA := t.TempDir()
	clusterID, err := prepareClusterIdentity(shared, localA, "repository", "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if clusterID == "" {
		t.Fatal("the first daemon produced an empty cluster ID")
	}
	if retry, err := prepareClusterIdentity(shared, localA, "repository", "node-a"); err != nil || retry != clusterID {
		t.Fatalf("same-root retry=(%q, %v), want %q", retry, err, clusterID)
	}
	if joined, err := prepareClusterIdentity(shared, t.TempDir(), "repository", "node-b"); err != nil || joined != clusterID {
		t.Fatalf("second node's identity=(%q, %v), want %q", joined, err, clusterID)
	}
	if _, err := prepareClusterIdentity(shared, t.TempDir(), "another-repository", "node-c"); err == nil {
		t.Fatal("mismatched repository joined the cluster")
	}
	if _, err := prepareClusterIdentity(t.TempDir(), localA, "repository", "node-a"); err == nil || !strings.Contains(err.Error(), "another cluster") {
		t.Fatalf("a local root of another cluster: %v", err)
	}
}

func TestLocalIncarnationReset(t *testing.T) {
	const (
		node        = "node-a"
		incarnation = "incarnation-a"
	)
	for _, test := range []struct {
		name    string
		state   *cluster.AuthorityState
		want    bool
		wantErr bool
	}{
		{name: "forgotten member", state: cluster.NewAuthorityState(), want: true},
		{name: "expired offline member", state: &cluster.AuthorityState{Nodes: map[string]cluster.NodeRecord{
			node: {NodeID: node, Incarnation: incarnation, State: cluster.NodeOffline},
		}}},
		{name: "live matching member", state: &cluster.AuthorityState{Nodes: map[string]cluster.NodeRecord{
			node: {NodeID: node, Incarnation: incarnation, State: cluster.NodeReady},
		}}},
		{name: "same name has new identity", state: &cluster.AuthorityState{Nodes: map[string]cluster.NodeRecord{
			node: {NodeID: node, Incarnation: "incarnation-new", State: cluster.NodeReady},
		}}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := localIncarnationReset(test.state, node, incarnation)
			if (err != nil) != test.wantErr || got != test.want {
				t.Fatalf("localIncarnationReset()=(%t, %v), want (%t, err=%t)", got, err, test.want, test.wantErr)
			}
		})
	}
}

func TestDiscardRetiredLocalAuthority(t *testing.T) {
	root := t.TempDir()
	removed := []string{
		"catalog", "cluster", "containers", "daemon", "filesystems",
		"graph-references", "objects", "publication-queue",
		"publication-staging", "runtime", "snapshot-graph",
	}
	for _, name := range removed {
		path := filepath.Join(root, name, "stale")
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "state"), []byte("old incarnation"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"images", "future-version-data"} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "keep"), []byte("immutable or unknown"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	if err := discardRetiredLocalAuthority(root); err != nil {
		t.Fatalf("discardRetiredLocalAuthority: %v", err)
	}
	for _, name := range removed {
		if _, err := os.Stat(filepath.Join(root, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("retired directory %q remains: %v", name, err)
		}
	}
	for _, name := range []string{"images", "future-version-data"} {
		if data, err := os.ReadFile(filepath.Join(root, name, "keep")); err != nil || string(data) != "immutable or unknown" {
			t.Errorf("preserved directory %q = %q, %v", name, data, err)
		}
	}
}

func TestAdaptivePrefetchControllerHysteresis(t *testing.T) {
	policy := ttcpolicy.DefaultPolicy()
	// A window that can grow: the default starts at the 2 MiB maximum.
	policy.TransferBytes = 1 << 20
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
	// A window that can grow: the default starts at the 2 MiB maximum.
	policy.TransferBytes = 1 << 20
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
	// A window that can grow: the default starts at the 2 MiB maximum.
	policy.TransferBytes = 1 << 20
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

func TestDecodeRuntimeStateStatusDistinguishesStoppedSandbox(t *testing.T) {
	for _, test := range []struct {
		name    string
		encoded string
		want    string
		wantErr bool
	}{
		{name: "running", encoded: `{"status":"running","pid":123}`, want: "running"},
		{name: "stopped", encoded: `{"status":"stopped","pid":-1}`, want: "stopped"},
		{name: "normalized", encoded: `{"status":" Paused "}`, want: "paused"},
		{name: "missing", encoded: `{"pid":123}`, wantErr: true},
		{name: "malformed", encoded: `{`, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := decodeRuntimeStateStatus([]byte(test.encoded))
			if (err != nil) != test.wantErr {
				t.Fatalf("decodeRuntimeStateStatus(%q)=(%q,%v), want error=%t", test.encoded, got, err, test.wantErr)
			}
			if got != test.want {
				t.Fatalf("decodeRuntimeStateStatus(%q)=%q, want %q", test.encoded, got, test.want)
			}
		})
	}
}

// TestCommandReconciledFamilies checks which families a command's placement
// reconciliation covers: those it resolved if it changes only them, the
// family whose record it removed, or every family, nil, otherwise; and that
// an image command calls for none.
func TestCommandReconciledFamilies(t *testing.T) {
	resolved := &runtimeBackend{resolvedFamilies: &familySet{}}
	resolved.resolvedFamilies.add("family-id")
	for _, test := range []struct {
		args      []string
		want      []string
		reconcile bool
	}{
		{[]string{"pause", "web@main"}, []string{"family-id"}, true},
		{[]string{"rm", "-f", "web@main"}, []string{"family-id"}, true},
		{[]string{"_auto-remove-now", "web@main"}, []string{"family-id"}, true},
		{[]string{"checkpoint", "web@main", "/tmp/checkpoint"}, []string{"family-id"}, true},
		{[]string{"commit", "web@main", "image:tag"}, []string{"family-id"}, true},
		{[]string{"restore", "/tmp/checkpoint", "web2"}, []string{"family-id"}, true},
		{[]string{"_remove-family-record", "removed-id"}, []string{"removed-id"}, true},
		{[]string{"_remove-family-record", "--volumes", "removed-id"}, []string{"removed-id"}, true},
		{[]string{"_remove-family-record"}, nil, true},
		{[]string{"node", "drain", "n1"}, nil, true},
		{[]string{"pull", "alpine"}, nil, false},
		{[]string{"load", "-i", "alpine.tar"}, nil, false},
		{[]string{"rmi", "alpine"}, nil, false},
		{[]string{"image", "rm", "alpine"}, nil, false},
	} {
		got, reconcile := commandReconciledFamilies(test.args, resolved)
		if !slices.Equal(got, test.want) || reconcile != test.reconcile {
			t.Errorf("commandReconciledFamilies(%q) = %q, %t, want %q, %t", test.args, got, reconcile, test.want, test.reconcile)
		}
	}
}

// A command routed to a cluster node is locked by the family its claims
// name, read from the family's record by the node it came through, when it
// names one family; otherwise the node reads the records itself.
func TestClaimedFamily(t *testing.T) {
	claim := func(family, branch string) daemonPlacementClaim {
		return daemonPlacementClaim{FamilyID: family, BranchID: branch}
	}
	for _, test := range []struct {
		names  []string
		claims []daemonPlacementClaim
		want   string
		ok     bool
	}{
		{[]string{"web"}, []daemonPlacementClaim{claim("id-1", "main")}, "id-1", true},
		{[]string{"web", "web"}, []daemonPlacementClaim{claim("id-1", "a"), claim("id-1", "b")}, "id-1", true},
		{[]string{"web"}, nil, "", false},
		{[]string{"web", "db"}, []daemonPlacementClaim{claim("id-1", "main")}, "", false},
		{[]string{"web"}, []daemonPlacementClaim{claim("id-1", "a"), claim("id-2", "b")}, "", false},
		{[]string{"web"}, []daemonPlacementClaim{claim("", "main")}, "", false},
	} {
		if got, ok := claimedFamily(test.names, test.claims); got != test.want || ok != test.ok {
			t.Errorf("claimedFamily(%q, %+v) = (%q, %t), want (%q, %t)", test.names, test.claims, got, ok, test.want, test.ok)
		}
	}
}

// A routed command's first resolution takes what the authority said of the
// branch its claim names, if it resolves that branch; nothing takes it after.
func TestClaimedBranchAuthorityIsTakenOnce(t *testing.T) {
	state := &cluster.AuthorityState{}
	claimed := &claimedBranchAuthority{family: "web", branch: "main", state: state}
	if got := claimed.take("web", "main"); got != state {
		t.Fatalf("the first take of the claimed branch = %p, want %p", got, state)
	}
	if got := claimed.take("web", "main"); got != nil {
		t.Errorf("a second take = %p, want none", got)
	}
	other := &claimedBranchAuthority{family: "web", branch: "main", state: state}
	if got := other.take("web", "a"); got != nil {
		t.Errorf("a take of another branch = %p, want none", got)
	}
	if got := other.take("web", "main"); got != nil {
		t.Errorf("a take after another branch's = %p, want none", got)
	}
	discarded := &claimedBranchAuthority{family: "web", branch: "main", state: state}
	discarded.discard()
	if got := discarded.take("web", "main"); got != nil {
		t.Errorf("a take after a discard = %p, want none", got)
	}
	var none *claimedBranchAuthority
	none.discard()
	if got := none.take("web", "main"); got != nil {
		t.Errorf("a take of nothing = %p, want none", got)
	}
}

// A command routed by a placement read with its owner's node record is sent
// to the peer made from that record, which is checked as a lookup of it alone
// is; with no record read, the owner is looked up.
func TestRoutedPeerChecksTheRecordReadWithThePlacement(t *testing.T) {
	source := &distributedSourceRuntime{
		capabilities: cluster.Capabilities{NodeID: "n1", RepositoryID: "repository", BuildID: "build"},
		dynamicPeers: make(map[string]dynamicClusterPeer), authToken: "test-cluster-token",
	}
	server := &daemonServer{distributedSource: source}
	live := cluster.NodeRecord{
		NodeID: "n2", Incarnation: "i2", State: cluster.NodeReady, ControlURL: "http://127.0.0.1:1",
		Capabilities:   cluster.Capabilities{NodeID: "n2", RepositoryID: "repository", BuildID: "build"},
		LeaseExpiresAt: time.Now().Add(time.Minute),
	}
	routing := &routingPlacements{server: server, nodes: map[string]cluster.NodeRecord{"n2": live}}
	if peer, err := server.routedPeer("n2", routing); err != nil || peer == nil {
		t.Fatalf("routedPeer of a live owner = %v, %v", peer, err)
	}
	expired := live
	expired.LeaseExpiresAt = time.Now().Add(-time.Second)
	routing.nodes["n2"] = expired
	if _, err := server.routedPeer("n2", routing); err == nil {
		t.Error("a command was routed to an owner whose lease expired")
	}
	other := live
	other.Capabilities.BuildID = "another build"
	routing.nodes["n2"] = other
	if _, err := server.routedPeer("n2", routing); err == nil {
		t.Error("a command was routed to an owner of another build")
	}
	if _, err := server.routedPeer("n3", routing); err == nil {
		t.Error("a command was routed to an owner neither read nor looked up")
	}
	var hints routeHints
	hints.set("family", "n2")
	if got := hints.get("family"); got != "n2" {
		t.Errorf("hint = %q, want n2", got)
	}
	for i := 0; i < maxRouteHints; i++ {
		hints.set(fmt.Sprint(i), "n3")
	}
	if len(hints.owner) > maxRouteHints {
		t.Errorf("%d hints kept, want at most %d", len(hints.owner), maxRouteHints)
	}
}

// Routing by the authority's placements asks the authority of a branch being
// made whether a command may take it, as routing by the catalog does: a
// family whose rehydration failed is removed so.
func TestRoutingPlacementsKeepTheBranchAuthority(t *testing.T) {
	catalog := branchAwarePlacementCatalog{branches: map[string]cluster.BranchAuthority{
		"family\x00main": {FamilyID: "family", BranchID: "main", Lifecycle: "materializing"},
	}}
	var placements cluster.PlacementCatalog = &routingPlacements{PlacementCatalog: catalog}
	authority, ok := placements.(interface {
		Branch(family, branch string) (*cluster.BranchAuthority, error)
	})
	if !ok {
		t.Fatal("routing placements have no branch authorities")
	}
	if branch, err := authority.Branch("family", "main"); err != nil || branch.Lifecycle != "materializing" {
		t.Errorf("Branch = %+v, %v; want the catalog's, materializing", branch, err)
	}
	if _, err := (&routingPlacements{}).Branch("family", "main"); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("Branch of placements without authorities: %v, want %v", err, errors.ErrUnsupported)
	}
}

// A request that makes a family beside other commands reserves the family's
// name, refusing one being made, and takes the new family's lock once it has
// an ID; one that would change more than its family takes the lifecycle lock
// alone.
// stats and diff read one branch beside the commands on every other family:
// they share the lifecycle lock, and take no family's lock.
func TestBranchReadersShareTheLifecycleLock(t *testing.T) {
	server := &daemonServer{}
	backend := &runtimeBackend{lockFamily: func(string) func() { return func() {} }}
	for _, args := range [][]string{{"stats", "demo@main"}, {"container", "stats", "--json", "demo@main"}, {"diff", "demo@b1"}} {
		if families, shared := server.requestFamilies(backend, args, nil); len(families) != 0 || !shared {
			t.Errorf("%q takes the locks of %q, the lifecycle lock shared %t: want none, and shared", args, families, shared)
		}
	}
	if _, shared := server.requestFamilies(backend, []string{"ps", "--size"}, nil); shared {
		t.Error("ps --size, which sizes every family, shares the lifecycle lock")
	}
}

func TestFamilyMakingReservesItsNameAndClaimsItsLock(t *testing.T) {
	server := &daemonServer{}
	root := t.TempDir()
	var released []string
	shared := server.lockDataRootShared(root)
	hold := &lifecycleHold{acquire: func() func() { return server.lockDataRootShared(root) }, release: shared}
	exclusive := 0
	making := &familyMaking{
		server: server, dataRoot: root, hold: hold,
		exclusive: func() func() {
			exclusive++
			return server.lockDataRoot(root)
		},
		holding: func(release func()) {
			released = append(released, "family")
			// An attached run releases its locks twice: once its family
			// runs, and once it is done.
			release()
			release()
		},
	}
	done, err := making.begin(runRequest{name: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := making.begin(runRequest{name: "web"}); err == nil {
		t.Error("a second family named web was made at once")
	}
	if other, err := making.begin(runRequest{name: "db"}); err != nil {
		t.Errorf("a family of another name: %v", err)
	} else {
		other()
	}
	done()
	if again, err := making.begin(runRequest{name: "web"}); err != nil {
		t.Errorf("web once made: %v", err)
	} else {
		again()
	}
	b := &runtimeBackend{}
	if err := making.claim(b, "id-1"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(b.heldFamilies, "id-1") || len(released) != 1 {
		t.Errorf("claim held %v, released %v", b.heldFamilies, released)
	}
	lock := server.familyLock(root, "id-1")
	select {
	case lock <- struct{}{}:
		<-lock
	default:
		t.Error("the claimed family's lock was not given back")
	}
	if exclusive != 0 {
		t.Fatalf("a family changing only itself took the lifecycle lock alone")
	}
	if _, err := making.begin(runRequest{name: "vol", managedVolumes: []ttcbundle.ManagedVolume{{Name: "data", Destination: "/data"}}}); err != nil {
		t.Fatal(err)
	}
	if exclusive != 1 {
		t.Errorf("a family attaching a named volume took the lifecycle lock alone %d times, want once", exclusive)
	}
	hold.unlock()
	for _, request := range []runRequest{
		{publishedPorts: []ttcbundle.PortBinding{{HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}}},
		{gpu: ttcbundle.GPUOptions{NVProxy: true}},
	} {
		if request.makesOnlyItsFamily() {
			t.Errorf("%+v makes only its family", request)
		}
	}
	if !(runRequest{managedVolumes: []ttcbundle.ManagedVolume{{Destination: "/anonymous"}}}).makesOnlyItsFamily() {
		t.Error("an anonymous volume is more than the family's own")
	}
}

func TestRemoveMovedBranchEntry(t *testing.T) {
	root := t.TempDir()
	write := func(info ttcbundle.ContainerInfo) {
		t.Helper()
		dir := filepath.Join(root, "containers", info.ID)
		info.BundleDir = filepath.Join(dir, "bundle")
		if err := os.MkdirAll(info.BundleDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(info.BundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := writeCurrentBundleFixture(filepath.Join(dir, "container.json"), info); err != nil {
			t.Fatal(err)
		}
	}
	// The family's record keeps the entry its migration left of mv, and mv's
	// fork, beside main; the migration's record holds the branch itself.
	write(ttcbundle.ContainerInfo{ID: "family", Name: "family", Branches: []ttcbundle.BranchInfo{
		{ID: "main", State: "running", NodeID: "n1", FilesystemBranch: "main"},
		{ID: "mv", ParentID: "main", State: "running", NodeID: "n1", FilesystemBranch: "b-mv"},
		{ID: "child", ParentID: "mv", State: "running", NodeID: "n1", FilesystemBranch: "b-child"},
	}})
	write(ttcbundle.ContainerInfo{
		ID: "fork-1", Name: "fork-1", DistributedFamilyID: "family", DistributedForkTransactionID: "fork-1",
		DistributedPhysicalRuntime: true,
		Branches:                   []ttcbundle.BranchInfo{{ID: "fork-1", ParentID: "main", State: "running", NodeID: "n2", FilesystemBranch: "b-1"}},
	})
	b := &runtimeBackend{dataRoot: root}
	if err := b.removeMovedBranchEntry("family", "mv"); err != nil {
		t.Fatal(err)
	}
	store := ttcbundle.Store{Root: root}
	family, err := store.LoadRuntimeContainer("family")
	if err != nil {
		t.Fatal(err)
	}
	var branches []string
	for _, branch := range family.Branches {
		branches = append(branches, branch.ID+"<"+branch.ParentID)
	}
	if want := []string{"main<", "child<main"}; !slices.Equal(branches, want) {
		t.Fatalf("family branches after removal = %v, want %v", branches, want)
	}
	// Again, and of a physical runtime's record: nothing to remove.
	if err := b.removeMovedBranchEntry("family", "mv"); err != nil {
		t.Fatal(err)
	}
	if err := b.removeMovedBranchEntry("fork-1", "fork-1"); err != nil {
		t.Fatal(err)
	}
	if physical, err := store.LoadRuntimeContainer("fork-1"); err != nil || len(physical.Branches) != 1 {
		t.Fatalf("physical runtime record = %+v, %v; want its branch kept", physical, err)
	}
	if err := b.removeMovedBranchEntry("gone", "mv"); err != nil {
		t.Fatal(err)
	}
}

// heldPublisher keeps every intent pending: its capture is never available.
type heldPublisher struct{}

func (heldPublisher) CaptureAvailable(ttcstorage.Intent) (bool, error) { return false, nil }
func (heldPublisher) Publish(context.Context, ttcstorage.Intent) (ttcstorage.Result, error) {
	return ttcstorage.Result{}, errors.New("held")
}
func (heldPublisher) Complete(ttcstorage.Intent, ttcstorage.Result) error { return nil }
func (heldPublisher) Failed(ttcstorage.Intent, error, time.Time) error    { return nil }
func (heldPublisher) Cleanup(ttcstorage.Intent) error                     { return nil }

// A migration from a runtime that a dehydration has just released leaves
// the runtime's node-local state, whose capture the dehydration's publication
// reads until it is done, and the state of a branch that needs the runtime.
// A journaled command whose first resolution found its branch on another
// node goes there, routed as the branch is placed now, under its request ID,
// and that node's answer is the command's.
func TestForwardMovedCommandSendsItToTheBranchsNode(t *testing.T) {
	root := t.TempDir()
	bundleDir := filepath.Join(root, "containers", "web", "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentBundleFixture(filepath.Join(root, "containers", "web", "container.json"), ttcbundle.ContainerInfo{ID: "web", Name: "web", NodeID: "node-b", BundleDir: bundleDir}); err != nil {
		t.Fatal(err)
	}
	placements, err := cluster.OpenPlacementStore(filepath.Join(root, "cluster", "placements"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := placements.Create(cluster.PlacementRecord{
		FamilyID: "web", BranchID: "main", PhysicalContainer: "web", PhysicalBranch: "main",
		OwnerNode: "node-b", OwnerIncarnation: "boot-b", State: cluster.PlacementRunning,
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	var received daemonRequest
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/daemon" {
			http.NotFound(w, r)
			return
		}
		line, _ := bufio.NewReader(r.Body).ReadBytes('\n')
		_ = json.Unmarshal(line, &received)
		var frames bytes.Buffer
		var mu sync.Mutex
		_ = writeDaemonFrame(&frames, &mu, daemonFrameStdout, []byte("web@main\n"))
		_ = writeDaemonStatus(&frames, &mu, 0)
		_, _ = w.Write(frames.Bytes())
	}))
	defer server.Close()
	peer, err := cluster.NewClient(server.URL, server.Client().Transport.(*http.Transport).TLSClientConfig, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	s := &daemonServer{
		dataRoot: root, clusterPlacements: placements, clusterCapabilities: &cluster.Capabilities{NodeID: "node-a"},
		distributedSource: &distributedSourceRuntime{peers: map[string]*cluster.Client{"node-b": peer}},
	}
	status, stdout, _, err := s.forwardMovedCommand(daemonRequest{Version: daemonAPIVersion, Action: "command", RequestID: "request-1", Args: []string{"pause", "web@main"}, Route: []string{"gw"}})
	if err != nil || status != 0 || stdout != "web@main\n" {
		t.Fatalf("forwardMovedCommand = %d, %q, %v; want node-b's answer", status, stdout, err)
	}
	if received.RequestID != "request-1" || !reflect.DeepEqual(received.Route, []string{"gw", "node-a"}) ||
		len(received.PlacementClaims) != 1 || received.PlacementClaims[0].OwnerNode != "node-b" {
		t.Fatalf("node-b received %+v, want the command under its request ID, routed by its claim", received)
	}
	// A node the command came through is not sent it again.
	if _, _, _, err := s.forwardMovedCommand(daemonRequest{Version: daemonAPIVersion, Action: "command", RequestID: "request-2", Args: []string{"pause", "web@main"}, Route: []string{"node-b"}}); err == nil {
		t.Fatal("forwardMovedCommand sent a command back to a node it came through")
	}
}

// Only a command's first resolution of a branch, before the command did
// anything, can leave it undone for its daemon to send on.
func TestCommandMovesNotesTheFirstResolution(t *testing.T) {
	moved := &remoteBranchOwnershipError{target: selector{container: "web", branch: "main"}, owner: "node-b"}
	var first commandMoves
	first.note(fmt.Errorf("resolving: %w", moved))
	if first.movedTo() != "node-b" {
		t.Fatalf("movedTo = %q after a first resolution that found the branch on node-b", first.movedTo())
	}
	var later commandMoves
	later.note(nil)
	later.note(moved)
	if later.movedTo() != "" {
		t.Fatalf("movedTo = %q after a resolution that followed another", later.movedTo())
	}
	var none *commandMoves
	none.note(moved)
	if none.movedTo() != "" {
		t.Fatal("a command without a record of moves recorded one")
	}
	// A routed command that is not journaled is refused as routed stale.
	refused := &runtimeBackend{moves: &commandMoves{refuse: true}}
	if refused.refusedAsMoved() {
		t.Fatal("a command was refused before it found its branch moved")
	}
	refused.moves.note(moved)
	if !refused.refusedAsMoved() {
		t.Fatal("a routed command whose branch moved was not refused as routed stale")
	}
	var status bytes.Buffer
	if code := runCommand([]string{"pause", "web@main"}, io.Discard, &status, &movedPauseBackend{runtimeBackend: refused}); code != 1 || !strings.HasPrefix(status.String(), protocol.StaleRouteRefusal) {
		t.Fatalf("runCommand = %d, %q; want the stale-route refusal", code, status.String())
	}
	if got := branchSelectorCount([]string{"exec", "-t", "web@main", "echo", "a"}); got != 1 {
		t.Fatalf("branchSelectorCount = %d, want 1", got)
	}
	for _, test := range []struct {
		args    []string
		archive bool
		want    bool
	}{
		{args: []string{"pause", "fc@main"}, want: true},
		{args: []string{"fork", "--node", "n2", "fc@main", "child"}, want: true},
		{args: []string{"migrate", "--node", "n3", "fc@c4n11"}, want: true},
		{args: []string{"stop", "fc@a", "fc@b"}},
		{args: []string{"rm", "fc"}},
		{args: []string{"cp", "fc@main:/x", "/tmp/x"}},
		{args: []string{"exec", "fc@main", "true"}},
		{args: []string{"pause", "fc@main"}, archive: true},
	} {
		if got := movableDaemonCommand(daemonRequest{Args: test.args}, test.archive); got != test.want {
			t.Errorf("movableDaemonCommand(%q, %t) = %t, want %t", test.args, test.archive, got, test.want)
		}
	}
}

// movedPauseBackend fails every pause on its branch's move to node-b.
type movedPauseBackend struct {
	*runtimeBackend
}

func (b *movedPauseBackend) pause(target selector) error {
	return &remoteBranchOwnershipError{target: target, owner: "node-b"}
}

// A command making a branch in a migration's source runtime keeps the
// runtime, and its node-local state, from the source's release: only the
// source's branch goes. A stop or a removal of the migrated branch, which
// releases the source at once, failed on a release that refused instead.
func TestMigratedSourceReleaseKeepsARuntimeBeingMade(t *testing.T) {
	calls := filepath.Join(t.TempDir(), "calls")
	runsc := filepath.Join(t.TempDir(), "runsc")
	script := "#!/bin/sh\necho \"$@\" >> " + calls + "\n" +
		"[ \"$3\" = list ] && { echo '[{\"id\":\"fork-0123\",\"status\":\"paused\"}]'; exit 0; }\n" +
		"[ \"$5\" = destroy ] && exit 0\nexit 2\n"
	if err := os.WriteFile(runsc, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	local := t.TempDir()
	state := filepath.Join(local, "containers", "fork-0123")
	if err := os.MkdirAll(state, 0700); err != nil {
		t.Fatal(err)
	}
	var operations familyOperations
	made := operations.beginMaking("fork-0123", "child")
	defer made()
	backend := &runtimeBackend{root: filepath.Join(t.TempDir(), "runsc-root"), runsc: runsc, dataRoot: t.TempDir(), localRoot: local,
		nodeID: "node-a", operations: &operations, stdin: strings.NewReader(""), stdout: io.Discard, stderr: io.Discard}
	if err := backend.removeUnrecordedMigratedSourceRuntime("fork-0123", "moved", nil); err != nil {
		t.Fatalf("removeUnrecordedMigratedSourceRuntime: %v", err)
	}
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "timetravel fork-0123 destroy moved") || strings.Contains(string(data), "delete") {
		t.Fatalf("runtime calls %q, want the source's branch destroyed and the runtime kept", data)
	}
	if _, err := os.Stat(state); err != nil {
		t.Fatalf("the runtime's node-local state went: %v", err)
	}
}

func TestUnrecordedMigratedSourceKeepsWhatItsBranchesNeed(t *testing.T) {
	for _, test := range []struct {
		name    string
		state   string
		pending bool
		kept    bool
	}{
		{name: "a publication pending", state: "dehydrated", pending: true, kept: true},
		{name: "nothing pending", state: "dehydrated"},
		{name: "a branch that needs the runtime", state: "dehydrating", kept: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			runsc := filepath.Join(dir, "runsc")
			script := "#!/bin/sh\ncase \"$3\" in\nlist) echo '[]' ;;\n*) echo \"unexpected command: $*\" >&2; exit 2 ;;\nesac\n"
			if err := os.WriteFile(runsc, []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			queue, err := ttcstorage.Open(filepath.Join(dir, "queue"), heldPublisher{})
			if err != nil {
				t.Fatal(err)
			}
			defer queue.Close()
			if test.pending {
				if err := queue.Enqueue(ttcstorage.Intent{Container: "fork-1", Branch: "x", PublicationID: "publication-1", Kind: "dehydrate", FilesystemBranch: "b-x"}); err != nil {
					t.Fatal(err)
				}
			}
			b := &runtimeBackend{root: "/runtime", runsc: runsc, dataRoot: filepath.Join(dir, "shared"), localRoot: filepath.Join(dir, "local"),
				operations: &familyOperations{}, publicationQueue: queue, nodeID: "n3"}
			capture := filepath.Join(dir, "local", "containers", "fork-1", "capture")
			if err := os.MkdirAll(filepath.Dir(capture), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(capture, []byte("pages"), 0600); err != nil {
				t.Fatal(err)
			}
			info := ttcbundle.ContainerInfo{
				ID: "fork-1", Name: "fork-1", DistributedFamilyID: "family", DistributedPhysicalRuntime: true,
				Branches: []ttcbundle.BranchInfo{{ID: "x", State: test.state, NodeID: "n3"}},
			}
			if err := b.removeUnrecordedMigratedSourceRuntime("fork-1", "y", &info); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(capture); (err == nil) != test.kept {
				t.Fatalf("the runtime's node-local state kept: %t, want %t", err == nil, test.kept)
			}
		})
	}
}
