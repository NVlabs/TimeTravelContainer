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
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDaemonMutationRecordReplay(t *testing.T) {
	root := t.TempDir()
	id, err := newDaemonRequestID()
	if err != nil {
		t.Fatal(err)
	}
	request := daemonRequest{
		Version: daemonAPIVersion, Action: "command", RequestID: id,
		Args: []string{"pause", "demo"},
	}
	record, replay, err := beginDaemonMutation(root, request)
	if err != nil || replay {
		t.Fatalf("beginDaemonMutation: replay=%t err=%v", replay, err)
	}
	if _, _, err := beginDaemonMutation(root, request); err == nil || !strings.Contains(err.Error(), "indeterminate outcome") {
		t.Fatalf("second pending begin: %v", err)
	}
	record, err = completeDaemonMutation(root, record, 7, []byte("output\n"), []byte("warning\n"))
	if err != nil {
		t.Fatal(err)
	}
	got, replay, err := beginDaemonMutation(root, request)
	if err != nil || !replay {
		t.Fatalf("completed replay: replay=%t err=%v", replay, err)
	}
	if got.Status != 7 || !bytes.Equal(got.Stdout, record.Stdout) || !bytes.Equal(got.Stderr, record.Stderr) {
		t.Fatalf("replayed record=%+v, want %+v", got, record)
	}

	request.Args = []string{"resume", "demo"}
	if _, _, err := beginDaemonMutation(root, request); err == nil || !strings.Contains(err.Error(), "reused for a different command") {
		t.Fatalf("request ID reuse: %v", err)
	}
}

func TestReplayCompletedDaemonMutationDoesNotCreateIntent(t *testing.T) {
	root := t.TempDir()
	request := daemonRequest{Version: daemonAPIVersion, Action: "command", RequestID: "00112233445566778899aabbccddeeff", Args: []string{"rm", "family@branch"}}
	if _, replay, err := replayCompletedDaemonMutation(root, request); err != nil || replay {
		t.Fatalf("replayCompletedDaemonMutation(fresh) replay=%t err=%v", replay, err)
	}
	path, err := daemonMutationPath(root, request.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fresh replay probe created journal state: %v", err)
	}
	record, replay, err := beginDaemonMutation(root, request)
	if err != nil || replay {
		t.Fatalf("beginDaemonMutation replay=%t err=%v", replay, err)
	}
	record, err = completeDaemonMutation(root, record, 0, []byte("family@branch\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, replay, err := replayCompletedDaemonMutation(root, request)
	if err != nil || !replay {
		t.Fatalf("replayCompletedDaemonMutation(completed) replay=%t err=%v", replay, err)
	}
	if got.Status != record.Status || string(got.Stdout) != string(record.Stdout) {
		t.Fatalf("replayed record = %+v, want %+v", got, record)
	}
}

func TestCompactDaemonMutationsRetainsNewestCompletedAndAllPending(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "daemon", "requests")
	base := time.Now().Add(-time.Hour)
	ids := make([]string, 4)
	for i := range ids {
		ids[i] = fmt.Sprintf("%032x", i+1)
		record := daemonMutationRecord{
			Version: daemonMutationSchemaVersion, RequestID: ids[i],
			Fingerprint: fmt.Sprintf("fingerprint-%d", i), State: "completed",
			CreatedAt:   base.Add(time.Duration(i) * time.Minute),
			CompletedAt: base.Add(time.Duration(i) * time.Minute),
		}
		path, err := daemonMutationPath(root, ids[i])
		if err != nil {
			t.Fatal(err)
		}
		if err := writeDurableJSON(path, &record); err != nil {
			t.Fatal(err)
		}
	}
	pendingID := fmt.Sprintf("%032x", 99)
	pendingPath, err := daemonMutationPath(root, pendingID)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDurableJSON(pendingPath, &daemonMutationRecord{
		Version: daemonMutationSchemaVersion, RequestID: pendingID,
		Fingerprint: "pending", State: "pending", CreatedAt: base,
	}); err != nil {
		t.Fatal(err)
	}
	if err := compactDaemonMutations(root, 2); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("journal entries=%d, want two completed plus one pending", len(entries))
	}
	for _, id := range []string{ids[2], ids[3], pendingID} {
		path, _ := daemonMutationPath(root, id)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("retained record %s: %v", id, err)
		}
	}
	for _, id := range ids[:2] {
		path, _ := daemonMutationPath(root, id)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("old record %s was not removed: %v", id, err)
		}
	}
	if err := compactDaemonMutations(root, -1); err == nil {
		t.Fatal("negative retention was accepted")
	}
}

