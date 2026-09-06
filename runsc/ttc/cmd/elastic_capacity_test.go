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
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gvisor.dev/gvisor/runsc/ttc/cluster"
)

func TestCappedCapacityOutputDoesNotPadShortWrites(t *testing.T) {
	var output cappedCapacityOutput
	buffer := make([]byte, elasticCapacityErrorLimit)
	copy(buffer, "short diagnostic")
	written, err := output.Write(buffer[:len("short diagnostic")])
	if err != nil || written != len("short diagnostic") || output.String() != "short diagnostic" {
		t.Fatalf("Write()=(%d, %v), output=%q", written, err, output.String())
	}
	written, err = output.Write(make([]byte, elasticCapacityErrorLimit*2))
	if err != nil || written != elasticCapacityErrorLimit*2 || len(output.String()) != elasticCapacityErrorLimit {
		t.Fatalf("capped Write()=(%d, %v), output length=%d", written, err, len(output.String()))
	}
}

func testElasticCapacityCapabilities(node string) cluster.Capabilities {
	return cluster.Capabilities{
		NodeID: node, ProtocolMin: 1, ProtocolMax: 1, BuildID: "test", Architecture: "amd64",
		BasePageSize: 4096, MaxTransferSize: 2 << 20, RepositoryID: "shared",
		Features: []string{cluster.FeatureRemotePull, cluster.FeatureNetworkRecreate, cluster.FeatureFilesystemBranch, cluster.FeatureSharedMemorySnapshot},
	}
}

func testElasticCapacityRecord(node string, current, target uint64) cluster.NodeRecord {
	return cluster.NodeRecord{
		NodeID: node, Incarnation: "boot-" + node,
		ControlURL: "https://" + node + ".example:7443", ControlServerName: node + ".example",
		Capabilities: testElasticCapacityCapabilities(node), MemoryCurrentBytes: current, MemoryTargetBytes: target,
	}
}

