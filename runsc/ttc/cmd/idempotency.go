// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	daemonMutationSchemaVersion = 1
	daemonMutationRetention     = 2048
	// Directory enumeration and JSON parsing are O(retained requests). Keep
	// them off the foreground path while still bounding durable replay history.
	daemonMutationCompactionInterval = 128
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
		Version int      `json:"version"`
		Action  string   `json:"action"`
		Args    []string `json:"args"`
		Stdin   bool     `json:"stdin"`
	}{request.Version, request.Action, request.Args, request.Stdin}
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func validateDaemonRequestID(requestID string) error {
	decoded, err := hex.DecodeString(requestID)
	if err != nil || len(decoded) != 16 {
		return fmt.Errorf("invalid daemon request ID %q", requestID)
	}
	return nil
}

// loadDaemonMutation reads a record an earlier daemon kept as a file of its
// own, at path; the journal takes them in.
func loadDaemonMutation(path string) (daemonMutationRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return daemonMutationRecord{}, err
	}
	var record daemonMutationRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return daemonMutationRecord{}, err
	}
	if err := validateDaemonMutationRecord(record); err != nil {
		return daemonMutationRecord{}, err
	}
	return record, nil
}

func validateDaemonMutationRecord(record daemonMutationRecord) error {
	if record.Version != daemonMutationSchemaVersion {
		return fmt.Errorf("unsupported daemon mutation record version %d", record.Version)
	}
	if record.State != "pending" && record.State != "completed" {
		return fmt.Errorf("invalid daemon mutation state %q", record.State)
	}
	return validateDaemonRequestID(record.RequestID)
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
	if err := validateDaemonRequestID(request.RequestID); err != nil {
		return daemonMutationRecord{}, false, err
	}
	fingerprint, err := daemonMutationFingerprint(request)
	if err != nil {
		return daemonMutationRecord{}, false, err
	}
	journal, err := openMutationJournal(dataRoot)
	if err != nil {
		return daemonMutationRecord{}, false, fmt.Errorf("loading daemon mutation record: %w", err)
	}
	record, ok, err := journal.lookup(request.RequestID)
	if err != nil {
		return daemonMutationRecord{}, false, fmt.Errorf("loading daemon mutation record: %w", err)
	}
	if ok {
		if record.RequestID != request.RequestID || record.Fingerprint != fingerprint {
			return daemonMutationRecord{}, false, fmt.Errorf("daemon request ID %s was reused for a different command", request.RequestID)
		}
		if record.State == "completed" {
			return record, true, nil
		}
		return daemonMutationRecord{}, false, fmt.Errorf("daemon request %s has an indeterminate outcome after daemon interruption; inspect state before retrying with a new request", request.RequestID)
	}
	record = daemonMutationRecord{
		Version: daemonMutationSchemaVersion, RequestID: request.RequestID,
		Fingerprint: fingerprint, Command: append([]string(nil), request.Args...),
		State: "pending", CreatedAt: time.Now().UTC(),
	}
	if err := journal.append(record); err != nil {
		return daemonMutationRecord{}, false, fmt.Errorf("persisting daemon mutation intent: %w", err)
	}
	return record, false, nil
}

// replayCompletedDaemonMutation returns a previously committed result without
// creating a new journal entry. Callers use this before validating mutable
// routing and lifecycle state: a successful mutation may have intentionally
// removed the very placement or runtime that guarded its first execution, and
// a lost response must still replay that committed result. Fresh requests are
// journaled only after ordinary preflight succeeds.
func replayCompletedDaemonMutation(dataRoot string, request daemonRequest) (daemonMutationRecord, bool, error) {
	if err := validateDaemonRequestID(request.RequestID); err != nil {
		return daemonMutationRecord{}, false, err
	}
	fingerprint, err := daemonMutationFingerprint(request)
	if err != nil {
		return daemonMutationRecord{}, false, err
	}
	journal, err := openMutationJournal(dataRoot)
	if err != nil {
		return daemonMutationRecord{}, false, fmt.Errorf("loading daemon mutation record: %w", err)
	}
	record, ok, err := journal.lookup(request.RequestID)
	if err != nil {
		return daemonMutationRecord{}, false, fmt.Errorf("loading daemon mutation record: %w", err)
	}
	if !ok {
		return daemonMutationRecord{}, false, nil
	}
	if record.RequestID != request.RequestID || record.Fingerprint != fingerprint {
		return daemonMutationRecord{}, false, fmt.Errorf("daemon request ID %s was reused for a different command", request.RequestID)
	}
	if record.State != "completed" {
		return daemonMutationRecord{}, false, nil
	}
	return record, true, nil
}

func completeDaemonMutation(dataRoot string, record daemonMutationRecord, status int, stdout, stderr []byte) (daemonMutationRecord, error) {
	if err := validateDaemonRequestID(record.RequestID); err != nil {
		return daemonMutationRecord{}, err
	}
	journal, err := openMutationJournal(dataRoot)
	if err != nil {
		return daemonMutationRecord{}, fmt.Errorf("persisting daemon mutation result: %w", err)
	}
	record.State = "completed"
	record.Status = status
	record.Stdout = append([]byte(nil), stdout...)
	record.Stderr = append([]byte(nil), stderr...)
	record.CompletedAt = time.Now().UTC()
	if err := journal.append(record); err != nil {
		return daemonMutationRecord{}, fmt.Errorf("persisting daemon mutation result: %w", err)
	}
	return record, nil
}

func listDaemonMutations(dataRoot, state string) ([]daemonMutationRecord, error) {
	if state != "" && state != "pending" && state != "completed" {
		return nil, fmt.Errorf("invalid daemon mutation state %q", state)
	}
	journal, err := openMutationJournal(dataRoot)
	if err != nil {
		return nil, err
	}
	return journal.list(state)
}

func inspectDaemonMutation(dataRoot, requestID string) (daemonMutationRecord, error) {
	if err := validateDaemonRequestID(requestID); err != nil {
		return daemonMutationRecord{}, err
	}
	journal, err := openMutationJournal(dataRoot)
	if err != nil {
		return daemonMutationRecord{}, err
	}
	record, ok, err := journal.lookup(requestID)
	if err != nil {
		return daemonMutationRecord{}, err
	}
	if !ok {
		return daemonMutationRecord{}, fmt.Errorf("daemon mutation request %s does not exist", requestID)
	}
	return record, nil
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
		record.Stderr = []byte(fmt.Sprintf("ttcd: interrupted mutation was resolved by an operator as %s; it was not re-executed\n", outcome))
	}
	journal, err := openMutationJournal(dataRoot)
	if err != nil {
		return daemonMutationRecord{}, err
	}
	if err := journal.append(record); err != nil {
		return daemonMutationRecord{}, fmt.Errorf("persisting daemon mutation resolution: %w", err)
	}
	_ = compactDaemonMutations(dataRoot, daemonMutationRetention)
	return record, nil
}

func compactDaemonMutations(dataRoot string, keepCompleted int) error {
	if keepCompleted < 0 {
		return fmt.Errorf("completed daemon mutation retention must not be negative")
	}
	journal, err := openMutationJournal(dataRoot)
	if err != nil {
		return err
	}
	return journal.compact(keepCompleted)
}