func TestDaemonMutationCompactionIsBatched(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("%032x", i+1)
		path, err := daemonMutationPath(root, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeDurableJSON(path, &daemonMutationRecord{
			Version: daemonMutationSchemaVersion, RequestID: id, Fingerprint: id,
			State: "completed", CreatedAt: time.Unix(int64(i+1), 0), CompletedAt: time.Unix(int64(i+1), 0),
		}); err != nil {
			t.Fatal(err)
		}
	}
	server := &daemonServer{}
	server.mutationCount.Store(daemonMutationCompactionInterval - 2)
	server.maybeCompactDaemonMutations(root)
	entries, err := os.ReadDir(filepath.Join(root, "daemon", "requests"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("non-boundary completion compacted history: %d entries", len(entries))
	}
	// Use a deliberately small retention directly to prove the boundary
	// counter without constructing thousands of records.
	server.mutationCount.Store(daemonMutationCompactionInterval - 1)
	if got := server.mutationCount.Add(1) % daemonMutationCompactionInterval; got != 0 {
		t.Fatalf("compaction boundary remainder=%d", got)
	}
	if err := compactDaemonMutations(root, 2); err != nil {
		t.Fatal(err)
	}
	entries, err = os.ReadDir(filepath.Join(root, "daemon", "requests"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("boundary compaction retained %d entries, want 2", len(entries))
	}
}

func TestResolveDaemonMutationIsExplicitAndNeverReexecutes(t *testing.T) {
	root := t.TempDir()
	id := fmt.Sprintf("%032x", 42)
	request := daemonRequest{
		Version: daemonAPIVersion, Action: "command", RequestID: id,
		Args: []string{"image", "rm", "important"},
	}
	record, replay, err := beginDaemonMutation(root, request)
	if err != nil || replay {
		t.Fatalf("beginDaemonMutation()=(%+v, %t, %v)", record, replay, err)
	}
	if got := strings.Join(record.Command, " "); got != "image rm important" {
		t.Fatalf("recorded command=%q", got)
	}
	record, err = resolveDaemonMutation(root, id, "succeeded", "verified image metadata")
	if err != nil {
		t.Fatal(err)
	}
	if record.State != "completed" || record.Status != 0 || record.Resolution != "succeeded" || record.Note != "verified image metadata" {
		t.Fatalf("resolved record=%+v", record)
	}
	if !strings.Contains(string(record.Stderr), "not re-executed") {
		t.Fatalf("resolution diagnostic=%q", record.Stderr)
	}
	if _, err := resolveDaemonMutation(root, id, "failed", ""); err == nil {
		t.Fatal("completed mutation was resolved twice")
	}
}

func TestDaemonMutationControlListInspectAndResolve(t *testing.T) {
	root := t.TempDir()
	id := fmt.Sprintf("%032x", 77)
	request := daemonRequest{
		Version: daemonAPIVersion, Action: "command", RequestID: id,
		Args: []string{"pause", "demo"},
	}
	if _, _, err := beginDaemonMutation(root, request); err != nil {
		t.Fatal(err)
	}
	server := &daemonServer{}
	var stdout, stderr bytes.Buffer
	if status := server.handleMutationControl(root, []string{"requests", "--state", "pending"}, &stdout, &stderr); status != 0 {
		t.Fatalf("list status=%d stderr=%q", status, stderr.String())
	}
	if output := stdout.String(); !strings.Contains(output, id) || !strings.Contains(output, "pause demo") {
		t.Fatalf("list output=%q", output)
	}
	stdout.Reset()
	stderr.Reset()
	if status := server.handleMutationControl(root, []string{"request", "inspect", "--json", id}, &stdout, &stderr); status != 0 {
		t.Fatalf("inspect status=%d stderr=%q", status, stderr.String())
	}
	if !strings.Contains(stdout.String(), "demo") {
		t.Fatalf("inspect output=%q", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if status := server.handleMutationControl(root, []string{"request", "resolve", "--outcome", "abandoned", "--note", "state unclear", id}, &stdout, &stderr); status != 0 {
		t.Fatalf("resolve status=%d stderr=%q", status, stderr.String())
	}
	record, err := inspectDaemonMutation(root, id)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != 125 || record.Resolution != "abandoned" || record.Note != "state unclear" {
		t.Fatalf("resolved record=%+v", record)
	}
}

func TestDaemonMutationRequestIDValidation(t *testing.T) {
	for _, id := range []string{"", "not-hex", "00"} {
		if _, err := daemonMutationPath(t.TempDir(), id); err == nil {
			t.Errorf("daemonMutationPath accepted %q", id)
		}
	}
}
