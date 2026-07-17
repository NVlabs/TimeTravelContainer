// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	daemonMutationSchemaVersion = 1
	daemonMutationRetention     = 2048
)

type daemonMutationRecord struct {
	Version     int       `json:"version"`
	RequestID   string    `json:"request_id"`
	Fingerprint string    `json:"fingerprint"`
	Command     []string  `json:"command,omitempty"`
	State       string    `json:"state"`
	Status      int       `json:"status,omitempty"`
	Stdout      []byte    `json:"stdout,omitempty"`
	Stderr      []byte    `json:"stderr,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
	Resolution  string    `json:"resolution,omitempty"`
	Note        string    `json:"note,omitempty"`
}

func newDaemonRequestID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("generating daemon request ID: %w", err)
	}
	return hex.EncodeToString(id[:]), nil
}

func daemonMutationFingerprint(request daemonRequest) (string, error) {
	value := struct {
		Version int               `json:"version"`
		Action  string            `json:"action"`
		Options daemonWireOptions `json:"options"`
		Args    []string          `json:"args"`
		Stdin   bool              `json:"stdin"`
	}{request.Version, request.Action, request.Options, request.Args, request.Stdin}
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func daemonMutationPath(dataRoot, requestID string) (string, error) {
	decoded, err := hex.DecodeString(requestID)
	if err != nil || len(decoded) != 16 {
		return "", fmt.Errorf("invalid daemon request ID %q", requestID)
	}
	if dataRoot == "" {
		return "", errors.New("idempotent daemon mutation requires a data root")
	}
	return filepath.Join(dataRoot, "daemon", "requests", requestID+".json"), nil
}

func loadDaemonMutation(path string) (daemonMutationRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return daemonMutationRecord{}, err
	}
	var record daemonMutationRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return daemonMutationRecord{}, err
	}
	if record.Version != daemonMutationSchemaVersion {
		return daemonMutationRecord{}, fmt.Errorf("unsupported daemon mutation record version %d", record.Version)
	}
	if record.State != "pending" && record.State != "completed" {
		return daemonMutationRecord{}, fmt.Errorf("invalid daemon mutation state %q", record.State)
	}
	return record, nil
}

func writeDurableJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0600); err != nil {
		temp.Close()
		return err
	}
	if err := writeDaemonBytes(temp, data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempName, path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = directory.Sync()
	closeErr := directory.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func beginDaemonMutation(dataRoot string, request daemonRequest) (daemonMutationRecord, bool, error) {
	path, err := daemonMutationPath(dataRoot, request.RequestID)
	if err != nil {
		return daemonMutationRecord{}, false, err
	}
	fingerprint, err := daemonMutationFingerprint(request)
	if err != nil {
		return daemonMutationRecord{}, false, err
	}
	record, err := loadDaemonMutation(path)
	if err == nil {
		if record.RequestID != request.RequestID || record.Fingerprint != fingerprint {
			return daemonMutationRecord{}, false, fmt.Errorf("daemon request ID %s was reused for a different command", request.RequestID)
		}
		if record.State == "completed" {
			return record, true, nil
		}
		return daemonMutationRecord{}, false, fmt.Errorf("daemon request %s has an indeterminate outcome after daemon interruption; inspect state before retrying with a new request", request.RequestID)
	}
	if !os.IsNotExist(err) {
		return daemonMutationRecord{}, false, fmt.Errorf("loading daemon mutation record: %w", err)
	}
	record = daemonMutationRecord{
		Version: daemonMutationSchemaVersion, RequestID: request.RequestID,
		Fingerprint: fingerprint, Command: append([]string(nil), request.Args...),
		State: "pending", CreatedAt: time.Now().UTC(),
	}
	if err := writeDurableJSON(path, &record); err != nil {
		return daemonMutationRecord{}, false, fmt.Errorf("persisting daemon mutation intent: %w", err)
	}
	return record, false, nil
}

func completeDaemonMutation(dataRoot string, record daemonMutationRecord, status int, stdout, stderr []byte) (daemonMutationRecord, error) {
	path, err := daemonMutationPath(dataRoot, record.RequestID)
	if err != nil {
		return daemonMutationRecord{}, err
	}
	record.State = "completed"
	record.Status = status
	record.Stdout = append([]byte(nil), stdout...)
	record.Stderr = append([]byte(nil), stderr...)
	record.CompletedAt = time.Now().UTC()
	if err := writeDurableJSON(path, &record); err != nil {
		return daemonMutationRecord{}, fmt.Errorf("persisting daemon mutation result: %w", err)
	}
	_ = compactDaemonMutations(dataRoot, daemonMutationRetention)
	return record, nil
}

func listDaemonMutations(dataRoot, state string) ([]daemonMutationRecord, error) {
	if state != "" && state != "pending" && state != "completed" {
		return nil, fmt.Errorf("invalid daemon mutation state %q", state)
	}
	dir := filepath.Join(dataRoot, "daemon", "requests")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	records := make([]daemonMutationRecord, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		record, err := loadDaemonMutation(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("loading mutation record %s: %w", entry.Name(), err)
		}
		if state == "" || record.State == state {
			records = append(records, record)
		}
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].CreatedAt.Equal(records[j].CreatedAt) {
			return records[i].RequestID < records[j].RequestID
		}
		return records[i].CreatedAt.After(records[j].CreatedAt)
	})
	return records, nil
}

func inspectDaemonMutation(dataRoot, requestID string) (daemonMutationRecord, error) {
	path, err := daemonMutationPath(dataRoot, requestID)
	if err != nil {
		return daemonMutationRecord{}, err
	}
	record, err := loadDaemonMutation(path)
	if os.IsNotExist(err) {
		return daemonMutationRecord{}, fmt.Errorf("daemon mutation request %s does not exist", requestID)
	}
	return record, err
}

func resolveDaemonMutation(dataRoot, requestID, outcome, note string) (daemonMutationRecord, error) {
	record, err := inspectDaemonMutation(dataRoot, requestID)
	if err != nil {
		return daemonMutationRecord{}, err
	}
	if record.State != "pending" {
		return daemonMutationRecord{}, fmt.Errorf("daemon mutation request %s is already %s", requestID, record.State)
	}
	switch outcome {
	case "succeeded":
		record.Status = 0
	case "failed":
		record.Status = 1
	case "abandoned":
		record.Status = 125
	default:
		return daemonMutationRecord{}, fmt.Errorf("invalid resolution outcome %q (want succeeded, failed, or abandoned)", outcome)
	}
	record.State = "completed"
	record.CompletedAt = time.Now().UTC()
	record.Resolution = outcome
	record.Note = note
	if len(record.Stderr) == 0 {
		record.Stderr = []byte(fmt.Sprintf("ttosd: interrupted mutation was resolved by an operator as %s; it was not re-executed\n", outcome))
	}
	path, err := daemonMutationPath(dataRoot, requestID)
	if err != nil {
		return daemonMutationRecord{}, err
	}
	if err := writeDurableJSON(path, &record); err != nil {
		return daemonMutationRecord{}, fmt.Errorf("persisting daemon mutation resolution: %w", err)
	}
	_ = compactDaemonMutations(dataRoot, daemonMutationRetention)
	return record, nil
}

func compactDaemonMutations(dataRoot string, keepCompleted int) error {
	if keepCompleted < 0 {
		return fmt.Errorf("completed daemon mutation retention must not be negative")
	}
	dir := filepath.Join(dataRoot, "daemon", "requests")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	type completedMutation struct {
		path string
		time time.Time
	}
	completed := make([]completedMutation, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		record, err := loadDaemonMutation(path)
		if err != nil || record.State != "completed" {
			continue
		}
		when := record.CompletedAt
		if when.IsZero() {
			when = record.CreatedAt
		}
		completed = append(completed, completedMutation{path: path, time: when})
	}
	if len(completed) <= keepCompleted {
		return nil
	}
	sort.Slice(completed, func(i, j int) bool {
		if completed[i].time.Equal(completed[j].time) {
			return completed[i].path < completed[j].path
		}
		return completed[i].time.Before(completed[j].time)
	})
	for _, entry := range completed[:len(completed)-keepCompleted] {
		if err := os.Remove(entry.path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = directory.Sync()
	closeErr := directory.Close()
	if err != nil {
		return err
	}
	return closeErr
}
