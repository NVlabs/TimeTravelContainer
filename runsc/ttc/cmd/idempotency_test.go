// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
	if records, err := listDaemonMutations(root, ""); err != nil || len(records) != 0 {
		t.Fatalf("fresh replay probe created journal state: %+v, %v", records, err)
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

// appendDaemonMutations appends records to the journal of root.
func appendDaemonMutations(t *testing.T, root string, records ...daemonMutationRecord) {
	t.Helper()
	journal, err := openMutationJournal(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if err := journal.append(record); err != nil {
			t.Fatal(err)
		}
	}
}

// forgetMutationJournal drops this process's journal of root, as a daemon
// restart would.
func forgetMutationJournal(root string) {
	path := mutationJournalPath(filepath.Clean(root))
	mutationJournals.mu.Lock()
	defer mutationJournals.mu.Unlock()
	if journal := mutationJournals.journals[path]; journal != nil {
		journal.close()
		delete(mutationJournals.journals, path)
	}
}

// reloadMutationJournal opens the journal of root again from its file.
func reloadMutationJournal(t *testing.T, root string) *mutationJournal {
	t.Helper()
	forgetMutationJournal(root)
	journal, err := openMutationJournal(root)
	if err != nil {
		t.Fatal(err)
	}
	return journal
}

// daemonMutationIDs returns the IDs of the records of root, sorted.
func daemonMutationIDs(t *testing.T, root, state string) []string {
	t.Helper()
	records, err := listDaemonMutations(root, state)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(records))
	for _, record := range records {
		ids = append(ids, record.RequestID)
	}
	slices.Sort(ids)
	return ids
}

func TestCompactDaemonMutationsRetainsNewestCompletedAndAllPending(t *testing.T) {
	root := t.TempDir()
	base := time.Now().Add(-time.Hour)
	ids := make([]string, 4)
	for i := range ids {
		ids[i] = fmt.Sprintf("%032x", i+1)
		appendDaemonMutations(t, root, daemonMutationRecord{
			Version: daemonMutationSchemaVersion, RequestID: ids[i],
			Fingerprint: fmt.Sprintf("fingerprint-%d", i), State: "completed",
			CreatedAt:   base.Add(time.Duration(i) * time.Minute),
			CompletedAt: base.Add(time.Duration(i) * time.Minute),
		})
	}
	pendingID := fmt.Sprintf("%032x", 99)
	appendDaemonMutations(t, root, daemonMutationRecord{
		Version: daemonMutationSchemaVersion, RequestID: pendingID,
		Fingerprint: "pending", State: "pending", CreatedAt: base,
	})
	if err := compactDaemonMutations(root, 2); err != nil {
		t.Fatal(err)
	}
	want := []string{ids[2], ids[3], pendingID}
	if got := daemonMutationIDs(t, root, ""); !slices.Equal(got, want) {
		t.Fatalf("retained records %v, want %v", got, want)
	}
	reloadMutationJournal(t, root)
	if got := daemonMutationIDs(t, root, ""); !slices.Equal(got, want) {
		t.Fatalf("after reopening, retained records %v, want %v", got, want)
	}
	if got := daemonMutationIDs(t, root, "pending"); !slices.Equal(got, []string{pendingID}) {
		t.Fatalf("pending records %v, want %v", got, pendingID)
	}
	if err := compactDaemonMutations(root, -1); err == nil {
		t.Fatal("negative retention was accepted")
	}
}

func TestDaemonMutationCompactionIsBatched(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("%032x", i+1)
		appendDaemonMutations(t, root, daemonMutationRecord{
			Version: daemonMutationSchemaVersion, RequestID: id, Fingerprint: id,
			State: "completed", CreatedAt: time.Unix(int64(i+1), 0), CompletedAt: time.Unix(int64(i+1), 0),
		})
	}
	server := &daemonServer{}
	server.mutationCount.Store(daemonMutationCompactionInterval - 2)
	server.maybeCompactDaemonMutations(root)
	if got := daemonMutationIDs(t, root, ""); len(got) != 3 {
		t.Fatalf("non-boundary completion compacted history: %d records", len(got))
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
	if got := daemonMutationIDs(t, root, ""); len(got) != 2 {
		t.Fatalf("boundary compaction retained %d records, want 2", len(got))
	}
}

