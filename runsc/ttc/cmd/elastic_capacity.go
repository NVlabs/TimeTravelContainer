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
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gvisor.dev/gvisor/runsc/ttc/cluster"
)

const (
	elasticCapacityRequestVersion  = 1
	elasticCapacityAcceptanceLease = 5 * time.Minute
	elasticCapacityErrorLimit      = 4096
)

// elasticCapacitySink durably accepts an idempotent request. Delivery success
// means that the provisioner owns the request; it does not mean that a node is
// ready. TTC retains the request until membership proves fulfillment.
type elasticCapacitySink interface {
	Deliver(context.Context, elasticCapacityRequest) error
	Configured() bool
}

type elasticCapacitySinkImpl struct {
	command string
	webhook string
	client  *http.Client
}

func (s *elasticCapacitySinkImpl) Configured() bool {
	return s != nil && (s.command != "" || s.webhook != "")
}

func newElasticCapacitySink(command, webhook string) (elasticCapacitySink, error) {
	if command != "" && webhook != "" {
		return nil, errors.New("--cluster-capacity-command and --cluster-capacity-webhook are mutually exclusive")
	}
	if command != "" {
		if !filepath.IsAbs(command) {
			return nil, errors.New("--cluster-capacity-command must be an absolute executable path")
		}
		info, err := os.Stat(command)
		if err != nil {
			return nil, fmt.Errorf("inspecting cluster capacity command: %w", err)
		}
		if !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
			return nil, fmt.Errorf("cluster capacity command %q is not an executable regular file", command)
		}
	}
	if webhook != "" {
		parsed, err := url.Parse(webhook)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
			return nil, fmt.Errorf("invalid cluster capacity webhook %q", webhook)
		}
	}
	return &elasticCapacitySinkImpl{
		command: command,
		webhook: webhook,
		client: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("capacity webhook redirects are not followed")
			},
		},
	}, nil
}

