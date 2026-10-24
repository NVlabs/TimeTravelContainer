// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testClusterToken authenticates the tests' cluster peers.
const testClusterToken = "test-cluster-token"

type testDaemonProxy struct {
	delay time.Duration
}

func (p testDaemonProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if p.delay != 0 {
		time.Sleep(p.delay)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append([]byte("proxied:"), data...))
}

func TestProxyDaemonUsesDedicatedNonReusedTransport(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "transactions"))
	if err != nil {
		t.Fatal(err)
	}
	control := &ControlPlane{
		AuthToken: testClusterToken, Store: store, MaxTransactions: 1, Now: time.Now,
		CommandProxy: testDaemonProxy{delay: 10 * time.Millisecond},
	}
	server := httptest.NewServer(control)
	defer server.Close()
	client, err := NewPlainClient(server.URL, "source", testClusterToken, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.daemonHTTP.Transport.(*http.Transport)
	if !ok || !transport.DisableKeepAlives {
		t.Fatalf("daemon transport=%#v, want dedicated transport without keep-alives", client.daemonHTTP.Transport)
	}
	for _, input := range []string{"first\n", "second\n"} {
		var output bytes.Buffer
		if err := client.ProxyDaemon(context.Background(), strings.NewReader(input), &output); err != nil {
			t.Fatalf("ProxyDaemon(%q): %v", input, err)
		}
		if got, want := output.String(), "proxied:"+input; got != want {
			t.Fatalf("ProxyDaemon(%q) output=%q, want %q", input, got, want)
		}
	}
}

func TestProxyDaemonIsFullDuplexBeforeUploadEOF(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "transactions"))
	if err != nil {
		t.Fatal(err)
	}
	control := &ControlPlane{
		AuthToken: testClusterToken, Store: store, MaxTransactions: 1, Now: time.Now,
		CommandProxy: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			controller := http.NewResponseController(w)
			if err := controller.EnableFullDuplex(); err != nil {
				t.Errorf("EnableFullDuplex: %v", err)
				return
			}
			request := make([]byte, 4)
			if _, err := io.ReadFull(r.Body, request); err != nil {
				t.Errorf("reading request: %v", err)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.ttc.daemon-stream")
			_, _ = w.Write(bytes.ToUpper(request))
			w.(http.Flusher).Flush()
		}),
	}
	server := httptest.NewServer(control)
	defer server.Close()
	client, err := NewPlainClient(server.URL, "source", testClusterToken, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	application, tunnel := net.Pipe()
	defer application.Close()
	done := make(chan error, 1)
	go func() {
		done <- client.ProxyDaemon(context.Background(), tunnel, tunnel)
		_ = tunnel.Close()
	}()
	if _, err := application.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if err := application.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, 4)
	if _, err := io.ReadFull(application, response); err != nil {
		t.Fatalf("reading response before upload EOF: %v", err)
	}
	if got := string(response); got != "PING" {
		t.Fatalf("response=%q", got)
	}
	_ = application.Close()
	if err := <-done; err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
}

func TestRenewExecutionAuthorityRunsOnTarget(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "transactions"))
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	control := &ControlPlane{
		AuthToken: testClusterToken, Capabilities: testCapabilities("target"), Store: store, MaxTransactions: 1, Now: time.Now,
		RenewExecution: func(context.Context) error {
			calls.Add(1)
			return nil
		},
	}
	server := httptest.NewServer(control)
	defer server.Close()
	client, err := NewPlainClient(server.URL, "source", testClusterToken, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.RenewExecutionAuthority(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("renew calls=%d, want 1", got)
	}
}

func TestProxyPortCarriesBidirectionalStream(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "transactions"))
	if err != nil {
		t.Fatal(err)
	}
	control := &ControlPlane{
		AuthToken: testClusterToken, Store: store, MaxTransactions: 1, Now: time.Now,
		PortProxy: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := r.URL.Query().Get("family") + "@" + r.URL.Query().Get("branch") + ":" + r.URL.Query().Get("port"); got != "web@main:8080" {
				t.Errorf("port target=%q", got)
			}
			payload, _ := io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/vnd.ttc.port-stream")
			_, _ = w.Write(append([]byte("guest:"), payload...))
		}),
	}
	server := httptest.NewServer(control)
	defer server.Close()
	client, err := NewPlainClient(server.URL, "source", testClusterToken, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := client.ProxyPort(context.Background(), "web", "main", 8080, strings.NewReader("hello"), &output); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != "guest:hello" {
		t.Fatalf("port response=%q", got)
	}
}

func TestProxyPortIsFullDuplexBeforeUploadEOF(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "transactions"))
	if err != nil {
		t.Fatal(err)
	}
	control := &ControlPlane{
		AuthToken: testClusterToken, Store: store, MaxTransactions: 1, Now: time.Now,
		PortProxy: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			controller := http.NewResponseController(w)
			if err := controller.EnableFullDuplex(); err != nil {
				t.Errorf("EnableFullDuplex: %v", err)
				return
			}
			request := make([]byte, 4)
			if _, err := io.ReadFull(r.Body, request); err != nil {
				t.Errorf("reading request: %v", err)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.ttc.port-stream")
			_, _ = w.Write(bytes.ToUpper(request))
			w.(http.Flusher).Flush()
		}),
	}
	server := httptest.NewServer(control)
	defer server.Close()
	client, err := NewPlainClient(server.URL, "source", testClusterToken, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	application, tunnel := net.Pipe()
	defer application.Close()
	done := make(chan error, 1)
	go func() {
		done <- client.ProxyPort(context.Background(), "web", "main", 8080, tunnel, tunnel)
		_ = tunnel.Close()
	}()
	if _, err := application.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if err := application.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, 4)
	if _, err := io.ReadFull(application, response); err != nil {
		t.Fatalf("reading response before upload EOF: %v", err)
	}
	if got := string(response); got != "PING" {
		t.Fatalf("response=%q", got)
	}
	_ = application.Close()
	if err := <-done; err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
}

type testImmutableObjectProvider struct {
	peer        string
	object      string
	snapshot    uint64
	transaction string
	data        []byte
	length      uint64
}

type testSnapshotReferenceProvider struct {
	peer      string
	snapshot  uint64
	operation string
	roots     []SnapshotRootBinding
	retained  int
	released  int
	published int
}

type testSharedReferenceProvider struct {
	peer      string
	snapshot  uint64
	operation string
	name      string
	object    string
	retain    bool
	from      uint64
	mutations []SharedReferenceMutation
	imports   int
}

type testReferenceInventory struct {
	peer      string
	inventory SnapshotReferenceInventory
}

func (inventory *testReferenceInventory) SnapshotReferenceInventory(_ context.Context, peer string) (*SnapshotReferenceInventory, error) {
	inventory.peer = peer
	result := inventory.inventory
	result.Claims = append([]SnapshotReferenceClaim(nil), result.Claims...)
	return &result, nil
}

func (provider *testSharedReferenceProvider) ImportSharedSnapshot(_ context.Context, peer string, snapshot uint64) error {
	provider.peer, provider.snapshot = peer, snapshot
	provider.imports++
	return nil
}

