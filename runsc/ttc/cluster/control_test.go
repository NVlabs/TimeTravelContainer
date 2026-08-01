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
	mu          sync.Mutex
	prepared    int
	published   int
	canceled    int
	independent int
}

func (r *testTargetRuntime) Reserve(context.Context, *Transaction) error { return nil }

func (r *testTargetRuntime) Prepare(context.Context, *Transaction, RemoteSessionDescriptor) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prepared++
	return nil
}

func (r *testTargetRuntime) Publish(context.Context, *Transaction) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.published++
	return nil
}

func (r *testTargetRuntime) Cancel(context.Context, *Transaction) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.canceled++
	return nil
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
	published, err := client.Publish(context.Background(), tx.Identity.TransactionID, prepared.Revision)
	if err != nil || published.State != TransactionPublished {
		t.Fatalf("publish=%+v err=%v", published, err)
	}
	independent, err := client.MarkIndependent(context.Background(), tx.Identity.TransactionID, published.Revision)
	if err != nil || independent.State != TransactionIndependent || independent.SourceNeeded {
		t.Fatalf("independent=%+v err=%v", independent, err)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.prepared != 1 || runtime.published != 1 || runtime.independent != 1 {
		t.Fatalf("runtime calls: %+v", runtime)
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