func (s *elasticCapacitySinkImpl) Deliver(ctx context.Context, request elasticCapacityRequest) error {
	if !s.Configured() {
		return errors.New("elastic capacity provider is not configured")
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return err
	}
	if s.command != "" {
		command := exec.CommandContext(ctx, s.command)
		command.Stdin = bytes.NewReader(payload)
		command.Env = append(os.Environ(),
			"TTC_CAPACITY_REQUEST_ID="+request.RequestID,
			"TTC_CAPACITY_SOURCE_NODE="+request.SourceNode,
		)
		var output cappedCapacityOutput
		command.Stdout = &output
		command.Stderr = &output
		if err := command.Run(); err != nil {
			return fmt.Errorf("capacity command: %w: %s", err, strings.TrimSpace(output.String()))
		}
		return nil
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, s.webhook, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Idempotency-Key", request.RequestID)
	response, err := s.client.Do(httpRequest)
	if err != nil {
		return fmt.Errorf("capacity webhook: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, elasticCapacityErrorLimit))
		return fmt.Errorf("capacity webhook returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, elasticCapacityErrorLimit))
	return nil
}

type cappedCapacityOutput struct {
	data []byte
}

func (w *cappedCapacityOutput) Write(data []byte) (int, error) {
	written := len(data)
	remaining := elasticCapacityErrorLimit - len(w.data)
	if remaining > 0 {
		copyBytes := len(data)
		if copyBytes > remaining {
			copyBytes = remaining
		}
		w.data = append(w.data, data[:copyBytes]...)
	}
	return written, nil
}

func (w *cappedCapacityOutput) String() string { return string(w.data) }

func elasticCapacityRequestPath(dataRoot, nodeID string) (string, error) {
	if nodeID == "" || filepath.Base(nodeID) != nodeID || nodeID == "." {
		return "", fmt.Errorf("invalid capacity request node ID %q", nodeID)
	}
	dir := filepath.Join(dataRoot, "cluster", "capacity-requests")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	return filepath.Join(dir, nodeID+".json"), nil
}

func withElasticCapacityRequest(dataRoot, nodeID string, fn func(string, *elasticCapacityRequest) error) error {
	path, err := elasticCapacityRequestPath(dataRoot, nodeID)
	if err != nil {
		return err
	}
	lock, err := os.OpenFile(strings.TrimSuffix(path, ".json")+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	var request elasticCapacityRequest
	contents, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(contents, &request); err != nil {
			return fmt.Errorf("decoding capacity request %q: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return fn(path, &request)
}

func validateElasticCapacityRequest(request elasticCapacityRequest, nodeID string) error {
	if request.Version != elasticCapacityRequestVersion || request.RequestID == "" || request.Operation != "acquire" ||
		request.Nodes <= 0 || request.Nodes > 1024 || request.SourceNode != nodeID || request.Architecture == "" ||
		request.RepositoryID == "" || request.Reason == "" || request.CreatedAt.IsZero() || request.Deadline.Before(request.CreatedAt) {
		return fmt.Errorf("invalid durable capacity request for node %q", nodeID)
	}
	if request.LastAttemptAt != nil && request.LastAttemptAt.Before(request.CreatedAt) ||
		request.NextAttemptAt != nil && request.NextAttemptAt.Before(request.CreatedAt) ||
		request.AcceptedAt != nil && request.AcceptedAt.Before(request.CreatedAt) || len(request.LastError) > elasticCapacityErrorLimit {
		return fmt.Errorf("invalid durable capacity request delivery state for node %q", nodeID)
	}
	return nil
}

func loadElasticCapacityRequest(dataRoot, nodeID string) (*elasticCapacityRequest, error) {
	var result *elasticCapacityRequest
	err := withElasticCapacityRequest(dataRoot, nodeID, func(_ string, request *elasticCapacityRequest) error {
		if request.Version == 0 {
			return nil
		}
		if err := validateElasticCapacityRequest(*request, nodeID); err != nil {
			return err
		}
		copy := *request
		result = &copy
		return nil
	})
	return result, err
}

func capacityRetryDelay(attempts uint64) time.Duration {
	shift := attempts
	if shift > 6 {
		shift = 6
	}
	return time.Duration(uint64(time.Second) << shift)
}

func truncateCapacityError(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	if len(message) > elasticCapacityErrorLimit {
		message = message[:elasticCapacityErrorLimit]
	}
	return message
}

func elasticCapacityFulfilled(request elasticCapacityRequest, source cluster.NodeRecord, records []cluster.NodeRecord, now time.Time) bool {
	for _, candidate := range records {
		if candidate.NodeID == request.SourceNode || !candidate.AcceptsPlacement(now) || !candidate.UpdatedAt.After(request.CreatedAt) {
			continue
		}
		if _, err := cluster.Negotiate(source.Capabilities, candidate.Capabilities); err != nil {
			continue
		}
		if candidate.MemoryTargetBytes == 0 {
			return true
		}
		if candidate.MemoryCurrentBytes > math.MaxUint64-candidate.IncomingReservedBytes {
			continue
		}
		used := candidate.MemoryCurrentBytes + candidate.IncomingReservedBytes
		if used <= candidate.MemoryTargetBytes && request.MemoryBytesEach <= candidate.MemoryTargetBytes-used {
			return true
		}
	}
	return false
}

// processElasticCapacityRequest performs at most one delivery attempt. It is
// intentionally restart-safe: provider calls happen without the file lock and
// the result is committed only if the same request ID is still outstanding.
func processElasticCapacityRequest(ctx context.Context, dataRoot, nodeID string, directory cluster.NodeCatalog, sink elasticCapacitySink, now time.Time) (bool, error) {
	request, err := loadElasticCapacityRequest(dataRoot, nodeID)
	if err != nil || request == nil {
		return false, err
	}
	// The maintainer passes the timestamp delivered with its timer event. A
	// request may be created concurrently after that timer fires but before it
	// acquires the outbox lock. Never persist an attempt timestamp before the
	// request itself; otherwise the next retry correctly rejects the durable
	// record as causally impossible.
	if now.Before(request.CreatedAt) {
		now = request.CreatedAt
	}
	source, err := directory.Lookup(nodeID)
	if err != nil {
		return false, err
	}
	records, err := directory.List()
	if err != nil {
		return false, err
	}
	if elasticCapacityFulfilled(*request, *source, records, now) {
		err := withElasticCapacityRequest(dataRoot, nodeID, func(path string, current *elasticCapacityRequest) error {
			if current.Version == 0 || current.RequestID != request.RequestID {
				return nil
			}
			return os.Remove(path)
		})
		return true, err
	}
	if !sink.Configured() {
		return false, nil
	}
	if request.NextAttemptAt != nil && now.Before(*request.NextAttemptAt) {
		return false, nil
	}
	deliveryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	deliveryErr := sink.Deliver(deliveryCtx, *request)
	cancel()
	err = withElasticCapacityRequest(dataRoot, nodeID, func(path string, current *elasticCapacityRequest) error {
		if current.Version == 0 || current.RequestID != request.RequestID {
			return nil
		}
		current.Attempts++
		attempted := now.UTC()
		current.LastAttemptAt = &attempted
		if deliveryErr != nil {
			next := attempted.Add(capacityRetryDelay(current.Attempts))
			current.NextAttemptAt = &next
			current.LastError = truncateCapacityError(deliveryErr)
		} else {
			current.AcceptedAt = &attempted
			current.Deadline = attempted.Add(elasticCapacityAcceptanceLease)
			next := current.Deadline
			current.NextAttemptAt = &next
			current.LastError = ""
		}
		return writeDurableJSON(path, current)
	})
	return false, errors.Join(deliveryErr, err)
}

func maintainElasticCapacityRequests(dataRoot, nodeID string, directory cluster.NodeCatalog, sink elasticCapacitySink, stderr io.Writer, done <-chan struct{}) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-done:
			return
		case now := <-timer.C:
			_, err := processElasticCapacityRequest(context.Background(), dataRoot, nodeID, directory, sink, now.UTC())
			if err != nil && stderr != nil {
				fmt.Fprintf(stderr, "ttcd: elastic capacity request for node %s needs attention: %v\n", nodeID, err)
			}
			timer.Reset(time.Second)
		}
	}
}
