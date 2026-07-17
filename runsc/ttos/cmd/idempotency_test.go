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
		Options: daemonWireOptions{DataRoot: root}, Args: []string{"pause", "demo"},
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

func TestDaemonMutationRequestIDValidation(t *testing.T) {
	for _, id := range []string{"", "not-hex", "00"} {
		if _, err := daemonMutationPath(t.TempDir(), id); err == nil {
			t.Errorf("daemonMutationPath accepted %q", id)
		}
	}
}