func TestElasticCapacityCommandSink(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "request.json")
	command := filepath.Join(dir, "provider")
	script := "#!/bin/sh\n[ \"$TTC_CAPACITY_REQUEST_ID\" = capacity-test ] || exit 4\ncat > \"$1\"\n"
	if err := os.WriteFile(command, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	// Command paths are intentionally shell-free and therefore take no command
	// arguments. Use a wrapper with the destination embedded for this fixture.
	script = "#!/bin/sh\n[ \"$TTC_CAPACITY_REQUEST_ID\" = capacity-test ] || exit 4\ncat > " + output + "\n"
	if err := os.WriteFile(command, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	sink, err := newElasticCapacitySink(command, "")
	if err != nil {
		t.Fatal(err)
	}
	request := elasticCapacityRequest{Version: 1, RequestID: "capacity-test", SourceNode: "node-a"}
	if err := sink.Deliver(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var decoded elasticCapacityRequest
	if err := json.Unmarshal(contents, &decoded); err != nil || decoded.RequestID != request.RequestID {
		t.Fatalf("decoded request=(%+v, %v)", decoded, err)
	}
}

func TestElasticCapacityWebhookSink(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls++
		if request.Method != http.MethodPost || request.Header.Get("Idempotency-Key") != "capacity-webhook" || request.Header.Get("Content-Type") != "application/json" {
			t.Errorf("request headers=%v method=%s", request.Header, request.Method)
		}
		body, _ := io.ReadAll(request.Body)
		if !strings.Contains(string(body), `"request_id":"capacity-webhook"`) {
			t.Errorf("request body=%q", body)
		}
		if calls == 1 {
			http.Error(w, "retry me", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	sink, err := newElasticCapacitySink("", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	request := elasticCapacityRequest{RequestID: "capacity-webhook"}
	if err := sink.Deliver(context.Background(), request); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("first delivery error=%v, want 503", err)
	}
	if err := sink.Deliver(context.Background(), request); err != nil {
		t.Fatal(err)
	}
}

type testElasticCapacitySink struct {
	calls int
	err   error
}

func (s *testElasticCapacitySink) Configured() bool { return true }
func (s *testElasticCapacitySink) Deliver(context.Context, elasticCapacityRequest) error {
	s.calls++
	return s.err
}

func TestElasticCapacityRequestRetriesAndWaitsForCompatibleCapacity(t *testing.T) {
	dataRoot := t.TempDir()
	directory, err := cluster.OpenNodeDirectory(filepath.Join(dataRoot, "cluster", "nodes"))
	if err != nil {
		t.Fatal(err)
	}
	created := time.Unix(10_000, 0).UTC()
	source, err := directory.Register(testElasticCapacityRecord("node-a", 7<<30, 8<<30), created.Add(-time.Minute), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := directory.Transition(source.NodeID, source.Incarnation, cluster.NodeReady, created.Add(-time.Minute+time.Second), 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	backend := &runtimeBackend{
		dataRoot:          dataRoot,
		distributedSource: &distributedSourceRuntime{capabilities: testElasticCapacityCapabilities("node-a")},
	}
	path, err := backend.emitCapacityRequest(*source, "memory-pressure")
	if err != nil {
		t.Fatal(err)
	}
	// Replace wall-clock creation with deterministic directory time.
	if err := withElasticCapacityRequest(dataRoot, "node-a", func(path string, request *elasticCapacityRequest) error {
		request.CreatedAt = created
		request.Deadline = created.Add(elasticCapacityAcceptanceLease)
		return writeDurableJSON(path, request)
	}); err != nil {
		t.Fatal(err)
	}
	sink := &testElasticCapacitySink{err: errors.New("controller unavailable")}
	if consumed, err := processElasticCapacityRequest(context.Background(), dataRoot, "node-a", directory, sink, created.Add(time.Second)); consumed || err == nil {
		t.Fatalf("failed attempt=(consumed=%t, err=%v)", consumed, err)
	}
	request, err := loadElasticCapacityRequest(dataRoot, "node-a")
	if err != nil || request.Attempts != 1 || request.LastError == "" || request.NextAttemptAt == nil {
		t.Fatalf("failed request=(%+v, %v)", request, err)
	}
	if _, err := processElasticCapacityRequest(context.Background(), dataRoot, "node-a", directory, sink, created.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if sink.calls != 1 {
		t.Fatalf("backoff made %d calls, want 1", sink.calls)
	}
	sink.err = nil
	acceptedAt := request.NextAttemptAt.Add(time.Millisecond)
	if _, err := processElasticCapacityRequest(context.Background(), dataRoot, "node-a", directory, sink, acceptedAt); err != nil {
		t.Fatal(err)
	}
	request, err = loadElasticCapacityRequest(dataRoot, "node-a")
	if err != nil || request.AcceptedAt == nil || request.Attempts != 2 {
		t.Fatalf("accepted request=(%+v, %v)", request, err)
	}
	// An incompatible joining node neither fulfills nor redelivers an accepted
	// request before its acceptance lease expires.
	targetRecord := testElasticCapacityRecord("node-b", 1<<30, 16<<30)
	targetRecord.Capabilities.Architecture = "arm64"
	target, err := directory.Register(targetRecord, acceptedAt.Add(time.Second), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := directory.Transition(target.NodeID, target.Incarnation, cluster.NodeReady, acceptedAt.Add(2*time.Second), 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if consumed, err := processElasticCapacityRequest(context.Background(), dataRoot, "node-a", directory, sink, acceptedAt.Add(3*time.Second)); consumed || err != nil {
		t.Fatalf("incompatible capacity=(consumed=%t, err=%v)", consumed, err)
	}
	// A compatible node with enough unreserved capacity is authoritative
	// fulfillment, after which TTC (not the provider) removes the outbox entry.
	targetRecord = testElasticCapacityRecord("node-c", 1<<30, 16<<30)
	target, err = directory.Register(targetRecord, acceptedAt.Add(4*time.Second), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := directory.Transition(target.NodeID, target.Incarnation, cluster.NodeReady, acceptedAt.Add(5*time.Second), 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if consumed, err := processElasticCapacityRequest(context.Background(), dataRoot, "node-a", directory, sink, acceptedAt.Add(6*time.Second)); !consumed || err != nil {
		t.Fatalf("compatible capacity=(consumed=%t, err=%v)", consumed, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("capacity request remains after fulfillment: %v", err)
	}
}

func TestElasticCapacityAttemptClampsStaleTimerTimestamp(t *testing.T) {
	dataRoot := t.TempDir()
	directory, err := cluster.OpenNodeDirectory(filepath.Join(dataRoot, "cluster", "nodes"))
	if err != nil {
		t.Fatal(err)
	}
	created := time.Unix(20_000, 1234).UTC()
	source, err := directory.Register(testElasticCapacityRecord("node-a", 7<<30, 8<<30), created.Add(-time.Minute), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := directory.Transition(source.NodeID, source.Incarnation, cluster.NodeReady, created.Add(-time.Minute+time.Second), 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	request := elasticCapacityRequest{
		Version: 1, RequestID: "capacity-race", Operation: "acquire", Nodes: 1,
		MemoryBytesEach: 8 << 30, Architecture: "amd64", RepositoryID: "shared",
		SourceNode: "node-a", Reason: "node-drain", CreatedAt: created,
		Deadline: created.Add(elasticCapacityAcceptanceLease),
	}
	if err := withElasticCapacityRequest(dataRoot, "node-a", func(path string, current *elasticCapacityRequest) error {
		*current = request
		return writeDurableJSON(path, current)
	}); err != nil {
		t.Fatal(err)
	}
	sink := &testElasticCapacitySink{err: errors.New("injected outage")}
	if _, err := processElasticCapacityRequest(context.Background(), dataRoot, "node-a", directory, sink, created.Add(-time.Millisecond)); err == nil {
		t.Fatal("failed delivery unexpectedly succeeded")
	}
	stored, err := loadElasticCapacityRequest(dataRoot, "node-a")
	if err != nil {
		t.Fatalf("stale timer produced an invalid durable request: %v", err)
	}
	if stored.LastAttemptAt == nil || stored.LastAttemptAt.Before(stored.CreatedAt) || stored.NextAttemptAt == nil || stored.NextAttemptAt.Before(stored.CreatedAt) {
		t.Fatalf("invalid clamped delivery timestamps: %+v", stored)
	}
}

func TestElasticCapacitySinkConfigurationValidation(t *testing.T) {
	if _, err := newElasticCapacitySink("relative", ""); err == nil {
		t.Error("relative command was accepted")
	}
	if _, err := newElasticCapacitySink("", "file:///tmp/provider"); err == nil {
		t.Error("file webhook was accepted")
	}
	if _, err := newElasticCapacitySink("/bin/true", "https://provider.example"); err == nil {
		t.Error("two capacity providers were accepted")
	}
}
