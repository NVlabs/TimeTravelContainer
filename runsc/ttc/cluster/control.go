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
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	controlReservePath = "/v1/forks/reserve"
	controlForkPrefix  = "/v1/forks/"
	maxControlBody     = 1 << 20
)

// LoadServerTLSConfig constructs a TLS 1.3 configuration that requires a
// client certificate chaining to clientCAFile.
func LoadServerTLSConfig(certFile, keyFile, clientCAFile string) (*tls.Config, error) {
	certificate, roots, err := loadPeerTLSMaterial(certFile, keyFile, clientCAFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate},
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots,
	}, nil
}

// LoadClientTLSConfig constructs a TLS 1.3 configuration with a client
// certificate and strict server-name validation.
func LoadClientTLSConfig(certFile, keyFile, serverCAFile, serverName string) (*tls.Config, error) {
	if serverName == "" {
		return nil, errors.New("cluster TLS server name is empty")
	}
	certificate, roots, err := loadPeerTLSMaterial(certFile, keyFile, serverCAFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate},
		RootCAs: roots, ServerName: serverName,
	}, nil
}

func loadPeerTLSMaterial(certFile, keyFile, caFile string) (tls.Certificate, *x509.CertPool, error) {
	if certFile == "" || keyFile == "" || caFile == "" {
		return tls.Certificate{}, nil, errors.New("cluster TLS certificate, key, and CA files are required")
	}
	certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("loading cluster TLS key pair: %w", err)
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("reading cluster CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return tls.Certificate{}, nil, errors.New("cluster CA file contains no certificates")
	}
	return certificate, roots, nil
}

// ReserveRequest asks the target to create a hidden transaction. The target
// independently negotiates capabilities rather than trusting a client result.
type ReserveRequest struct {
	SourceCapabilities Capabilities `json:"source_capabilities"`
	Identity           ForkIdentity `json:"identity"`
}

// CancelRequest uses optimistic revision checking.
type CancelRequest struct {
	Revision uint64 `json:"revision"`
}

// RemoteSessionDescriptor is the secret-bearing handoff from the source
// Sentry page server to the target daemon. It is sent only over mutual TLS and
// is never stored in the durable transaction record.
type RemoteSessionDescriptor struct {
	Address          string `json:"address"`
	SnapshotToken    string `json:"snapshot_token"`
	MetadataBytes    uint64 `json:"metadata_bytes"`
	StreamBytes      uint64 `json:"stream_bytes"`
	MaxTransferBytes uint64 `json:"max_transfer_bytes"`
	// SharedMetadata is a clean data-root-relative path containing the
	// authenticated SessionMetadata envelope. It permits restoring a
	// process-free dehydrated source without a source page server.
	SharedMetadata string `json:"shared_metadata,omitempty"`
	// DurableDehydration declares that SharedMetadata is a TTC dehydration
	// envelope rather than a live-fork SessionMetadata envelope.
	DurableDehydration bool `json:"durable_dehydration,omitempty"`
	// SharedSnapshot is a clean repository-relative path containing the
	// immutable packed memory stream. It is non-secret and remains valid after
	// the source Sentry and page server exit.
	SharedSnapshot string `json:"shared_snapshot,omitempty"`
	// SparseRepository names a shared-data-root-relative TTC chunk repository;
	// SparseManifest is the immutable memory graph to demand-load from it.
	SparseRepository string `json:"sparse_repository,omitempty"`
	SparseManifest   string `json:"sparse_manifest,omitempty"`
	SparseBranch     string `json:"sparse_branch,omitempty"`
}

