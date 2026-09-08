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

package cluster

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type testTargetRuntime struct {
	mu            sync.Mutex
	prepared      int
	published     int
	activated     int
	publishPaused []bool
	drained       int
	handedOff     int
	canceled      int
	independent   int
	prepareErr    error
	cancelErr     error
}

func (r *testTargetRuntime) Reserve(context.Context, *Transaction) error { return nil }

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

func (r *testTargetRuntime) Handoff(context.Context, *Transaction, SparseHandoffDescriptor) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handedOff++
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
		Capabilities: testCapabilities("target"), Store: store, MaxTransactions: 1,
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
	request := ReserveRequest{SourceCapabilities: testCapabilities("source"), Identity: testForkIdentity()}
	tx, err := client.Reserve(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if tx.State != TransactionTargetReserved || tx.Revision != 2 {
		t.Fatalf("reserved transaction=%+v", tx)
	}
	// A lost reserve response may be retried without consuming quota or
	// replacing the existing identity.
	retried, err := client.Reserve(context.Background(), request)
	if err != nil || retried.Revision != tx.Revision {
		t.Fatalf("idempotent reserve=%+v err=%v", retried, err)
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
		Capabilities: testCapabilities("target"), Store: store, MaxTransactions: 1,
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
	tx, err := client.Reserve(context.Background(), ReserveRequest{SourceCapabilities: testCapabilities("source"), Identity: testForkIdentity()})
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
		Capabilities: testCapabilities("target"), Store: store, MaxTransactions: 1,
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
	tx, err := client.Reserve(context.Background(), ReserveRequest{SourceCapabilities: testCapabilities("source"), Identity: testForkIdentity()})
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
	metrics := TransferMetrics{PullRequests: 7, PullBytes: 4096, MetadataBytes: 512, PagesTotal: 1, PagesReleased: 1}
	independent, err := client.MarkIndependentWithMetrics(context.Background(), tx.Identity.TransactionID, draining.Revision, metrics)
	if err != nil || independent.State != TransactionIndependent || independent.SourceNeeded {
		t.Fatalf("independent=%+v err=%v", independent, err)
	}
	if independent.Metrics == nil || independent.Metrics.PullRequests != 7 || independent.Metrics.TimeToIndependent <= 0 {
		t.Fatalf("independent metrics=%+v", independent.Metrics)
	}
	migrationIdentity := testForkIdentity()
	migrationIdentity.TransactionID = "tx-migration"
	migrationIdentity.TargetContainer = migrationIdentity.SourceContainer
	migrationIdentity.TargetBranch = "tx-migration"
	migrationIdentity.TargetVisibleBranch = migrationIdentity.SourceBranch
	migrationIdentity.Migration = true
	migration, err := client.Reserve(context.Background(), ReserveRequest{SourceCapabilities: testCapabilities("source"), Identity: migrationIdentity})
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
	if migration.Activated || migration.DesiredPaused || migration.State != TransactionPublished {
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
	if runtime.prepared != 2 || runtime.published != 2 || runtime.activated != 1 || runtime.drained != 1 || runtime.independent != 1 || len(runtime.publishPaused) != 2 || !runtime.publishPaused[0] || !runtime.publishPaused[1] {
		t.Fatalf("runtime calls: %+v", runtime)
	}
}

func TestMutualTLSControlPlaneSparseHandoffIsDurableAndIdempotent(t *testing.T) {
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
		Capabilities: testCapabilities("target"), Store: store, MaxTransactions: 1,
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
	tx, err := client.Reserve(context.Background(), ReserveRequest{SourceCapabilities: testCapabilities("source"), Identity: testForkIdentity()})
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
	sparse := SparseHandoffDescriptor{Repository: "cluster/memory-chunks", Manifest: strings.Repeat("12", 32), Branch: "branch-a"}
	draining, err := client.Handoff(context.Background(), tx.Identity.TransactionID, published.Revision, sparse)
	if err != nil || draining.State != TransactionDraining || draining.SparseHandoff == nil || *draining.SparseHandoff != sparse {
		t.Fatalf("handoff=%+v err=%v", draining, err)
	}
	reopened, err := OpenStore(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := reopened.Load(tx.Identity.TransactionID)
	if err != nil || persisted.SparseHandoff == nil || *persisted.SparseHandoff != sparse {
		t.Fatalf("persisted handoff=%+v err=%v", persisted, err)
	}
	// A replay with the pre-handoff revision models a lost HTTP response.
	retried, err := client.Handoff(context.Background(), tx.Identity.TransactionID, published.Revision, sparse)
	if err != nil || retried.Revision != draining.Revision {
		t.Fatalf("handoff retry=%+v err=%v", retried, err)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.handedOff != 1 {
		t.Fatalf("runtime handoff calls=%d, want 1", runtime.handedOff)
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
	tx, err := client.Reserve(context.Background(), ReserveRequest{SourceCapabilities: testCapabilities("source"), Identity: testForkIdentity()})
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
		Capabilities: testCapabilities("target"), Store: store, MaxTransactions: 1, Now: time.Now,
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
		SourceCapabilities: testCapabilities("source"), Identity: testForkIdentity(),
	})
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("certificate mismatch error=%v", err)
	}
}

func TestPlainControlPlaneOptionalToken(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "transactions"))
	if err != nil {
		t.Fatal(err)
	}
	control := &ControlPlane{
		Capabilities: testCapabilities("target"), Store: store, MaxTransactions: 2,
		Now: time.Now, AuthToken: "cluster-secret", TargetRuntime: &testTargetRuntime{},
	}
	server := httptest.NewServer(control)
	defer server.Close()
	good, err := NewPlainClient(server.URL, "source", "cluster-secret", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := good.Reserve(context.Background(), ReserveRequest{
		SourceCapabilities: testCapabilities("source"), Identity: testForkIdentity(),
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
		SourceCapabilities: testCapabilities("source"), Identity: identity,
	}); err == nil || !strings.Contains(err.Error(), "authentication token") {
		t.Fatalf("wrong token error=%v", err)
	}
}