func (provider *testSharedReferenceProvider) ApplySharedSnapshotReference(_ context.Context, peer string, snapshot uint64, operation, name, object string, retain bool) error {
	provider.peer, provider.snapshot = peer, snapshot
	provider.operation, provider.name, provider.object, provider.retain = operation, name, object, retain
	return nil
}

func (provider *testSharedReferenceProvider) ApplySharedSnapshotHandoff(_ context.Context, peer string, snapshot, from uint64, mutations []SharedReferenceMutation) error {
	provider.peer, provider.snapshot, provider.from = peer, snapshot, from
	provider.mutations = append([]SharedReferenceMutation(nil), mutations...)
	return nil
}

func (provider *testSharedReferenceProvider) FlushSharedSnapshot(_ context.Context, peer string, snapshot uint64) error {
	provider.peer, provider.snapshot = peer, snapshot
	return nil
}

func (provider *testSnapshotReferenceProvider) RetainSnapshotRoots(_ context.Context, peer string, snapshot uint64, _, operation string, roots []SnapshotRootBinding) error {
	provider.peer, provider.snapshot, provider.operation = peer, snapshot, operation
	provider.roots = append([]SnapshotRootBinding(nil), roots...)
	provider.retained++
	return nil
}

func (provider *testSnapshotReferenceProvider) ReleaseSnapshotRoots(_ context.Context, peer string, snapshot uint64, _, operation string, roots []SnapshotRootBinding) error {
	provider.peer, provider.snapshot, provider.operation = peer, snapshot, operation
	provider.roots = append([]SnapshotRootBinding(nil), roots...)
	provider.released++
	return nil
}

func (provider *testSnapshotReferenceProvider) ApplySnapshotReference(_ context.Context, peer string, snapshot uint64, operation, _, _ string, retain bool) error {
	provider.peer, provider.snapshot, provider.operation = peer, snapshot, operation
	if retain {
		provider.retained++
	} else {
		provider.released++
	}
	return nil
}

func (provider *testSnapshotReferenceProvider) PublishSnapshot(_ context.Context, peer string, snapshot uint64) error {
	provider.peer, provider.snapshot = peer, snapshot
	provider.published++
	return nil
}