func (s RemoteSessionDescriptor) validate(negotiated NegotiatedCapabilities) error {
	if s.SharedMetadata == "" {
		host, port, err := net.SplitHostPort(s.Address)
		portNumber, portErr := strconv.ParseUint(port, 10, 16)
		if err != nil || host == "" || portErr != nil || portNumber == 0 {
			return fmt.Errorf("invalid remote page address %q", s.Address)
		}
		token, err := hex.DecodeString(s.SnapshotToken)
		if err != nil || len(token) != 16 {
			return errors.New("snapshot token must be 16 bytes encoded as hexadecimal")
		}
	} else {
		clean := filepath.Clean(s.SharedMetadata)
		if filepath.IsAbs(s.SharedMetadata) || clean != s.SharedMetadata || clean == "." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("invalid shared distributed metadata path %q", s.SharedMetadata)
		}
		if s.Address != "" || s.SnapshotToken != "" {
			return errors.New("shared distributed metadata must not include a page-server address or token")
		}
	}
	if s.MetadataBytes == 0 || s.StreamBytes == 0 {
		return errors.New("remote session has an empty metadata or page stream")
	}
	if s.StreamBytes%negotiated.BasePageSize != 0 {
		return fmt.Errorf("remote page stream size %d is not base-page aligned", s.StreamBytes)
	}
	if s.MaxTransferBytes < negotiated.BasePageSize || s.MaxTransferBytes > negotiated.MaxTransferSize || s.MaxTransferBytes%negotiated.BasePageSize != 0 {
		return fmt.Errorf("remote transfer size %d is outside negotiated bounds", s.MaxTransferBytes)
	}
	if s.SharedSnapshot != "" {
		clean := filepath.Clean(s.SharedSnapshot)
		if filepath.IsAbs(s.SharedSnapshot) || clean != s.SharedSnapshot || clean == "." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("invalid shared memory snapshot path %q", s.SharedSnapshot)
		}
	}
	if (s.SparseRepository == "") != (s.SparseManifest == "") ||
		(s.SparseRepository == "") != (s.SparseBranch == "") {
		return errors.New("sparse repository, manifest, and branch must be specified together")
	}
	if s.SparseRepository != "" {
		if s.SharedSnapshot != "" {
			return errors.New("packed and sparse shared snapshots are mutually exclusive")
		}
		if s.SharedMetadata == "" && s.Address == "" {
			return errors.New("sparse memory restore has no metadata source")
		}
		clean := filepath.Clean(s.SparseRepository)
		if filepath.IsAbs(s.SparseRepository) || clean != s.SparseRepository || clean == "." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("invalid sparse memory repository path %q", s.SparseRepository)
		}
		decoded, err := hex.DecodeString(s.SparseManifest)
		if err != nil || len(decoded) != sha256.Size {
			return errors.New("sparse memory manifest must be a SHA-256 hexadecimal ID")
		}
		if s.SparseBranch == "." || s.SparseBranch == ".." || strings.ContainsAny(s.SparseBranch, `/\\`) || strings.TrimSpace(s.SparseBranch) != s.SparseBranch {
			return fmt.Errorf("invalid sparse memory branch %q", s.SparseBranch)
		}
	}
	if s.SharedMetadata != "" && s.SparseRepository == "" {
		return errors.New("shared distributed metadata requires a sparse memory repository")
	}
	if s.DurableDehydration && s.SharedMetadata == "" {
		return errors.New("durable dehydration restore requires shared metadata")
	}
	return nil
}

func (s RemoteSessionDescriptor) digest() (string, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// PrepareRequest asks the target to restore an unpublished, paused container.
type PrepareRequest struct {
	Revision uint64                  `json:"revision"`
	Session  RemoteSessionDescriptor `json:"session"`
}

// SparseHandoffDescriptor identifies a non-secret shared-filesystem memory
// snapshot that can replace a target's live source connection.
type SparseHandoffDescriptor struct {
	Repository string `json:"repository"`
	Manifest   string `json:"manifest"`
	Branch     string `json:"branch"`
}

func (d SparseHandoffDescriptor) validate() error {
	clean := filepath.Clean(d.Repository)
	if d.Repository == "" || filepath.IsAbs(d.Repository) || clean != d.Repository || clean == "." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("invalid sparse memory repository path %q", d.Repository)
	}
	decoded, err := hex.DecodeString(d.Manifest)
	if err != nil || len(decoded) != sha256.Size {
		return errors.New("sparse memory manifest must be a SHA-256 hexadecimal ID")
	}
	if d.Branch == "" || d.Branch == "." || d.Branch == ".." || strings.ContainsAny(d.Branch, `/\\`) || strings.TrimSpace(d.Branch) != d.Branch {
		return fmt.Errorf("invalid sparse memory branch %q", d.Branch)
	}
	return nil
}

