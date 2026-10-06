// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestIntentIDBoundsMigrationFilename(t *testing.T) {
	short := intentID("family", "main", "publication-1")
	if strings.HasPrefix(short, "sha256-") {
		t.Fatalf("short intent identity unexpectedly hashed: %q", short)
	}
	long := intentID(strings.Repeat("f", 64), "fork-"+strings.Repeat("a", 32), "publication-"+strings.Repeat("b", 32))
	if !strings.HasPrefix(long, "sha256-") || len(long)+len(".json") > 240 {
		t.Fatalf("long intent identity = %q (%d bytes)", long, len(long))
	}
	if long == intentID(strings.Repeat("f", 64), "fork-"+strings.Repeat("c", 32), "publication-"+strings.Repeat("b", 32)) {
		t.Fatal("distinct long publication identities collided")
	}
}

type testPublisher struct {
	mu        sync.Mutex
	failed    bool
	completed chan Intent
}

func (p *testPublisher) Publish(_ context.Context, intent Intent) (Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failed {
		p.failed = false
		return Result{}, errors.New("injected publication failure")
	}
	return Result{Manifest: intent.ID}, nil
}
func (p *testPublisher) CaptureAvailable(Intent) (bool, error)  { return true, nil }
func (p *testPublisher) Complete(intent Intent, _ Result) error { p.completed <- intent; return nil }
func (p *testPublisher) Failed(Intent, error, time.Time) error  { return nil }
func (p *testPublisher) Cleanup(Intent) error                   { return nil }

type recoveryPublisher struct {
	testPublisher
	available bool
	failed    chan error
	cleaned   chan Intent
}

type blockingPublisher struct {
	started chan struct{}
	release chan struct{}
	cleaned chan struct{}
}

type retryCleanupPublisher struct {
	testPublisher
	mu       sync.Mutex
	attempts int
}

type supersededPublisher struct {
	testPublisher
	cleaned chan Intent
	failed  chan error
}

func (p *supersededPublisher) Publish(context.Context, Intent) (Result, error) {
	return Result{}, fmt.Errorf("%w: replaced by publication-new", ErrPublicationSuperseded)
}

func (p *supersededPublisher) Cleanup(intent Intent) error { p.cleaned <- intent; return nil }
func (p *supersededPublisher) Failed(_ Intent, err error, _ time.Time) error {
	p.failed <- err
	return nil
}

func (p *retryCleanupPublisher) Cleanup(Intent) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.attempts++
	if p.attempts == 1 {
		return errors.New("injected cleanup failure")
	}
	return nil
}

func (p *blockingPublisher) CaptureAvailable(Intent) (bool, error) { return true, nil }
func (p *blockingPublisher) Publish(context.Context, Intent) (Result, error) {
	close(p.started)
	<-p.release
	return Result{}, errors.New("publication interrupted after cancellation")
}
func (*blockingPublisher) Complete(Intent, Result) error         { return nil }
func (*blockingPublisher) Failed(Intent, error, time.Time) error { return nil }
func (p *blockingPublisher) Cleanup(Intent) error                { close(p.cleaned); return nil }

func (p *recoveryPublisher) CaptureAvailable(Intent) (bool, error) { return p.available, nil }
func (p *recoveryPublisher) Failed(_ Intent, err error, _ time.Time) error {
	p.failed <- err
	return nil
}
func (p *recoveryPublisher) Cleanup(intent Intent) error { p.cleaned <- intent; return nil }