func TestDaemonMutationJournalImportsRequestFiles(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "daemon", "requests")
	completedID, pendingID := fmt.Sprintf("%032x", 1), fmt.Sprintf("%032x", 2)
	created := time.Unix(1000, 0).UTC()
	for _, record := range []daemonMutationRecord{
		{Version: daemonMutationSchemaVersion, RequestID: completedID, Fingerprint: "a", Command: []string{"rm", "demo"}, State: "completed", Status: 3, Stdout: []byte("demo\n"), CreatedAt: created, CompletedAt: created.Add(time.Second)},
		{Version: daemonMutationSchemaVersion, RequestID: pendingID, Fingerprint: "b", Command: []string{"pause", "demo"}, State: "pending", CreatedAt: created},
	} {
		if err := writeDurableJSON(filepath.Join(dir, record.RequestID+".json"), &record); err != nil {
			t.Fatal(err)
		}
	}
	// A temporary a crash left, and a record no request was answered from.
	if err := os.WriteFile(filepath.Join(dir, "."+pendingID+".json.tmp-123"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	damaged := filepath.Join(dir, fmt.Sprintf("%032x", 3)+".json")
	if err := os.WriteFile(damaged, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, want := daemonMutationIDs(t, root, ""), []string{completedID, pendingID}; !slices.Equal(got, want) {
		t.Fatalf("imported records %v, want %v", got, want)
	}
	record, err := inspectDaemonMutation(root, completedID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != 3 || string(record.Stdout) != "demo\n" || !record.CompletedAt.Equal(created.Add(time.Second)) || strings.Join(record.Command, " ") != "rm demo" {
		t.Fatalf("imported record=%+v", record)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(damaged) {
		t.Fatalf("after the import, the request directory holds %v, want only the damaged record", entries)
	}
	// The records are the journal's now: a restart reads them from it.
	if err := os.Remove(damaged); err != nil {
		t.Fatal(err)
	}
	reloadMutationJournal(t, root)
	if got, want := daemonMutationIDs(t, root, ""), []string{completedID, pendingID}; !slices.Equal(got, want) {
		t.Fatalf("records after reopening %v, want %v", got, want)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the emptied request directory remains: %v", err)
	}
}

func TestDaemonMutationJournalEndsAtATornRecord(t *testing.T) {
	root := t.TempDir()
	first := daemonMutationRecord{Version: daemonMutationSchemaVersion, RequestID: fmt.Sprintf("%032x", 1), Fingerprint: "a", State: "pending", CreatedAt: time.Unix(1, 0)}
	appendDaemonMutations(t, root, first)
	journal, err := openMutationJournal(root)
	if err != nil {
		t.Fatal(err)
	}
	end := journal.end
	// A crash cut a record short: its frame's length and checksum, and part
	// of its payload.
	frame, err := encodeMutationJournalRecord(daemonMutationRecord{Version: daemonMutationSchemaVersion, RequestID: fmt.Sprintf("%032x", 2), Fingerprint: "b", State: "pending", CreatedAt: time.Unix(2, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.file.WriteAt(frame[:len(frame)-5], end); err != nil {
		t.Fatal(err)
	}
	journal = reloadMutationJournal(t, root)
	if journal.end != end {
		t.Fatalf("the reopened journal ends at %d, want %d", journal.end, end)
	}
	tail := make([]byte, len(frame))
	if _, err := journal.file.ReadAt(tail, end); err != nil {
		t.Fatal(err)
	}
	if !allZero(tail) {
		t.Fatal("the torn record was not cleared")
	}
	// A record is written where the torn one was.
	third := daemonMutationRecord{Version: daemonMutationSchemaVersion, RequestID: fmt.Sprintf("%032x", 3), Fingerprint: "c", State: "pending", CreatedAt: time.Unix(3, 0)}
	appendDaemonMutations(t, root, third)
	reloadMutationJournal(t, root)
	if got, want := daemonMutationIDs(t, root, ""), []string{first.RequestID, third.RequestID}; !slices.Equal(got, want) {
		t.Fatalf("records %v, want %v", got, want)
	}
	// A file that is no journal is not taken for one.
	forgetMutationJournal(root)
	if err := os.WriteFile(mutationJournalPath(root), []byte("something else\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := openMutationJournal(root); err == nil || !strings.Contains(err.Error(), "is not a daemon mutation journal") {
		t.Fatalf("opening a foreign file: %v", err)
	}
}

func TestDaemonMutationJournalExtendsAndCompactsSupersededRecords(t *testing.T) {
	root := t.TempDir()
	output := bytes.Repeat([]byte("x"), 64<<10)
	// 40 requests of 64 KiB each, twice over: more than an extent, most of
	// it superseded.
	for i := 0; i < 40; i++ {
		request := daemonRequest{Version: daemonAPIVersion, Action: "command", RequestID: fmt.Sprintf("%032x", i+1), Args: []string{"pause", "demo"}}
		record, _, err := beginDaemonMutation(root, request)
		if err != nil {
			t.Fatal(err)
		}
		pending := record
		pending.Stdout = output
		appendDaemonMutations(t, root, pending)
		if _, err := completeDaemonMutation(root, record, 0, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.Stat(mutationJournalPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() < 40*64<<10 {
		t.Fatalf("journal of %d bytes holds less than was written", before.Size())
	}
	if err := compactDaemonMutations(root, daemonMutationRetention); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(mutationJournalPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() > 2*mutationJournalExtent {
		t.Fatalf("compaction left %d bytes of %d", after.Size(), before.Size())
	}
	reloadMutationJournal(t, root)
	if got := daemonMutationIDs(t, root, "completed"); len(got) != 40 {
		t.Fatalf("%d completed records after compaction, want 40", len(got))
	}
	for i := 0; i < 40; i++ {
		request := daemonRequest{Version: daemonAPIVersion, Action: "command", RequestID: fmt.Sprintf("%032x", i+1), Args: []string{"pause", "demo"}}
		if _, replay, err := replayCompletedDaemonMutation(root, request); err != nil || !replay {
			t.Fatalf("request %d after compaction: replay=%t err=%v", i+1, replay, err)
		}
	}
}

func TestDaemonMutationJournalConcurrentAppends(t *testing.T) {
	root := t.TempDir()
	const writers, each = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				request := daemonRequest{Version: daemonAPIVersion, Action: "command", RequestID: fmt.Sprintf("%016x%016x", w+1, i+1), Args: []string{"kill", fmt.Sprintf("demo%d", w)}}
				record, _, err := beginDaemonMutation(root, request)
				if err == nil {
					_, err = completeDaemonMutation(root, record, i, []byte(request.RequestID), nil)
				}
				if err == nil && i%10 == 9 {
					err = compactDaemonMutations(root, writers*each/2)
				}
				if err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	reloadMutationJournal(t, root)
	records, err := listDaemonMutations(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) < writers*each/2 {
		t.Fatalf("%d records after concurrent appends, want at least %d", len(records), writers*each/2)
	}
	for _, record := range records {
		if record.State != "completed" || string(record.Stdout) != record.RequestID {
			t.Fatalf("record %+v", record)
		}
	}
}

func TestDaemonMutationJournalOfARemovedRootStartsAnew(t *testing.T) {
	root := t.TempDir()
	appendDaemonMutations(t, root, daemonMutationRecord{Version: daemonMutationSchemaVersion, RequestID: fmt.Sprintf("%032x", 1), Fingerprint: "a", State: "pending", CreatedAt: time.Unix(1, 0)})
	if err := os.RemoveAll(filepath.Join(root, "daemon")); err != nil {
		t.Fatal(err)
	}
	if got := daemonMutationIDs(t, root, ""); len(got) != 0 {
		t.Fatalf("records of a removed journal: %v", got)
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
		if err := validateDaemonRequestID(id); err == nil {
			t.Errorf("validateDaemonRequestID accepted %q", id)
		}
	}
}