// HandoffRequest durably binds and installs a sparse replacement reader.
type HandoffRequest struct {
	Revision uint64                  `json:"revision"`
	Sparse   SparseHandoffDescriptor `json:"sparse"`
}

// RevisionRequest advances a transaction without carrying new secret state.
type RevisionRequest struct {
	Revision uint64           `json:"revision"`
	Metrics  *TransferMetrics `json:"metrics,omitempty"`
}

// PublishRequest controls the initial externally visible target state.
type PublishRequest struct {
	Revision uint64 `json:"revision"`
	Paused   bool   `json:"paused"`
}

// TargetRuntime implements idempotent target-local container operations. Each
// method is keyed by tx.Identity.TransactionID and may be called again after a
// lost response or daemon restart.
type TargetRuntime interface {
	Reserve(context.Context, *Transaction) error
	Prepare(context.Context, *Transaction, RemoteSessionDescriptor) error
	Publish(context.Context, *Transaction, bool) error
	Drain(context.Context, *Transaction) error
	Handoff(context.Context, *Transaction, SparseHandoffDescriptor) error
	Cancel(context.Context, *Transaction) error
	MarkIndependent(context.Context, *Transaction) error
}

type controlError struct {
	Error string `json:"error"`
}

// ControlPlane is the mutually authenticated target-side HTTP handler.
type ControlPlane struct {
	Capabilities    Capabilities
	Store           *Store
	MaxTransactions int
	Now             func() time.Time
	TargetRuntime   TargetRuntime
	operationMu     sync.Mutex // serializes reservation/quota accounting
	transactionMu   sync.Map   // transaction ID -> *sync.Mutex
}