func TestSnapshotReferenceProviderReceivesBoundedOperation(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "transactions")
	if err := os.Mkdir(storeRoot, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	provider := &testSnapshotReferenceProvider{}
	control := &ControlPlane{AuthToken: testClusterToken, Capabilities: testCapabilities("target"), Store: store, MaxTransactions: 1, Now: time.Now, SnapshotReferences: provider}
	server := httptest.NewServer(control)
	defer server.Close()
	client, err := NewPlainClient(server.URL, "target", testClusterToken, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	roots := []SnapshotRootBinding{{Slot: 3, Object: "000000000000002a-03-0000000000000001"}}
	if err := client.RetainSnapshotRoots(context.Background(), 42, "tx-request", "tx-request", roots); err != nil {
		t.Fatal(err)
	}
	if provider.retained != 1 || provider.released != 0 || provider.peer != "target" || provider.snapshot != 42 || provider.operation != "tx-request" || len(provider.roots) != 1 || provider.roots[0] != roots[0] {
		t.Fatalf("retain provider state=%+v", provider)
	}
	if err := client.ReleaseSnapshotRoots(context.Background(), 42, "tx-request", "tx-request", roots); err != nil {
		t.Fatal(err)
	}
	if provider.retained != 1 || provider.released != 1 {
		t.Fatalf("release provider state=%+v", provider)
	}
	// Descendant branches have their own durable, unguessable operation and
	// exact immutable root bindings; they deliberately do not remain coupled
	// to the transaction that first imported an ancestor.
	if err := client.RetainSnapshotRoots(context.Background(), 42, "", "branch-request", roots); err != nil {
		t.Fatalf("retaining descendant roots without ancestor transaction: %v", err)
	}
	if err := client.ReleaseSnapshotRoots(context.Background(), 42, "", "branch-request", roots); err != nil {
		t.Fatalf("releasing descendant roots without ancestor transaction: %v", err)
	}
	if provider.retained != 2 || provider.released != 2 {
		t.Fatalf("descendant provider state=%+v", provider)
	}
	if err := client.ApplySnapshotReference(context.Background(), 42, "edge-0000000000000001-0000000000000001", "edge/0000000000000001-04-0000000000000001/0003", roots[0].Object, true); err != nil {
		t.Fatal(err)
	}
	if provider.retained != 3 || provider.operation != "edge-0000000000000001-0000000000000001" {
		t.Fatalf("owner reference provider state=%+v", provider)
	}
	if err := client.PublishSnapshot(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	if provider.published != 1 || provider.peer != "target" || provider.snapshot != 42 {
		t.Fatalf("publication provider state=%+v", provider)
	}
}

func TestSharedReferenceProviderReceivesImportAndCatchup(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "transactions")
	if err := os.Mkdir(storeRoot, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	provider := &testSharedReferenceProvider{}
	control := &ControlPlane{AuthToken: testClusterToken, Capabilities: testCapabilities("target"), Store: store, MaxTransactions: 1, Now: time.Now, SharedReferences: provider}
	server := httptest.NewServer(control)
	defer server.Close()
	client, err := NewPlainClient(server.URL, "target", testClusterToken, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.ImportSharedSnapshot(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	if err := client.ApplySharedSnapshotReference(context.Background(), 42, "publication-bridge-0001", "root/publication-bridge/0001", "000000000000002a-03-0000000000000001", true); err != nil {
		t.Fatal(err)
	}
	mutations := []SharedReferenceMutation{{Sequence: 8, Operation: "op", Name: "root/x", Object: "000000000000002a-03-0000000000000001", Retain: true}}
	if err := client.ApplySharedSnapshotHandoff(context.Background(), 42, 7, mutations); err != nil {
		t.Fatal(err)
	}
	if err := client.FlushSharedSnapshot(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	if provider.imports != 1 || provider.peer != "target" || provider.snapshot != 42 || provider.operation != "publication-bridge-0001" || provider.name != "root/publication-bridge/0001" || provider.object != "000000000000002a-03-0000000000000001" || !provider.retain || provider.from != 7 || len(provider.mutations) != 1 || provider.mutations[0] != mutations[0] {
		t.Fatalf("shared provider state=%+v", provider)
	}
}

func TestSnapshotReferenceInventoryRoundTrip(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "transactions")
	if err := os.Mkdir(storeRoot, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	want := []SnapshotReferenceClaim{{
		Origin: 42, Operation: "branch-claim",
		Roots: []SnapshotRootBinding{{Slot: 3, Object: "000000000000002a-03-0000000000000001"}},
	}}
	inventory := &testReferenceInventory{inventory: SnapshotReferenceInventory{NodeID: "target", Incarnation: "target-boot", Claims: want}}
	control := &ControlPlane{AuthToken: testClusterToken, Capabilities: testCapabilities("target"), Store: store, MaxTransactions: 1, Now: time.Now, ReferenceInventory: inventory}
	server := httptest.NewServer(control)
	defer server.Close()
	client, err := NewPlainClient(server.URL, "coordinator", testClusterToken, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.SnapshotReferenceInventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if inventory.peer != "coordinator" || got.NodeID != "target" || got.Incarnation != "target-boot" || len(got.Claims) != 1 || got.Claims[0].Origin != want[0].Origin || got.Claims[0].Operation != want[0].Operation || len(got.Claims[0].Roots) != 1 || got.Claims[0].Roots[0] != want[0].Roots[0] {
		t.Fatalf("claims=%+v peer=%q, want %+v coordinator", got, inventory.peer, want)
	}
}

func (provider *testImmutableObjectProvider) OpenSnapshot(_ context.Context, peer string, snapshot uint64, transaction string) (io.ReadCloser, uint64, error) {
	provider.peer, provider.snapshot, provider.transaction = peer, snapshot, transaction
	return io.NopCloser(bytes.NewReader(provider.data)), provider.length, nil
}

func (provider *testImmutableObjectProvider) OpenSnapshotObject(_ context.Context, peer string, snapshot uint64, object, transaction string) (io.ReadCloser, uint64, error) {
	provider.peer, provider.snapshot, provider.object, provider.transaction = peer, snapshot, object, transaction
	return io.NopCloser(bytes.NewReader(provider.data)), provider.length, nil
}

func (provider *testImmutableObjectProvider) OpenObject(_ context.Context, peer, object, transaction string) (io.ReadCloser, uint64, error) {
	provider.peer, provider.object, provider.transaction = peer, object, transaction
	return io.NopCloser(bytes.NewReader(provider.data)), provider.length, nil
}

func TestImmutableSnapshotProviderStreamsExactCatalog(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "transactions")
	if err := os.Mkdir(storeRoot, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	provider := &testImmutableObjectProvider{data: []byte(`{"version":1,"snapshot":42,"objects":[]}`)}
	provider.length = uint64(len(provider.data))
	control := &ControlPlane{AuthToken: testClusterToken, Capabilities: testCapabilities("target"), Store: store, MaxTransactions: 1, Now: time.Now, SnapshotProvider: provider}
	server := httptest.NewServer(control)
	defer server.Close()
	client, err := NewPlainClient(server.URL, "source", testClusterToken, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	reader, length, err := client.FetchSnapshot(context.Background(), 42, "tx-request")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || length != uint64(len(provider.data)) || !bytes.Equal(got, provider.data) {
		t.Fatalf("catalog length=%d bytes=%d err=%v", length, len(got), err)
	}
	if provider.peer != "source" || provider.snapshot != 42 || provider.transaction != "tx-request" {
		t.Fatalf("provider request=(%q,%d,%q)", provider.peer, provider.snapshot, provider.transaction)
	}
}

func TestImmutableSnapshotProviderStreamsOneMetadataObject(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "transactions")
	if err := os.Mkdir(storeRoot, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	provider := &testImmutableObjectProvider{data: []byte(`{"ID":{"Snapshot":42,"Ordinal":7,"Kind":4},"State":"live","Refs":1,"BulkLength":0,"Data":null,"Children":null}`)}
	provider.length = uint64(len(provider.data))
	control := &ControlPlane{AuthToken: testClusterToken, Capabilities: testCapabilities("target"), Store: store, MaxTransactions: 1, Now: time.Now, SnapshotProvider: provider}
	server := httptest.NewServer(control)
	defer server.Close()
	client, err := NewPlainClient(server.URL, "source", testClusterToken, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	const object = "000000000000002a-04-0000000000000007"
	reader, length, err := client.FetchSnapshotObject(context.Background(), 42, object, "tx-request")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || length != uint64(len(provider.data)) || !bytes.Equal(got, provider.data) {
		t.Fatalf("metadata object length=%d bytes=%d err=%v", length, len(got), err)
	}
	if provider.peer != "source" || provider.snapshot != 42 || provider.object != object || provider.transaction != "tx-request" {
		t.Fatalf("provider request=(%q,%d,%q,%q)", provider.peer, provider.snapshot, provider.object, provider.transaction)
	}
}

func TestImmutableObjectProviderStreamsExactObject(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "transactions")
	if err := os.Mkdir(storeRoot, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	provider := &testImmutableObjectProvider{data: bytes.Repeat([]byte("object"), 1000)}
	provider.length = uint64(len(provider.data))
	control := &ControlPlane{AuthToken: testClusterToken, Capabilities: testCapabilities("target"), Store: store, MaxTransactions: 1, Now: time.Now, ObjectProvider: provider}
	server := httptest.NewServer(control)
	defer server.Close()
	client, err := NewPlainClient(server.URL, "source", testClusterToken, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	reader, length, err := client.FetchObject(context.Background(), "0000000000000001-02-0000000000000001", "tx-request")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || length != uint64(len(provider.data)) || !bytes.Equal(got, provider.data) {
		t.Fatalf("object length=%d bytes=%d err=%v", length, len(got), err)
	}
	if provider.peer != "source" || provider.object != "0000000000000001-02-0000000000000001" || provider.transaction != "tx-request" {
		t.Fatalf("provider request=(%q,%q,%q)", provider.peer, provider.object, provider.transaction)
	}
}

type testTargetRuntime struct {
	mu            sync.Mutex
	reserved      int
	prepared      int
	published     int
	activated     int
	publishPaused []bool
	drained       int
	handedOff     int
	canceled      int
	independent   int
	prepareErr    error
	handoffErr    error
	cancelErr     error
}

type staleReservationStore struct {
	TransactionCatalog
	once sync.Once
}

type atomicTestTransactionStore struct {
	*Store
}

func (s *atomicTestTransactionStore) PublishTarget(id string, revision uint64, desiredPaused bool, family, branch, physicalContainer, physicalBranch, targetIncarnation string) (*Transaction, error) {
	return nil, errors.New("unexpected fork publication in migration test")
}

// lostResponseTransactionStore commits one selected mutation and then models
// the daemon or HTTP connection disappearing before its response arrives.
type lostResponseTransactionStore struct {
	TransactionCatalog
	failAdvance TransactionState
	failBind    bool
	failPublish bool
	failIndep   bool
}

func (s *lostResponseTransactionStore) Advance(id string, revision uint64, next TransactionState, failure string, now time.Time) (*Transaction, error) {
	tx, err := s.TransactionCatalog.Advance(id, revision, next, failure, now)
	if err == nil && s.failAdvance == next {
		s.failAdvance = ""
		return nil, io.ErrUnexpectedEOF
	}
	return tx, err
}

func (s *lostResponseTransactionStore) BindSession(id string, revision uint64, digest string, now time.Time) (*Transaction, error) {
	tx, err := s.TransactionCatalog.BindSession(id, revision, digest, now)
	if err == nil && s.failBind {
		s.failBind = false
		return nil, io.ErrUnexpectedEOF
	}
	return tx, err
}

func (s *lostResponseTransactionStore) Publish(id string, revision uint64, desiredPaused, activated bool, now time.Time) (*Transaction, error) {
	tx, err := s.TransactionCatalog.Publish(id, revision, desiredPaused, activated, now)
	if err == nil && s.failPublish {
		s.failPublish = false
		return nil, io.ErrUnexpectedEOF
	}
	return tx, err
}

func (s *lostResponseTransactionStore) AdvanceIndependent(id string, revision uint64, metrics TransferMetrics, now time.Time) (*Transaction, error) {
	tx, err := s.TransactionCatalog.AdvanceIndependent(id, revision, metrics, now)
	if err == nil && s.failIndep {
		s.failIndep = false
		return nil, io.ErrUnexpectedEOF
	}
	return tx, err
}

func (s *staleReservationStore) Advance(id string, revision uint64, next TransactionState, failure string, now time.Time) (*Transaction, error) {
	if next == TransactionTargetReserved {
		injected := false
		s.once.Do(func() {
			injected = true
			_, _ = s.TransactionCatalog.Advance(id, revision, next, failure, now)
		})
		if injected {
			return nil, errors.New("stale transaction revision")
		}
	}
	return s.TransactionCatalog.Advance(id, revision, next, failure, now)
}

func (r *testTargetRuntime) Reserve(context.Context, *Transaction, RemoteFamilyDescriptor) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reserved++
	return nil
}

func testRemoteFamilyDescriptor() RemoteFamilyDescriptor {
	return RemoteFamilyDescriptor{Container: json.RawMessage(`{}`), Spec: json.RawMessage(`{}`)}
}

func (r *testTargetRuntime) Prepare(context.Context, *Transaction, RemoteSessionDescriptor) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prepared++
	return r.prepareErr
}

func (r *testTargetRuntime) Publish(_ context.Context, _ *Transaction, paused bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.published++
	r.publishPaused = append(r.publishPaused, paused)
	return nil
}

func (r *testTargetRuntime) PublishedTarget(tx *Transaction) (family, branch, physicalContainer, physicalBranch, incarnation string, err error) {
	return tx.Identity.TargetContainer, tx.Identity.VisibleTargetBranch(), "physical-target", tx.Identity.VisibleTargetBranch(), "target-incarnation", nil
}

func (r *testTargetRuntime) Activate(context.Context, *Transaction) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.activated++
	return nil
}

func (r *testTargetRuntime) Drain(context.Context, *Transaction) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drained++
	return nil
}

func (r *testTargetRuntime) Handoff(context.Context, *Transaction, GraphHandoffDescriptor) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handedOff++
	if r.handoffErr != nil {
		err := r.handoffErr
		r.handoffErr = nil
		return err
	}
	return nil
}

func (r *testTargetRuntime) Cancel(context.Context, *Transaction) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.canceled++
	return r.cancelErr
}

func (r *testTargetRuntime) MarkIndependent(context.Context, *Transaction) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.independent++
	return nil
}

type testTLSFiles struct {
	ca, serverCert, serverKey, clientCert, clientKey, intruderCert, intruderKey string
}

func writeTestPEM(t *testing.T, name, typ string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: data}), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newTestTLSFiles(t *testing.T) testTLSFiles {
	t.Helper()
	now := time.Now()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "TTC test CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write := func(name, typ string, data []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: data}), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	files := testTLSFiles{ca: write("ca.pem", "CERTIFICATE", caDER)}
	issue := func(name string, serial int64, server bool) (string, string) {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		template := &x509.Certificate{
			SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name},
			NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
			KeyUsage: x509.KeyUsageDigitalSignature,
		}
		if server {
			template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
			template.DNSNames = []string{"localhost"}
			template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		} else {
			template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		}
		der, err := x509.CreateCertificate(rand.Reader, template, caTemplate, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		return write(name+".pem", "CERTIFICATE", der), write(name+"-key.pem", "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(key))
	}
	files.serverCert, files.serverKey = issue("target", 2, true)
	files.clientCert, files.clientKey = issue("source", 3, false)
	files.intruderCert, files.intruderKey = issue("intruder", 4, false)
	return files
}

func TestMutualTLSControlPlaneReservationStatusAndCancel(t *testing.T) {
	files := newTestTLSFiles(t)
	serverTLS, err := LoadServerTLSConfig(files.serverCert, files.serverKey, files.ca)
	if err != nil {
		t.Fatal(err)
	}
	clientTLS, err := LoadClientTLSConfig(files.clientCert, files.clientKey, files.ca, "localhost")
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "transactions"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2000, 0)
	control := &ControlPlane{
		AuthToken: testClusterToken, Capabilities: testCapabilities("target"), Store: store, MaxTransactions: 1,
		Now:           func() time.Time { now = now.Add(time.Second); return now },
		TargetRuntime: &testTargetRuntime{},
	}
	server := httptest.NewUnstartedServer(control)
	server.TLS = serverTLS
	server.StartTLS()
	defer server.Close()
	client, err := NewClient(server.URL, clientTLS, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	request := ReserveRequest{
		SourceCapabilities: testCapabilities("source"), Identity: testForkIdentity(), Family: testRemoteFamilyDescriptor(),
		SourcePhysicalContainer: "cold-cache", SourcePhysicalBranch: "cold-branch",
	}
	tx, err := client.Reserve(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if tx.State != TransactionTargetReserved || tx.Revision != 2 {
		t.Fatalf("reserved transaction=%+v", tx)
	}
	if tx.SourcePhysicalContainer != "cold-cache" || tx.SourcePhysicalBranch != "cold-branch" {
		t.Fatalf("reserved physical source=%s@%s", tx.SourcePhysicalContainer, tx.SourcePhysicalBranch)
	}
	// A lost reserve response may be retried without consuming quota or
	// replacing the existing identity.
	retried, err := client.Reserve(context.Background(), request)
	if err != nil || retried.Revision != tx.Revision {
		t.Fatalf("idempotent reserve=%+v err=%v", retried, err)
	}
	mismatched := request
	mismatched.SourcePhysicalContainer = "other-cache"
	if _, err := client.Reserve(context.Background(), mismatched); err == nil {
		t.Fatal("reserve accepted a different physical source for the same transaction")
	}
	loaded, err := client.Status(context.Background(), tx.Identity.TransactionID)
	if err != nil || loaded.Revision != tx.Revision {
		t.Fatalf("status=%+v err=%v", loaded, err)
	}
	canceled, err := client.Cancel(context.Background(), tx.Identity.TransactionID, tx.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if canceled.State != TransactionCanceled || !canceled.Terminal() {
		t.Fatalf("canceled transaction=%+v", canceled)
	}
	// A lost cancel response is also idempotent with the original revision.
	if again, err := client.Cancel(context.Background(), tx.Identity.TransactionID, tx.Revision); err != nil || again.Revision != canceled.Revision {
		t.Fatalf("idempotent cancel=%+v err=%v", again, err)
	}
}

func TestControlPlaneReservationResolvesLostAdvanceResponse(t *testing.T) {
	base, err := OpenStore(filepath.Join(t.TempDir(), "transactions"))
	if err != nil {
		t.Fatal(err)
	}
	store := &staleReservationStore{TransactionCatalog: base}
	control := &ControlPlane{
		AuthToken: testClusterToken, Capabilities: testCapabilities("target"), Store: store, MaxTransactions: 1,
		Now: time.Now, TargetRuntime: &testTargetRuntime{},
	}
	server := httptest.NewServer(control)
	defer server.Close()
	client, err := NewPlainClient(server.URL, "source", testClusterToken, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := client.Reserve(context.Background(), ReserveRequest{SourceCapabilities: testCapabilities("source"), Identity: testForkIdentity(), Family: testRemoteFamilyDescriptor()})
	if err != nil {
		t.Fatal(err)
	}
	if tx.State != TransactionTargetReserved || tx.Revision != 2 {
		t.Fatalf("reserved transaction=%+v", tx)
	}
}

func TestCancelFailedPreparationReclaimsTargetWithoutRewritingFailure(t *testing.T) {
	files := newTestTLSFiles(t)
	serverTLS, err := LoadServerTLSConfig(files.serverCert, files.serverKey, files.ca)
	if err != nil {
		t.Fatal(err)
	}
	clientTLS, err := LoadClientTLSConfig(files.clientCert, files.clientKey, files.ca, "localhost")
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "transactions"))
	if err != nil {
		t.Fatal(err)
	}
	runtime := &testTargetRuntime{prepareErr: context.DeadlineExceeded}
	control := &ControlPlane{
		AuthToken: testClusterToken, Capabilities: testCapabilities("target"), Store: store, MaxTransactions: 1,
		Now: time.Now, TargetRuntime: runtime,
	}
	server := httptest.NewUnstartedServer(control)
	server.TLS = serverTLS
	server.StartTLS()
	defer server.Close()
	client, err := NewClient(server.URL, clientTLS, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := client.Reserve(context.Background(), ReserveRequest{SourceCapabilities: testCapabilities("source"), Identity: testForkIdentity(), Family: testRemoteFamilyDescriptor()})
	if err != nil {
		t.Fatal(err)
	}
	session := RemoteSessionDescriptor{
		Address: "10.0.0.1:12345", SnapshotToken: strings.Repeat("ab", 16),
		MetadataBytes: 1 << 20, StreamBytes: 8 << 20, MaxTransferBytes: 64 << 10,
	}
	if _, err := client.Prepare(context.Background(), tx.Identity.TransactionID, tx.Revision, session); err == nil {
		t.Fatal("Prepare unexpectedly succeeded")
	}
	failed, err := client.Status(context.Background(), tx.Identity.TransactionID)
	if err != nil || failed.State != TransactionFailed || failed.Failure == "" {
		t.Fatalf("failed transaction=%+v err=%v", failed, err)
	}
	cleaned, err := client.Cancel(context.Background(), failed.Identity.TransactionID, failed.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if cleaned.State != TransactionFailed || cleaned.Failure != failed.Failure || cleaned.Revision != failed.Revision {
		t.Fatalf("cleanup rewrote failed transaction: before=%+v after=%+v", failed, cleaned)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.canceled != 1 {
		t.Fatalf("target cleanup calls=%d, want 1", runtime.canceled)
	}
}

func TestMutualTLSControlPlanePreparePublishAndDrain(t *testing.T) {
	files := newTestTLSFiles(t)
	serverTLS, err := LoadServerTLSConfig(files.serverCert, files.serverKey, files.ca)
	if err != nil {
		t.Fatal(err)
	}
	clientTLS, err := LoadClientTLSConfig(files.clientCert, files.clientKey, files.ca, "localhost")
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "transactions"))
	if err != nil {
		t.Fatal(err)
	}
	runtime := &testTargetRuntime{}
	now := time.Unix(3000, 0)
	control := &ControlPlane{
		AuthToken: testClusterToken, Capabilities: testCapabilities("target"), Store: store, MaxTransactions: 1,
		Now: func() time.Time { now = now.Add(time.Second); return now }, TargetRuntime: runtime,
	}
	server := httptest.NewUnstartedServer(control)
	server.TLS = serverTLS
	server.StartTLS()
	defer server.Close()
	client, err := NewClient(server.URL, clientTLS, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := client.Reserve(context.Background(), ReserveRequest{SourceCapabilities: testCapabilities("source"), Identity: testForkIdentity(), Family: testRemoteFamilyDescriptor()})
	if err != nil {
		t.Fatal(err)
	}
	session := RemoteSessionDescriptor{
		Address: "10.0.0.1:12345", SnapshotToken: strings.Repeat("ab", 16),
		MetadataBytes: 1 << 20, StreamBytes: 8 << 20, MaxTransferBytes: 64 << 10,
	}
	prepared, err := client.Prepare(context.Background(), tx.Identity.TransactionID, tx.Revision, session)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.State != TransactionTargetPaused || prepared.SessionDigest == "" {
		t.Fatalf("prepared=%+v", prepared)
	}
	// Repeating the original request after losing the response must neither
	// restore a second time nor reject the stale revision.
	retried, err := client.Prepare(context.Background(), tx.Identity.TransactionID, tx.Revision, session)
	if err != nil || retried.Revision != prepared.Revision {
		t.Fatalf("prepare retry=%+v err=%v", retried, err)
	}
	published, err := client.Publish(context.Background(), tx.Identity.TransactionID, prepared.Revision, true)
	if err != nil || published.State != TransactionPublished {
		t.Fatalf("publish=%+v err=%v", published, err)
	}
	draining, err := client.Drain(context.Background(), tx.Identity.TransactionID, published.Revision)
	if err != nil || draining.State != TransactionDraining || !draining.SourceNeeded {
		t.Fatalf("draining=%+v err=%v", draining, err)
	}
	notCanceled, err := client.Cancel(context.Background(), tx.Identity.TransactionID, draining.Revision)
	if err != nil || notCanceled.State != TransactionDraining || notCanceled.Revision != draining.Revision {
		t.Fatalf("cancel during drain=%+v err=%v, want unchanged draining transaction", notCanceled, err)
	}
	metrics := TransferMetrics{PullRequests: 7, PullBytes: 4096, MetadataBytes: 512, PagesTotal: 1, PagesReleased: 1}
	independent, err := client.MarkIndependentWithMetrics(context.Background(), tx.Identity.TransactionID, draining.Revision, metrics)
	if err != nil || independent.State != TransactionIndependent || independent.SourceNeeded {
		t.Fatalf("independent=%+v err=%v", independent, err)
	}
	if independent.Metrics == nil || independent.Metrics.PullRequests != 7 || independent.Metrics.TimeToIndependent <= 0 {
		t.Fatalf("independent metrics=%+v", independent.Metrics)
	}
	notCanceled, err = client.Cancel(context.Background(), tx.Identity.TransactionID, independent.Revision)
	if err != nil || notCanceled.State != TransactionIndependent || notCanceled.Revision != independent.Revision {
		t.Fatalf("cancel after independence=%+v err=%v, want unchanged independent transaction", notCanceled, err)
	}
	migrationIdentity := testForkIdentity()
	migrationIdentity.TransactionID = "tx-migration"
	migrationIdentity.TargetContainer = migrationIdentity.SourceContainer
	migrationIdentity.TargetBranch = "tx-migration"
	migrationIdentity.TargetVisibleBranch = migrationIdentity.SourceBranch
	migrationIdentity.Migration = true
	migration, err := client.Reserve(context.Background(), ReserveRequest{SourceCapabilities: testCapabilities("source"), Identity: migrationIdentity, Family: testRemoteFamilyDescriptor()})
	if err != nil {
		t.Fatal(err)
	}
	migration, err = client.Prepare(context.Background(), migration.Identity.TransactionID, migration.Revision, session)
	if err != nil {
		t.Fatal(err)
	}
	migration, err = client.Publish(context.Background(), migration.Identity.TransactionID, migration.Revision, false)
	if err != nil {
		t.Fatal(err)
	}
	if migration.Activated || migration.DesiredPaused || migration.State != TransactionPublished ||
		migration.TargetPhysicalContainer != "physical-target" || migration.TargetPhysicalBranch != migration.Identity.VisibleTargetBranch() ||
		migration.TargetPhysicalIncarnation != "target-incarnation" {
		t.Fatalf("migration publication=%+v, want paused cutover awaiting activation", migration)
	}
	migration, err = client.Activate(context.Background(), migration.Identity.TransactionID, migration.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if !migration.Activated {
		t.Fatalf("activated migration=%+v", migration)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.prepared != 2 || runtime.published != 2 || runtime.activated != 1 || runtime.drained != 1 || runtime.independent != 1 || runtime.canceled != 0 || len(runtime.publishPaused) != 2 || !runtime.publishPaused[0] || !runtime.publishPaused[1] {
		t.Fatalf("runtime calls: %+v", runtime)
	}
}

func TestMigrationPublishRetryCannotActivateBeforeCutover(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "transactions"))
	if err != nil {
		t.Fatal(err)
	}
	runtime := &testTargetRuntime{}
	control := &ControlPlane{
		AuthToken: testClusterToken, Capabilities: testCapabilities("target"), Store: &atomicTestTransactionStore{Store: store},
		MaxTransactions: 1, Now: time.Now, TargetRuntime: runtime,
	}
	server := httptest.NewServer(control)
	defer server.Close()
	client, err := NewPlainClient(server.URL, "source", testClusterToken, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	identity := testForkIdentity()
	identity.TransactionID = "migration-publish-retry"
	identity.TargetContainer = identity.SourceContainer
	identity.TargetBranch = identity.TransactionID
	identity.TargetVisibleBranch = identity.SourceBranch
	identity.Migration = true
	tx, err := client.Reserve(context.Background(), ReserveRequest{SourceCapabilities: testCapabilities("source"), Identity: identity, Family: testRemoteFamilyDescriptor()})
	if err != nil {
		t.Fatal(err)
	}
	session := RemoteSessionDescriptor{
		Address: "10.0.0.1:12345", SnapshotToken: strings.Repeat("ab", 16),
		MetadataBytes: 1 << 20, StreamBytes: 8 << 20, MaxTransferBytes: 64 << 10,
	}
	prepared, err := client.Prepare(context.Background(), tx.Identity.TransactionID, tx.Revision, session)
	if err != nil {
		t.Fatal(err)
	}
	published, err := client.Publish(context.Background(), tx.Identity.TransactionID, prepared.Revision, false)
	if err != nil {
		t.Fatal(err)
	}
	if published.State != TransactionPublished || published.Activated || published.OwnershipCommitted {
		t.Fatalf("pre-cutover publication=%+v", published)
	}
	// Model a lost first response. A retry carrying the old prepared revision
	// must return the durable publication without resuming the target.
	retried, err := client.Publish(context.Background(), tx.Identity.TransactionID, prepared.Revision, false)
	if err != nil || retried.Revision != published.Revision || retried.Activated {
		t.Fatalf("pre-cutover publish retry=%+v err=%v", retried, err)
	}
	if _, err := client.Activate(context.Background(), tx.Identity.TransactionID, published.Revision); err == nil {
		t.Fatal("pre-cutover migration activation unexpectedly succeeded")
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.published != 1 || runtime.activated != 0 {
		t.Fatalf("runtime calls after publish retry: %+v", runtime)
	}
}

func TestTransactionLostResponsesRecoverAtEveryPersistentBoundary(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "transactions")
	runtime := &testTargetRuntime{}
	start := func(catalog TransactionCatalog) (*Client, func()) {
		t.Helper()
		server := httptest.NewServer(&ControlPlane{
			AuthToken: testClusterToken, Capabilities: testCapabilities("target"), Store: catalog,
			MaxTransactions: 1, Now: time.Now, TargetRuntime: runtime,
		})
		client, err := NewPlainClient(server.URL, "source", testClusterToken, 5*time.Second)
		if err != nil {
			server.Close()
			t.Fatal(err)
		}
		return client, server.Close
	}
	reopen := func() *Store {
		t.Helper()
		store, err := OpenStore(directory)
		if err != nil {
			t.Fatal(err)
		}
		return store
	}
	request := ReserveRequest{SourceCapabilities: testCapabilities("source"), Identity: testForkIdentity(), Family: testRemoteFamilyDescriptor()}

	// Target reservation committed, but the first daemon disappeared before
	// acknowledging it. The handler resolves the committed state immediately,
	// and a replacement daemon must not reserve twice.
	client, stop := start(&lostResponseTransactionStore{TransactionCatalog: reopen(), failAdvance: TransactionTargetReserved})
	tx, err := client.Reserve(context.Background(), request)
	if err != nil || tx.State != TransactionTargetReserved {
		t.Fatalf("reservation response recovery=%+v err=%v", tx, err)
	}
	stop()
	client, stop = start(reopen())
	tx, err = client.Reserve(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	stop()

	session := RemoteSessionDescriptor{
		Address: "10.0.0.1:12345", SnapshotToken: strings.Repeat("ab", 16),
		MetadataBytes: 1 << 20, StreamBytes: 8 << 20, MaxTransferBytes: 64 << 10,
	}
	// Session binding and runtime-restore completion are separate durable
	// boundaries. Lose each response, reopen the catalog, and replay the same
	// original request/revision.
	client, stop = start(&lostResponseTransactionStore{TransactionCatalog: reopen(), failBind: true})
	if _, err := client.Prepare(context.Background(), tx.Identity.TransactionID, tx.Revision, session); err == nil {
		t.Fatal("session binding with lost response unexpectedly succeeded")
	}
	stop()
	client, stop = start(&lostResponseTransactionStore{TransactionCatalog: reopen(), failAdvance: TransactionTargetPaused})
	if _, err := client.Prepare(context.Background(), tx.Identity.TransactionID, tx.Revision, session); err == nil {
		t.Fatal("restore completion with lost response unexpectedly succeeded")
	}
	stop()
	client, stop = start(reopen())
	prepared, err := client.Prepare(context.Background(), tx.Identity.TransactionID, tx.Revision, session)
	if err != nil {
		t.Fatal(err)
	}
	stop()

	client, stop = start(&lostResponseTransactionStore{TransactionCatalog: reopen(), failPublish: true})
	if _, err := client.Publish(context.Background(), tx.Identity.TransactionID, prepared.Revision, false); err == nil {
		t.Fatal("publication with lost response unexpectedly succeeded")
	}
	stop()
	client, stop = start(reopen())
	published, err := client.Publish(context.Background(), tx.Identity.TransactionID, prepared.Revision, false)
	if err != nil || published.State != TransactionPublished {
		t.Fatalf("publication recovery=%+v err=%v", published, err)
	}
	stop()

	client, stop = start(&lostResponseTransactionStore{TransactionCatalog: reopen(), failAdvance: TransactionDraining})
	if _, err := client.Drain(context.Background(), tx.Identity.TransactionID, published.Revision); err == nil {
		t.Fatal("drain with lost response unexpectedly succeeded")
	}
	stop()
	client, stop = start(reopen())
	draining, err := client.Drain(context.Background(), tx.Identity.TransactionID, published.Revision)
	if err != nil || draining.State != TransactionDraining {
		t.Fatalf("drain recovery=%+v err=%v", draining, err)
	}
	stop()

	metrics := TransferMetrics{PullRequests: 1, PullBytes: 4096, PagesTotal: 1, PagesReleased: 1}
	client, stop = start(&lostResponseTransactionStore{TransactionCatalog: reopen(), failIndep: true})
	if _, err := client.MarkIndependentWithMetrics(context.Background(), tx.Identity.TransactionID, draining.Revision, metrics); err == nil {
		t.Fatal("independence with lost response unexpectedly succeeded")
	}
	stop()
	client, stop = start(reopen())
	independent, err := client.MarkIndependentWithMetrics(context.Background(), tx.Identity.TransactionID, draining.Revision, metrics)
	if err != nil || independent.State != TransactionIndependent || independent.SourceNeeded {
		t.Fatalf("independence recovery=%+v err=%v", independent, err)
	}
	stop()

	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.reserved != 1 || runtime.prepared != 1 || runtime.published != 1 || runtime.drained != 1 || runtime.independent != 1 {
		t.Fatalf("runtime operations were not idempotent across response loss: %+v", runtime)
	}
}

func TestMutualTLSControlPlaneGraphHandoffIsDurableAndIdempotent(t *testing.T) {
	files := newTestTLSFiles(t)
	serverTLS, err := LoadServerTLSConfig(files.serverCert, files.serverKey, files.ca)
	if err != nil {
		t.Fatal(err)
	}
	clientTLS, err := LoadClientTLSConfig(files.clientCert, files.clientKey, files.ca, "localhost")
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "transactions"))
	if err != nil {
		t.Fatal(err)
	}
	runtime := &testTargetRuntime{}
	now := time.Unix(4000, 0)
	control := &ControlPlane{
		AuthToken: testClusterToken, Capabilities: testCapabilities("target"), Store: store, MaxTransactions: 1,
		Now: func() time.Time { now = now.Add(time.Second); return now }, TargetRuntime: runtime,
	}
	server := httptest.NewUnstartedServer(control)
	server.TLS = serverTLS
	server.StartTLS()
	defer server.Close()
	client, err := NewClient(server.URL, clientTLS, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := client.Reserve(context.Background(), ReserveRequest{SourceCapabilities: testCapabilities("source"), Identity: testForkIdentity(), Family: testRemoteFamilyDescriptor()})
	if err != nil {
		t.Fatal(err)
	}
	session := RemoteSessionDescriptor{
		Address: "10.0.0.1:12345", SnapshotToken: strings.Repeat("ab", 16),
		MetadataBytes: 1 << 20, StreamBytes: 8 << 20, MaxTransferBytes: 64 << 10,
	}
	prepared, err := client.Prepare(context.Background(), tx.Identity.TransactionID, tx.Revision, session)
	if err != nil {
		t.Fatal(err)
	}
	published, err := client.Publish(context.Background(), tx.Identity.TransactionID, prepared.Revision, true)
	if err != nil {
		t.Fatal(err)
	}
	sparse := GraphHandoffDescriptor{
		GraphManifest: "cluster/memory-manifests/tx-1.manifest", GraphSnapshot: 17,
		GraphRootObjects: "0000000000000011-03-0000000000000001",
	}
	runtime.mu.Lock()
	runtime.handoffErr = errors.New("injected target handoff failure")
	runtime.mu.Unlock()
	if _, err := client.Handoff(context.Background(), tx.Identity.TransactionID, published.Revision, sparse); err == nil {
		t.Fatal("initial target handoff unexpectedly succeeded")
	}
	reopened, err := OpenStore(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := reopened.Load(tx.Identity.TransactionID)
	if err != nil || persisted.State != TransactionPublished || persisted.GraphHandoff == nil || *persisted.GraphHandoff != sparse {
		t.Fatalf("persisted handoff=%+v err=%v", persisted, err)
	}
	// Eager drain must recover the bound-but-not-installed handoff before it
	// advances. Otherwise a later independence request has no target-local
	// completion status and can retry forever.
	draining, err := client.Drain(context.Background(), tx.Identity.TransactionID, persisted.Revision)
	if err != nil || draining.State != TransactionDraining {
		t.Fatalf("drain recovery=%+v err=%v", draining, err)
	}
	// A replay with the pre-handoff revision models a lost HTTP response.
	retried, err := client.Handoff(context.Background(), tx.Identity.TransactionID, published.Revision, sparse)
	if err != nil || retried.Revision != draining.Revision {
		t.Fatalf("handoff retry=%+v err=%v", retried, err)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.handedOff != 3 || runtime.drained != 1 {
		t.Fatalf("runtime handoff/drain calls=(%d,%d), want (3,1)", runtime.handedOff, runtime.drained)
	}
}

func TestGraphHandoffDescriptorRejectsCrossSnapshotRoots(t *testing.T) {
	valid := GraphHandoffDescriptor{
		GraphManifest:    "cluster/memory-manifests/tx.manifest",
		GraphSnapshot:    17,
		GraphRootObjects: "0000000000000011-03-0000000000000001",
	}
	if err := valid.validate(); err != nil {
		t.Fatalf("valid descriptor: %v", err)
	}
	wrong := valid
	wrong.GraphRootObjects = "0000000000000012-03-0000000000000001"
	if err := wrong.validate(); err == nil {
		t.Fatal("cross-snapshot root was accepted")
	}
	missing := valid
	missing.GraphRootObjects = ""
	if err := missing.validate(); err == nil {
		t.Fatal("empty graph root set was accepted")
	}
}

func TestControlPlaneTransactionLocksAreIndependent(t *testing.T) {
	control := &ControlPlane{}
	unlockA := control.lockTransaction("fork-a")
	defer unlockA()
	acquiredB := make(chan func(), 1)
	go func() {
		acquiredB <- control.lockTransaction("fork-b")
	}()
	select {
	case unlockB := <-acquiredB:
		unlockB()
	case <-time.After(time.Second):
		t.Fatal("unrelated transaction lock was serialized")
	}
	acquiredA := make(chan func(), 1)
	go func() {
		acquiredA <- control.lockTransaction("fork-a")
	}()
	select {
	case unlock := <-acquiredA:
		unlock()
		t.Fatal("same transaction lock was acquired concurrently")
	case <-time.After(10 * time.Millisecond):
	}
}

func TestPrepareRejectsChangedSessionAfterLostResponse(t *testing.T) {
	files := newTestTLSFiles(t)
	serverTLS, _ := LoadServerTLSConfig(files.serverCert, files.serverKey, files.ca)
	clientTLS, _ := LoadClientTLSConfig(files.clientCert, files.clientKey, files.ca, "localhost")
	store, err := OpenStore(filepath.Join(t.TempDir(), "transactions"))
	if err != nil {
		t.Fatal(err)
	}
	control := &ControlPlane{Capabilities: testCapabilities("target"), Store: store, MaxTransactions: 1, Now: time.Now, TargetRuntime: &testTargetRuntime{}}
	server := httptest.NewUnstartedServer(control)
	server.TLS = serverTLS
	server.StartTLS()
	defer server.Close()
	client, _ := NewClient(server.URL, clientTLS, 5*time.Second)
	tx, err := client.Reserve(context.Background(), ReserveRequest{SourceCapabilities: testCapabilities("source"), Identity: testForkIdentity(), Family: testRemoteFamilyDescriptor()})
	if err != nil {
		t.Fatal(err)
	}
	session := RemoteSessionDescriptor{Address: "127.0.0.1:1", SnapshotToken: strings.Repeat("11", 16), MetadataBytes: 1, StreamBytes: 4096, MaxTransferBytes: 4096}
	if _, err := client.Prepare(context.Background(), tx.Identity.TransactionID, tx.Revision, session); err != nil {
		t.Fatal(err)
	}
	session.SnapshotToken = strings.Repeat("22", 16)
	if _, err := client.Prepare(context.Background(), tx.Identity.TransactionID, tx.Revision, session); err == nil || !strings.Contains(err.Error(), "another data-plane session") {
		t.Fatalf("changed session error=%v", err)
	}
}

func TestControlPlaneRejectsCertificateNodeMismatch(t *testing.T) {
	files := newTestTLSFiles(t)
	serverTLS, err := LoadServerTLSConfig(files.serverCert, files.serverKey, files.ca)
	if err != nil {
		t.Fatal(err)
	}
	intruderTLS, err := LoadClientTLSConfig(files.intruderCert, files.intruderKey, files.ca, "localhost")
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "transactions"))
	if err != nil {
		t.Fatal(err)
	}
	control := &ControlPlane{
		AuthToken: testClusterToken, Capabilities: testCapabilities("target"), Store: store, MaxTransactions: 1, Now: time.Now,
	}
	server := httptest.NewUnstartedServer(control)
	server.TLS = serverTLS
	server.StartTLS()
	defer server.Close()
	client, err := NewClient(server.URL, intruderTLS, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Reserve(context.Background(), ReserveRequest{
		SourceCapabilities: testCapabilities("source"), Identity: testForkIdentity(), Family: testRemoteFamilyDescriptor(),
	})
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("certificate mismatch error=%v", err)
	}
}

// TestPlainControlPlaneRequiresToken asks with the cluster's token, with
// another, and with none: only the first is admitted. A control plane without
// a token admits no one, not even a caller naming a node, as a cluster
// without a token once did to whoever reached its listener, guests included.
func TestPlainControlPlaneRequiresToken(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "transactions"))
	if err != nil {
		t.Fatal(err)
	}
	proxied := false
	control := &ControlPlane{
		Capabilities: testCapabilities("target"), Store: store, MaxTransactions: 2,
		Now: time.Now, AuthToken: "cluster-secret", TargetRuntime: &testTargetRuntime{},
		CommandProxy: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { proxied = true }),
	}
	server := httptest.NewServer(control)
	defer server.Close()
	good, err := NewPlainClient(server.URL, "source", "cluster-secret", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := good.Reserve(context.Background(), ReserveRequest{
		SourceCapabilities: testCapabilities("source"), Identity: testForkIdentity(), Family: testRemoteFamilyDescriptor(),
	}); err != nil {
		t.Fatalf("authenticated plaintext request: %v", err)
	}
	bad, err := NewPlainClient(server.URL, "source", "wrong", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	identity := testForkIdentity()
	identity.TransactionID = "tx-rejected-token"
	if _, err := bad.Reserve(context.Background(), ReserveRequest{
		SourceCapabilities: testCapabilities("source"), Identity: identity, Family: testRemoteFamilyDescriptor(),
	}); err == nil || !strings.Contains(err.Error(), "authentication token") {
		t.Fatalf("wrong token error=%v", err)
	}
	if _, err := NewPlainClient(server.URL, "source", "", 5*time.Second); err == nil {
		t.Fatal("a client without a token was made")
	}
	open := &ControlPlane{
		Capabilities: testCapabilities("target"), Store: store, MaxTransactions: 2,
		Now: time.Now, TargetRuntime: &testTargetRuntime{}, CommandProxy: control.CommandProxy,
	}
	tokenless := httptest.NewServer(open)
	defer tokenless.Close()
	for _, url := range []string{server.URL, tokenless.URL} {
		for _, authorization := range []string{"", "Bearer ", "Bearer wrong"} {
			request, err := http.NewRequest(http.MethodPost, url+controlProxyPath, strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set(controlNodeHeader, "source")
			if authorization != "" {
				request.Header.Set("Authorization", authorization)
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusUnauthorized || proxied {
				t.Fatalf("daemon proxy with authorization %q: status %d, proxied %t", authorization, response.StatusCode, proxied)
			}
		}
	}
}

// An incarnation whose lease expired joins again when it returns. Its
// registration must take effect, not be answered from the join it first made:
// its record would stay offline. A retry of a join, once registered,
// registers nothing.
func TestAuthorityJoinRegistersAReturningIncarnation(t *testing.T) {
	node := newTestHolder(t).node
	lease := 50 * time.Millisecond
	propose := func(id string, kind AuthorityCommandKind, payload any) {
		t.Helper()
		result, err := node.Propose(context.Background(), id, kind, payload)
		if err != nil || result.Err() != nil {
			t.Fatalf("applying %s: result=%+v err=%v", kind, result, err)
		}
	}
	propose("policy", AuthorityClusterPolicySet, AuthorityClusterPolicyPayload{Policy: ClusterRuntimePolicy{
		Version: 1, PagingPolicy: testCapabilities("node-a").PagingPolicy, LeaseDuration: lease, MaxTransactions: 32,
	}})
	service := &AuthorityService{Node: node, Capabilities: testCapabilities("node-a"), ClusterID: "cluster"}
	join := func() NodeRecord {
		t.Helper()
		body, err := json.Marshal(AuthorityJoinRequest{
			NodeID: "node-b", Incarnation: "incarnation-b", ControlURL: "http://127.0.0.1:2",
			ClusterID: "cluster", Capabilities: testCapabilities("node-b"),
		})
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		service.serveHTTP(response, httptest.NewRequest(http.MethodPost, controlAuthorityJoinPath, bytes.NewReader(body)), "node-b")
		if response.Code != http.StatusOK {
			t.Fatalf("joining: %d %s", response.Code, response.Body)
		}
		state, err := node.LinearizableState(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return state.Nodes["node-b"]
	}

	joined := join()
	if joined.State != NodeJoining {
		t.Fatalf("joined as %q, want %q", joined.State, NodeJoining)
	}
	time.Sleep(2 * lease)
	propose("expire", AuthorityNodeExpire, AuthorityNodeExpirePayload{NodeID: "node-b", Incarnation: "incarnation-b", Revision: joined.Revision})
	returned := join()
	if returned.State != NodeJoining || returned.Revision <= joined.Revision {
		t.Fatalf("returned as %q at revision %d, want %q after revision %d", returned.State, returned.Revision, NodeJoining, joined.Revision)
	}
	if retried := join(); retried.Revision != returned.Revision {
		t.Fatalf("a retried join registered again: revision %d, was %d", retried.Revision, returned.Revision)
	}
}
