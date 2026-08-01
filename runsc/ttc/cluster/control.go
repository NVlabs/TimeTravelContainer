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
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
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

type controlError struct {
	Error string `json:"error"`
}

// ControlPlane is the mutually authenticated target-side HTTP handler.
type ControlPlane struct {
	Capabilities    Capabilities
	Store           *Store
	MaxTransactions int
	Now             func() time.Time
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
	writeControlError(w, http.StatusNotFound, "unknown cluster endpoint")
}

func (c *ControlPlane) reserve(w http.ResponseWriter, r *http.Request, peer string) {
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
	writeControlJSON(w, http.StatusCreated, tx)
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