func (c *ControlPlane) lockTransaction(id string) func() {
	value, _ := c.transactionMu.LoadOrStore(id, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// ServeHTTP serves the versioned reservation/status/cancel API. The HTTP
// server must use a configuration returned by LoadServerTLSConfig.
func (c *ControlPlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if c.Store == nil || c.Now == nil || c.MaxTransactions <= 0 {
		writeControlError(w, http.StatusServiceUnavailable, "cluster control plane is not configured")
		return
	}
	peer, err := verifiedPeerNodeID(r)
	if err != nil {
		writeControlError(w, http.StatusUnauthorized, err.Error())
		return
	}
	if r.URL.Path == controlReservePath {
		c.reserve(w, r, peer)
		return
	}
	if !strings.HasPrefix(r.URL.Path, controlForkPrefix) {
		writeControlError(w, http.StatusNotFound, "unknown cluster endpoint")
		return
	}
	remainder := strings.TrimPrefix(r.URL.Path, controlForkPrefix)
	parts := strings.Split(remainder, "/")
	if len(parts) == 0 || !transactionIDPattern.MatchString(parts[0]) {
		writeControlError(w, http.StatusBadRequest, "invalid transaction ID")
		return
	}
	id := parts[0]
	if len(parts) == 1 && r.Method == http.MethodGet {
		c.status(w, id, peer)
		return
	}
	if len(parts) == 2 && parts[1] == "cancel" && r.Method == http.MethodPost {
		c.cancel(w, r, id, peer)
		return
	}
	if len(parts) == 2 && parts[1] == "prepare" && r.Method == http.MethodPost {
		c.prepare(w, r, id, peer)
		return
	}
	if len(parts) == 2 && parts[1] == "publish" && r.Method == http.MethodPost {
		c.publish(w, r, id, peer)
		return
	}
	if len(parts) == 2 && parts[1] == "independent" && r.Method == http.MethodPost {
		c.independent(w, r, id, peer)
		return
	}
	if len(parts) == 2 && parts[1] == "drain" && r.Method == http.MethodPost {
		c.drain(w, r, id, peer)
		return
	}
	if len(parts) == 2 && parts[1] == "handoff" && r.Method == http.MethodPost {
		c.handoff(w, r, id, peer)
		return
	}
	writeControlError(w, http.StatusNotFound, "unknown cluster endpoint")
}

func (c *ControlPlane) reserve(w http.ResponseWriter, r *http.Request, peer string) {
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	if r.Method != http.MethodPost {
		writeControlError(w, http.StatusMethodNotAllowed, "reserve requires POST")
		return
	}
	var request ReserveRequest
	if err := decodeControlJSON(r.Body, &request); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	if request.SourceCapabilities.NodeID != peer || request.Identity.SourceNodeID != peer {
		writeControlError(w, http.StatusForbidden, "client certificate does not match source node ID")
		return
	}
	if request.Identity.TargetNodeID != c.Capabilities.NodeID || request.Identity.RepositoryID != c.Capabilities.RepositoryID {
		writeControlError(w, http.StatusConflict, "target node or repository identity does not match")
		return
	}
	negotiated, err := Negotiate(request.SourceCapabilities, c.Capabilities)
	if err != nil {
		writeControlError(w, http.StatusConflict, err.Error())
		return
	}
	if existing, err := c.Store.Load(request.Identity.TransactionID); err == nil {
		if reflect.DeepEqual(existing.Identity, request.Identity) && reflect.DeepEqual(existing.Negotiated, negotiated) {
			if existing.State == TransactionPreparing {
				if c.TargetRuntime == nil {
					writeControlError(w, http.StatusServiceUnavailable, "target distributed-fork runtime is not configured")
					return
				}
				if err := c.TargetRuntime.Reserve(r.Context(), existing); err != nil {
					writeControlError(w, http.StatusConflict, "reserving target runtime: "+err.Error())
					return
				}
				existing, err = c.Store.Advance(existing.Identity.TransactionID, existing.Revision, TransactionTargetReserved, "", c.Now())
				if err != nil {
					writeControlError(w, http.StatusInternalServerError, err.Error())
					return
				}
			}
			writeControlJSON(w, http.StatusOK, existing)
			return
		}
		writeControlError(w, http.StatusConflict, "transaction ID is already bound to different fork parameters")
		return
	}
	records, err := c.Store.List()
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, err.Error())
		return
	}
	active := 0
	for i := range records {
		if !records[i].Terminal() {
			active++
		}
	}
	if active >= c.MaxTransactions {
		writeControlError(w, http.StatusTooManyRequests, "target distributed-fork transaction quota is exhausted")
		return
	}
	tx, err := NewTransaction(request.Identity, negotiated, c.Now())
	if err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := c.Store.Create(tx); err != nil {
		writeControlError(w, http.StatusConflict, err.Error())
		return
	}
	if c.TargetRuntime == nil {
		writeControlError(w, http.StatusServiceUnavailable, "target distributed-fork runtime is not configured")
		return
	}
	if err := c.TargetRuntime.Reserve(r.Context(), tx); err != nil {
		_, _ = c.Store.Advance(tx.Identity.TransactionID, tx.Revision, TransactionFailed, err.Error(), c.Now())
		writeControlError(w, http.StatusConflict, "reserving target runtime: "+err.Error())
		return
	}
	tx, err = c.Store.Advance(tx.Identity.TransactionID, tx.Revision, TransactionTargetReserved, "", c.Now())
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeControlJSON(w, http.StatusCreated, tx)
}