func TestQueuePersistsRetriesAndCompletes(t *testing.T) {
	root := t.TempDir()
	publisher := &testPublisher{failed: true, completed: make(chan Intent, 1)}
	queue, err := Open(root, publisher)
	if err != nil {
		t.Fatal(err)
	}
	intent := Intent{
		Container: "physical", Branch: "transaction", AuthorityContainer: "family", AuthorityBranch: "main",
		PublicationID: "publication-7", Kind: "dehydrate", FilesystemBranch: "capture",
	}
	if err := queue.Enqueue(intent); err != nil {
		t.Fatal(err)
	}
	if err := queue.Activate("physical", "transaction", "publication-7"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		pending, err := queue.Pending()
		if err == nil && len(pending) == 1 && pending[0].Attempts == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("publication failure was not persisted: %+v, %v", pending, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	queue.Close()
	pending, err := queue.Pending()
	if err != nil || len(pending) != 1 || pending[0].Attempts != 1 {
		t.Fatalf("pending after failure=%+v, err=%v", pending, err)
	}
	if pending[0].AuthorityContainer != "family" || pending[0].AuthorityBranch != "main" {
		t.Fatalf("stable authority identity was not persisted: %+v", pending[0])
	}
	pending[0].RetryAt = time.Now().Add(-time.Second)
	if err := writeJSONAtomic(queue.path("pending", pending[0].ID), pending[0]); err != nil {
		t.Fatal(err)
	}
	queue, err = Open(root, publisher)
	if err != nil {
		t.Fatal(err)
	}
	defer queue.Close()
	select {
	case completed := <-publisher.completed:
		if completed.PublicationID != "publication-7" {
			t.Fatalf("completed %+v", completed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("recovered queue did not retry")
	}
}

func TestCancelWithoutPendingPublicationIsNoop(t *testing.T) {
	publisher := &testPublisher{completed: make(chan Intent, 1)}
	root := t.TempDir()
	queue, err := Open(root, publisher)
	if err != nil {
		t.Fatal(err)
	}
	defer queue.Close()
	if err := queue.Cancel("family", "reused", "publication-3"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "publication-queue", "canceled"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("cancel without pending work retained markers: %v", entries)
	}
}

func TestCancelBranchRemovesSupersededPublications(t *testing.T) {
	publisher := &testPublisher{failed: true, completed: make(chan Intent, 2)}
	queue, err := Open(t.TempDir(), publisher)
	if err != nil {
		t.Fatal(err)
	}
	defer queue.Close()
	for _, publicationID := range []string{"publication-1", "publication-2"} {
		intent := Intent{Container: "family", Branch: "main", PublicationID: publicationID, Kind: "stop", FilesystemBranch: "capture"}
		if err := queue.Enqueue(intent); err != nil {
			t.Fatal(err)
		}
	}
	if err := queue.Enqueue(Intent{Container: "family", Branch: "peer", PublicationID: "publication-3", Kind: "stop", FilesystemBranch: "capture"}); err != nil {
		t.Fatal(err)
	}
	if canceled, err := queue.CancelBranch("family", "main"); err != nil || canceled != 2 {
		t.Fatalf("CancelBranch = %d, %v, want 2 publications canceled", canceled, err)
	}
	pending, err := queue.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Branch != "peer" {
		t.Fatalf("pending after branch cancellation = %+v, want only peer", pending)
	}
	entries, err := os.ReadDir(filepath.Join(queue.root, "canceled"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("completed branch cancellation retained markers: %v", entries)
	}
}

func TestCancelDuringPublishDoesNotRaceCleanupOrResurrectIntent(t *testing.T) {
	publisher := &blockingPublisher{
		started: make(chan struct{}), release: make(chan struct{}), cleaned: make(chan struct{}),
	}
	queue, err := Open(t.TempDir(), publisher)
	if err != nil {
		t.Fatal(err)
	}
	defer queue.Close()
	intent := Intent{Container: "family", Branch: "main", PublicationID: "publication-1", Kind: "stop", FilesystemBranch: "capture"}
	if err := queue.Enqueue(intent); err != nil {
		t.Fatal(err)
	}
	if err := queue.Activate("family", "main", "publication-1"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-publisher.started:
	case <-time.After(5 * time.Second):
		t.Fatal("publication did not start")
	}
	canceled := make(chan error, 1)
	go func() { canceled <- queue.Cancel("family", "main", "publication-1") }()
	select {
	case <-publisher.cleaned:
		t.Fatal("capture was cleaned while publisher was using it")
	default:
	}
	close(publisher.release)
	select {
	case <-publisher.cleaned:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not clean canceled capture")
	}
	if err := <-canceled; err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		pending, err := queue.Pending()
		if err == nil && len(pending) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("canceled intent was resurrected: pending=%+v, err=%v", pending, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCanceledCleanupRetriesFromDurableIntent(t *testing.T) {
	publisher := &retryCleanupPublisher{testPublisher: testPublisher{completed: make(chan Intent, 1)}}
	queue, err := Open(t.TempDir(), publisher)
	if err != nil {
		t.Fatal(err)
	}
	defer queue.Close()
	intent := Intent{Container: "family", Branch: "main", PublicationID: "publication-9", Kind: "stop", FilesystemBranch: "capture"}
	if err := queue.Enqueue(intent); err != nil {
		t.Fatal(err)
	}
	if err := queue.Cancel("family", "main", "publication-9"); err == nil {
		t.Fatal("Cancel succeeded despite injected cleanup failure")
	}
	pending, err := queue.Pending()
	if err != nil || len(pending) != 1 || pending[0].LastError == "" {
		t.Fatalf("failed canceled cleanup was not retained: pending=%+v err=%v", pending, err)
	}
	queue.signal()
	deadline := time.Now().Add(5 * time.Second)
	for {
		pending, err = queue.Pending()
		if err == nil && len(pending) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("canceled cleanup was not retried: pending=%+v err=%v", pending, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSupersededPublicationIsCleanedWithoutRetry(t *testing.T) {
	publisher := &supersededPublisher{
		testPublisher: testPublisher{completed: make(chan Intent, 1)},
		cleaned:       make(chan Intent, 1), failed: make(chan error, 1),
	}
	queue, err := Open(t.TempDir(), publisher)
	if err != nil {
		t.Fatal(err)
	}
	defer queue.Close()
	intent := Intent{Container: "family", Branch: "main", PublicationID: "publication-old", Kind: "stop", FilesystemBranch: "capture"}
	if err := queue.Enqueue(intent); err != nil {
		t.Fatal(err)
	}
	if err := queue.Activate("family", "main", "publication-old"); err != nil {
		t.Fatal(err)
	}
	select {
	case cleaned := <-publisher.cleaned:
		if cleaned.PublicationID != intent.PublicationID {
			t.Fatalf("cleaned %+v", cleaned)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("superseded capture was not cleaned")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		pending, err := queue.Pending()
		if err == nil && len(pending) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("superseded intent remained pending: %+v, %v", pending, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-publisher.failed:
		t.Fatalf("superseded publication reported retryable failure: %v", err)
	default:
	}
}

func TestOpenRejectsIncompleteRecoveredCapture(t *testing.T) {
	root := t.TempDir()
	publisher := &testPublisher{completed: make(chan Intent, 1)}
	queue, err := Open(root, publisher)
	if err != nil {
		t.Fatal(err)
	}
	intent := Intent{Container: "family", Branch: "main", PublicationID: "publication-9", Kind: "stop", FilesystemBranch: "capture"}
	if err := queue.Enqueue(intent); err != nil {
		t.Fatal(err)
	}
	queue.Close()
	recovery := &recoveryPublisher{
		testPublisher: testPublisher{completed: make(chan Intent, 1)},
		failed:        make(chan error, 1), cleaned: make(chan Intent, 1),
	}
	queue, err = Open(root, recovery)
	if err != nil {
		t.Fatal(err)
	}
	defer queue.Close()
	if pending, err := queue.Pending(); err != nil || len(pending) != 0 {
		t.Fatalf("pending = %+v, %v; want empty", pending, err)
	}
	select {
	case <-recovery.failed:
	default:
		t.Fatal("incomplete capture was not reported")
	}
	select {
	case cleaned := <-recovery.cleaned:
		if cleaned.PublicationID != "publication-9" {
			t.Fatalf("cleaned %+v", cleaned)
		}
	default:
		t.Fatal("incomplete capture was not cleaned")
	}
}

// fencedCapturePublisher's capture is not ready when it is activated; the
// worker's check of it then waits for release, and fails as a publication
// identity the cancellation fenced fails.
type fencedCapturePublisher struct {
	testPublisher
	checks  atomic.Int32
	started chan struct{}
	release chan struct{}
	cleaned chan Intent
}

func (p *fencedCapturePublisher) CaptureAvailable(Intent) (bool, error) {
	if p.checks.Add(1) == 1 {
		return false, nil
	}
	close(p.started)
	<-p.release
	return false, errors.New("publication identity is not usable")
}
func (p *fencedCapturePublisher) Cleanup(intent Intent) error { p.cleaned <- intent; return nil }

func TestCancelFinishesACaptureCheckItFenced(t *testing.T) {
	publisher := &fencedCapturePublisher{
		testPublisher: testPublisher{completed: make(chan Intent, 1)},
		started:       make(chan struct{}), release: make(chan struct{}), cleaned: make(chan Intent, 2),
	}
	queue, err := Open(t.TempDir(), publisher)
	if err != nil {
		t.Fatal(err)
	}
	defer queue.Close()
	intent := Intent{Container: "family", Branch: "main", PublicationID: "publication-3", Kind: "stop", FilesystemBranch: "capture"}
	if err := queue.Enqueue(intent); err != nil {
		t.Fatal(err)
	}
	if err := queue.Activate("family", "main", "publication-3"); err == nil {
		t.Fatal("Activate succeeded before the capture was ready")
	}
	select {
	case <-publisher.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker's capture check did not start")
	}
	canceled := make(chan error, 1)
	go func() { canceled <- queue.Cancel("family", "main", "publication-3") }()
	close(publisher.release)
	if err := <-canceled; err != nil {
		t.Fatalf("Cancel returned the error of the capture check it fenced: %v", err)
	}
	if pending, err := queue.Pending(); err != nil || len(pending) != 0 {
		t.Fatalf("canceled intent remains: pending=%+v err=%v", pending, err)
	}
	if len(publisher.cleaned) != 1 {
		t.Fatalf("canceled capture cleaned %d times, want 1", len(publisher.cleaned))
	}
}

// gatedPublisher's completion waits for a gate that the test holds, as a
// capture holds the snapshot handoff gate.
type gatedPublisher struct {
	testPublisher
	waiting chan struct{}
	gate    chan struct{}
}

func (p *gatedPublisher) BeginCompletion(abandon func() bool) (func(), bool) {
	close(p.waiting)
	for {
		select {
		case <-p.gate:
			return func() {}, true
		case <-time.After(5 * time.Millisecond):
			if abandon() {
				return nil, false
			}
		}
	}
}

func TestCompletionWaitsForItsGateWithoutTheQueueLock(t *testing.T) {
	publisher := &gatedPublisher{
		testPublisher: testPublisher{completed: make(chan Intent, 1)},
		waiting:       make(chan struct{}), gate: make(chan struct{}),
	}
	queue, err := Open(t.TempDir(), publisher)
	if err != nil {
		t.Fatal(err)
	}
	defer queue.Close()
	if err := queue.Enqueue(Intent{Container: "family", Branch: "main", PublicationID: "publication-4", Kind: "stop", FilesystemBranch: "capture"}); err != nil {
		t.Fatal(err)
	}
	if err := queue.Activate("family", "main", "publication-4"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-publisher.waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("completion did not begin")
	}
	// A removal that holds what the gate's holder waits for cancels another
	// branch's publication meanwhile.
	if err := queue.Enqueue(Intent{Container: "family", Branch: "other", PublicationID: "publication-5", Kind: "stop", FilesystemBranch: "capture"}); err != nil {
		t.Fatal(err)
	}
	canceled := make(chan error, 1)
	go func() { canceled <- queue.Cancel("family", "other", "publication-5") }()
	select {
	case err := <-canceled:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Cancel waited for a completion that waits for its gate")
	}
	close(publisher.gate)
	select {
	case <-publisher.completed:
	case <-time.After(5 * time.Second):
		t.Fatal("publication did not complete once its gate opened")
	}
}

// A removal cancels a publication whose completion waits for its gate, and
// waits for that attempt holding what the gate's holder waits for: the
// attempt must give the gate up, rather than deadlock the three.
func TestCancelOfACompletionWaitingForItsGate(t *testing.T) {
	publisher := &gatedPublisher{
		testPublisher: testPublisher{completed: make(chan Intent, 1)},
		waiting:       make(chan struct{}), gate: make(chan struct{}),
	}
	queue, err := Open(t.TempDir(), publisher)
	if err != nil {
		t.Fatal(err)
	}
	defer queue.Close()
	if err := queue.Enqueue(Intent{Container: "family", Branch: "main", PublicationID: "publication-6", Kind: "stop", FilesystemBranch: "capture"}); err != nil {
		t.Fatal(err)
	}
	if err := queue.Activate("family", "main", "publication-6"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-publisher.waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("completion did not begin")
	}
	canceled := make(chan error, 1)
	go func() { canceled <- queue.Cancel("family", "main", "publication-6") }()
	select {
	case err := <-canceled:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Cancel and the completion it waits for deadlocked on the gate")
	}
	defer close(publisher.gate)
	if pending, err := queue.Pending(); err != nil || len(pending) != 0 {
		t.Fatalf("canceled intent remains: pending=%+v err=%v", pending, err)
	}
	select {
	case intent := <-publisher.completed:
		t.Fatalf("canceled publication %s completed", intent.PublicationID)
	default:
	}
}

// waitingPublisher's publication waits until its context ends, as one waits
// for a lock whose holder waits for what a cancellation's caller holds.
type waitingPublisher struct {
	testPublisher
	started chan struct{}
	release chan struct{}
	ended   chan error
	cleaned chan Intent
}

func (p *waitingPublisher) Publish(ctx context.Context, _ Intent) (Result, error) {
	close(p.started)
	select {
	case <-ctx.Done():
	case <-p.release:
	}
	p.ended <- ctx.Err()
	return Result{}, errors.New("publication interrupted")
}
func (p *waitingPublisher) Cleanup(intent Intent) error { p.cleaned <- intent; return nil }

// A removal cancels a publication and waits for its attempt under way, which
// waits for what the removal holds: the cancellation ends the attempt's
// context, rather than deadlock the two.
func TestCancelEndsTheAttemptUnderWay(t *testing.T) {
	publisher := &waitingPublisher{
		testPublisher: testPublisher{completed: make(chan Intent, 1)},
		started:       make(chan struct{}), release: make(chan struct{}),
		ended: make(chan error, 1), cleaned: make(chan Intent, 2),
	}
	queue, err := Open(t.TempDir(), publisher)
	if err != nil {
		t.Fatal(err)
	}
	defer queue.Close()
	defer close(publisher.release)
	if err := queue.Enqueue(Intent{Container: "family", Branch: "main", PublicationID: "publication-8", Kind: "dehydrate", FilesystemBranch: "capture"}); err != nil {
		t.Fatal(err)
	}
	if err := queue.Activate("family", "main", "publication-8"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-publisher.started:
	case <-time.After(5 * time.Second):
		t.Fatal("publication did not start")
	}
	canceled := make(chan error, 1)
	go func() { canceled <- queue.Cancel("family", "main", "publication-8") }()
	select {
	case err := <-canceled:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Cancel and the attempt it waits for deadlocked")
	}
	if err := <-publisher.ended; !errors.Is(err, context.Canceled) {
		t.Fatalf("the attempt's context ended with %v, want %v", err, context.Canceled)
	}
	if pending, err := queue.Pending(); err != nil || len(pending) != 0 {
		t.Fatalf("canceled intent remains: pending=%+v err=%v", pending, err)
	}
	if len(publisher.cleaned) != 1 {
		t.Fatalf("canceled capture cleaned %d times, want 1", len(publisher.cleaned))
	}
	select {
	case intent := <-publisher.completed:
		t.Fatalf("canceled publication %s completed", intent.PublicationID)
	default:
	}
}

// removedPublisher's captures will never be ready: their branch was removed.
type removedPublisher struct {
	supersededPublisher
}

func (p *removedPublisher) CaptureAvailable(Intent) (bool, error) {
	return false, fmt.Errorf("%w: the branch was removed", ErrPublicationSuperseded)
}

// A capture whose branch is gone is cleaned and dropped, not retried for good:
// every drain of the queue, as a node's stop, waited for it.
func TestACaptureThatWillNeverBeReadyIsDropped(t *testing.T) {
	publisher := &removedPublisher{supersededPublisher{
		testPublisher: testPublisher{completed: make(chan Intent, 1)},
		cleaned:       make(chan Intent, 1), failed: make(chan error, 1),
	}}
	queue, err := Open(t.TempDir(), publisher)
	if err != nil {
		t.Fatal(err)
	}
	defer queue.Close()
	if err := queue.Enqueue(Intent{Container: "family", Branch: "main", PublicationID: "publication-1", Kind: "stop", FilesystemBranch: "capture"}); err != nil {
		t.Fatal(err)
	}
	if err := queue.Activate("family", "main", "publication-1"); !errors.Is(err, ErrPublicationSuperseded) {
		t.Fatalf("activating a capture of a removed branch: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := queue.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-publisher.cleaned:
	default:
		t.Error("the capture was not cleaned")
	}
	select {
	case err := <-publisher.failed:
		t.Errorf("a capture that will never be ready reported a retryable failure: %v", err)
	default:
	}
}

// A drain retries a failed publication at once, not when its backoff, which
// grows to minutes, ends.
func TestDrainRetriesAtOnce(t *testing.T) {
	publisher := &testPublisher{failed: true, completed: make(chan Intent, 1)}
	queue, err := Open(t.TempDir(), publisher)
	if err != nil {
		t.Fatal(err)
	}
	defer queue.Close()
	if err := queue.Enqueue(Intent{Container: "family", Branch: "main", PublicationID: "publication-1", Kind: "stop", FilesystemBranch: "capture"}); err != nil {
		t.Fatal(err)
	}
	if err := queue.Activate("family", "main", "publication-1"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		pending, err := queue.Pending()
		if err == nil && len(pending) == 1 && pending[0].Attempts == 1 {
			// Its next attempt is ten minutes off.
			queue.mu.Lock()
			pending[0].RetryAt = time.Now().Add(10 * time.Minute)
			err = writeJSONAtomic(queue.path("pending", pending[0].ID), pending[0])
			queue.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the failure was not persisted: %+v, %v", pending, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := queue.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-publisher.completed:
	default:
		t.Error("the drained publication did not complete")
	}
}