func (c *ControlPlane) prepare(w http.ResponseWriter, r *http.Request, id, peer string) {
	unlock := c.lockTransaction(id)
	defer unlock()
	if c.TargetRuntime == nil {
		writeControlError(w, http.StatusServiceUnavailable, "target distributed-fork runtime is not configured")
		return
	}
	tx, err := c.Store.Load(id)
	if err != nil {
		writeControlError(w, http.StatusNotFound, err.Error())
		return
	}
	if peer != tx.Identity.SourceNodeID {
		writeControlError(w, http.StatusForbidden, "only the source node may prepare the target")
		return
	}
	var request PrepareRequest
	if err := decodeControlJSON(r.Body, &request); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := request.Session.validate(tx.Negotiated); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	digest, err := request.Session.digest()
	if err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	if tx.SessionDigest != "" && tx.SessionDigest != digest {
		writeControlError(w, http.StatusConflict, "transaction is bound to another data-plane session")
		return
	}
	if tx.State == TransactionTargetReserved {
		tx, err = c.Store.BindSession(id, request.Revision, digest, c.Now())
		if err != nil {
			writeControlError(w, http.StatusConflict, err.Error())
			return
		}
	}
	if tx.State == TransactionSourceCaptured {
		tx, err = c.Store.Advance(id, tx.Revision, TransactionTargetRestoring, "", c.Now())
		if err != nil {
			writeControlError(w, http.StatusConflict, err.Error())
			return
		}
	}
	if tx.State == TransactionTargetRestoring {
		if err := c.TargetRuntime.Prepare(r.Context(), tx, request.Session); err != nil {
			failed, advanceErr := c.Store.Advance(id, tx.Revision, TransactionFailed, err.Error(), c.Now())
			if advanceErr != nil {
				writeControlError(w, http.StatusInternalServerError, fmt.Sprintf("target restore failed: %v; recording failure: %v", err, advanceErr))
				return
			}
			writeControlError(w, http.StatusConflict, "target restore failed: "+failed.Failure)
			return
		}
		tx, err = c.Store.Advance(id, tx.Revision, TransactionTargetPaused, "", c.Now())
		if err != nil {
			writeControlError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if tx.State != TransactionTargetPaused && tx.State != TransactionPublished && tx.State != TransactionDraining && tx.State != TransactionIndependent {
		writeControlError(w, http.StatusConflict, fmt.Sprintf("transaction is %s, not prepared", tx.State))
		return
	}
	writeControlJSON(w, http.StatusOK, tx)
}

func (c *ControlPlane) publish(w http.ResponseWriter, r *http.Request, id, peer string) {
	unlock := c.lockTransaction(id)
	defer unlock()
	if c.TargetRuntime == nil {
		writeControlError(w, http.StatusServiceUnavailable, "target distributed-fork runtime is not configured")
		return
	}
	tx, err := c.Store.Load(id)
	if err != nil {
		writeControlError(w, http.StatusNotFound, err.Error())
		return
	}
	if peer != tx.Identity.SourceNodeID {
		writeControlError(w, http.StatusForbidden, "only the source node may publish the target")
		return
	}
	var request PublishRequest
	if err := decodeControlJSON(r.Body, &request); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	if tx.State == TransactionPublished || tx.State == TransactionDraining || tx.State == TransactionIndependent {
		writeControlJSON(w, http.StatusOK, tx)
		return
	}
	if tx.State != TransactionTargetPaused || tx.Revision != request.Revision {
		writeControlError(w, http.StatusConflict, fmt.Sprintf("transaction is %s revision %d, want %s revision %d", tx.State, tx.Revision, TransactionTargetPaused, request.Revision))
		return
	}
	if err := c.TargetRuntime.Publish(r.Context(), tx, request.Paused); err != nil {
		writeControlError(w, http.StatusConflict, err.Error())
		return
	}
	tx, err = c.Store.Advance(id, tx.Revision, TransactionPublished, "", c.Now())
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeControlJSON(w, http.StatusOK, tx)
}

func (c *ControlPlane) independent(w http.ResponseWriter, r *http.Request, id, peer string) {
	var operation func(context.Context, *Transaction) error
	if c.TargetRuntime != nil {
		operation = c.TargetRuntime.MarkIndependent
	}
	c.advanceRuntime(w, r, id, peer, TransactionPublished, TransactionIndependent, operation)
}

func (c *ControlPlane) drain(w http.ResponseWriter, r *http.Request, id, peer string) {
	var operation func(context.Context, *Transaction) error
	if c.TargetRuntime != nil {
		operation = c.TargetRuntime.Drain
	}
	c.advanceRuntime(w, r, id, peer, TransactionPublished, TransactionDraining, operation)
}

func (c *ControlPlane) handoff(w http.ResponseWriter, r *http.Request, id, peer string) {
	unlock := c.lockTransaction(id)
	defer unlock()
	if c.TargetRuntime == nil {
		writeControlError(w, http.StatusServiceUnavailable, "target distributed-fork runtime is not configured")
		return
	}
	tx, err := c.Store.Load(id)
	if err != nil {
		writeControlError(w, http.StatusNotFound, err.Error())
		return
	}
	if peer != tx.Identity.SourceNodeID {
		writeControlError(w, http.StatusForbidden, "only the source node may hand off target memory")
		return
	}
	var request HandoffRequest
	if err := decodeControlJSON(r.Body, &request); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := request.Sparse.validate(); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	if tx.SparseHandoff != nil && *tx.SparseHandoff != request.Sparse {
		writeControlError(w, http.StatusConflict, "transaction is bound to another sparse handoff")
		return
	}
	if tx.State == TransactionIndependent || tx.State == TransactionDraining {
		if tx.SparseHandoff == nil {
			writeControlError(w, http.StatusConflict, "transaction advanced without a sparse handoff")
			return
		}
		writeControlJSON(w, http.StatusOK, tx)
		return
	}
	if tx.State != TransactionPublished || tx.Revision != request.Revision {
		writeControlError(w, http.StatusConflict, fmt.Sprintf("transaction is %s revision %d, want %s revision %d", tx.State, tx.Revision, TransactionPublished, request.Revision))
		return
	}
	if tx.SparseHandoff == nil {
		tx, err = c.Store.BindSparseHandoff(id, tx.Revision, request.Sparse, c.Now())
		if err != nil {
			writeControlError(w, http.StatusConflict, err.Error())
			return
		}
	}
	if err := c.TargetRuntime.Handoff(r.Context(), tx, request.Sparse); err != nil {
		writeControlError(w, http.StatusConflict, err.Error())
		return
	}
	tx, err = c.Store.Advance(id, tx.Revision, TransactionDraining, "", c.Now())
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeControlJSON(w, http.StatusOK, tx)
}

func (c *ControlPlane) advanceRuntime(w http.ResponseWriter, r *http.Request, id, peer string, required, next TransactionState, operation func(context.Context, *Transaction) error) {
	unlock := c.lockTransaction(id)
	defer unlock()
	if c.TargetRuntime == nil {
		writeControlError(w, http.StatusServiceUnavailable, "target distributed-fork runtime is not configured")
		return
	}
	tx, err := c.Store.Load(id)
	if err != nil {
		writeControlError(w, http.StatusNotFound, err.Error())
		return
	}
	if peer != tx.Identity.SourceNodeID {
		writeControlError(w, http.StatusForbidden, "only the source node may advance the target")
		return
	}
	var request RevisionRequest
	if err := decodeControlJSON(r.Body, &request); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	if tx.State == next || (next == TransactionIndependent && tx.State == TransactionIndependent) {
		writeControlJSON(w, http.StatusOK, tx)
		return
	}
	stateMatches := tx.State == required || (next == TransactionIndependent && tx.State == TransactionDraining)
	if !stateMatches || tx.Revision != request.Revision {
		writeControlError(w, http.StatusConflict, fmt.Sprintf("transaction is %s revision %d, want %s revision %d", tx.State, tx.Revision, required, request.Revision))
		return
	}
	if operation == nil {
		writeControlError(w, http.StatusServiceUnavailable, "target runtime operation is unavailable")
		return
	}
	if err := operation(r.Context(), tx); err != nil {
		writeControlError(w, http.StatusConflict, err.Error())
		return
	}
	if next == TransactionIndependent && request.Metrics != nil {
		metrics := *request.Metrics
		metrics.TimeToIndependent = c.Now().Sub(tx.CreatedAt)
		tx, err = c.Store.AdvanceIndependent(id, tx.Revision, metrics, c.Now())
	} else {
		tx, err = c.Store.Advance(id, tx.Revision, next, "", c.Now())
	}
	if err != nil {
		writeControlError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeControlJSON(w, http.StatusOK, tx)
}

func (c *ControlPlane) status(w http.ResponseWriter, id, peer string) {
	tx, err := c.Store.Load(id)
	if err != nil {
		writeControlError(w, http.StatusNotFound, err.Error())
		return
	}
	if peer != tx.Identity.SourceNodeID && peer != tx.Identity.TargetNodeID {
		writeControlError(w, http.StatusForbidden, "peer does not own transaction")
		return
	}
	writeControlJSON(w, http.StatusOK, tx)
}

func (c *ControlPlane) cancel(w http.ResponseWriter, r *http.Request, id, peer string) {
	unlock := c.lockTransaction(id)
	defer unlock()
	tx, err := c.Store.Load(id)
	if err != nil {
		writeControlError(w, http.StatusNotFound, err.Error())
		return
	}
	if peer != tx.Identity.SourceNodeID && peer != tx.Identity.TargetNodeID {
		writeControlError(w, http.StatusForbidden, "peer does not own transaction")
		return
	}
	var request CancelRequest
	if err := decodeControlJSON(r.Body, &request); err != nil {
		writeControlError(w, http.StatusBadRequest, err.Error())
		return
	}
	if tx.State == TransactionCanceled {
		writeControlJSON(w, http.StatusOK, tx)
		return
	}
	if c.TargetRuntime != nil {
		if err := c.TargetRuntime.Cancel(r.Context(), tx); err != nil {
			writeControlError(w, http.StatusConflict, err.Error())
			return
		}
	}
	tx, err = c.Store.Advance(id, request.Revision, TransactionCanceled, "", c.Now())
	if err != nil {
		writeControlError(w, http.StatusConflict, err.Error())
		return
	}
	writeControlJSON(w, http.StatusOK, tx)
}

func verifiedPeerNodeID(r *http.Request) (string, error) {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
		return "", errors.New("verified client certificate is required")
	}
	nodeID := r.TLS.VerifiedChains[0][0].Subject.CommonName
	if nodeID == "" {
		return "", errors.New("client certificate common name must contain the TTC node ID")
	}
	return nodeID, nil
}

func decodeControlJSON(reader io.Reader, value any) error {
	decoder := json.NewDecoder(io.LimitReader(reader, maxControlBody+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("decoding cluster request: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("cluster request contains trailing JSON")
	}
	return nil
}

func writeControlJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeControlError(w http.ResponseWriter, status int, message string) {
	writeControlJSON(w, status, controlError{Error: message})
}

// Client calls one target ttcd control plane over a mutually authenticated
// HTTP client.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// NewClient constructs a client using tlsConfig for peer authentication.
func NewClient(baseURL string, tlsConfig *tls.Config, timeout time.Duration) (*Client, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Path != "" {
		return nil, fmt.Errorf("invalid cluster peer URL %q", baseURL)
	}
	if tlsConfig == nil || timeout <= 0 {
		return nil, errors.New("cluster client requires TLS and a positive timeout")
	}
	transport := &http.Transport{TLSClientConfig: tlsConfig.Clone()}
	return &Client{BaseURL: strings.TrimSuffix(baseURL, "/"), HTTP: &http.Client{Transport: transport, Timeout: timeout}}, nil
}

// Reserve negotiates and reserves an unpublished target transaction.
func (c *Client) Reserve(ctx context.Context, request ReserveRequest) (*Transaction, error) {
	var tx Transaction
	if err := c.call(ctx, http.MethodPost, controlReservePath, request, &tx); err != nil {
		return nil, err
	}
	return &tx, nil
}

// Status reloads one target transaction after a disconnect or daemon restart.
func (c *Client) Status(ctx context.Context, id string) (*Transaction, error) {
	if !transactionIDPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid transaction ID %q", id)
	}
	var tx Transaction
	if err := c.call(ctx, http.MethodGet, controlForkPrefix+id, nil, &tx); err != nil {
		return nil, err
	}
	return &tx, nil
}

// Cancel requests idempotent target cleanup at revision.
func (c *Client) Cancel(ctx context.Context, id string, revision uint64) (*Transaction, error) {
	if !transactionIDPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid transaction ID %q", id)
	}
	var tx Transaction
	if err := c.call(ctx, http.MethodPost, controlForkPrefix+id+"/cancel", CancelRequest{Revision: revision}, &tx); err != nil {
		return nil, err
	}
	return &tx, nil
}

// Prepare restores a hidden target from a source data-plane session. Retrying
// the identical descriptor is safe after a lost response.
func (c *Client) Prepare(ctx context.Context, id string, revision uint64, session RemoteSessionDescriptor) (*Transaction, error) {
	if !transactionIDPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid transaction ID %q", id)
	}
	var tx Transaction
	if err := c.call(ctx, http.MethodPost, controlForkPrefix+id+"/prepare", PrepareRequest{Revision: revision, Session: session}, &tx); err != nil {
		return nil, err
	}
	return &tx, nil
}

// Publish makes a successfully prepared paused target externally visible.
func (c *Client) Publish(ctx context.Context, id string, revision uint64, paused bool) (*Transaction, error) {
	if !transactionIDPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid transaction ID %q", id)
	}
	var tx Transaction
	if err := c.call(ctx, http.MethodPost, controlForkPrefix+id+"/publish", PublishRequest{Revision: revision, Paused: paused}, &tx); err != nil {
		return nil, err
	}
	return &tx, nil
}

// MarkIndependent records that all remote pages are local and the source may
// be stopped without affecting the target.
func (c *Client) MarkIndependent(ctx context.Context, id string, revision uint64) (*Transaction, error) {
	return c.advance(ctx, id, "independent", revision)
}

// MarkIndependentWithMetrics records final source transfer accounting with the
// independence transition.
func (c *Client) MarkIndependentWithMetrics(ctx context.Context, id string, revision uint64, metrics TransferMetrics) (*Transaction, error) {
	if !transactionIDPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid transaction ID %q", id)
	}
	if err := metrics.Validate(); err != nil {
		return nil, err
	}
	var tx Transaction
	if err := c.call(ctx, http.MethodPost, controlForkPrefix+id+"/independent", RevisionRequest{Revision: revision, Metrics: &metrics}, &tx); err != nil {
		return nil, err
	}
	return &tx, nil
}

// Drain requests eager target materialization so the source may stop safely.
func (c *Client) Drain(ctx context.Context, id string, revision uint64) (*Transaction, error) {
	return c.advance(ctx, id, "drain", revision)
}

// Handoff replaces future live page pulls with a durable sparse manifest.
func (c *Client) Handoff(ctx context.Context, id string, revision uint64, sparse SparseHandoffDescriptor) (*Transaction, error) {
	if !transactionIDPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid transaction ID %q", id)
	}
	if err := sparse.validate(); err != nil {
		return nil, err
	}
	var tx Transaction
	if err := c.call(ctx, http.MethodPost, controlForkPrefix+id+"/handoff", HandoffRequest{Revision: revision, Sparse: sparse}, &tx); err != nil {
		return nil, err
	}
	return &tx, nil
}

func (c *Client) advance(ctx context.Context, id, operation string, revision uint64) (*Transaction, error) {
	if !transactionIDPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid transaction ID %q", id)
	}
	var tx Transaction
	if err := c.call(ctx, http.MethodPost, controlForkPrefix+id+"/"+operation, RevisionRequest{Revision: revision}, &tx); err != nil {
		return nil, err
	}
	return &tx, nil
}

func (c *Client) call(ctx context.Context, method, path string, request, response any) error {
	var body io.Reader
	if request != nil {
		data, err := json.Marshal(request)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, maxControlBody+1)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var remote controlError
		if err := json.NewDecoder(limited).Decode(&remote); err != nil || remote.Error == "" {
			return fmt.Errorf("cluster peer returned HTTP %s", resp.Status)
		}
		return fmt.Errorf("cluster peer: %s", remote.Error)
	}
	if err := json.NewDecoder(limited).Decode(response); err != nil {
		return fmt.Errorf("decoding cluster response: %w", err)
	}
	return nil
}
