// Copyright 2026 The gVisor Authors.
// Licensed under the Apache License, Version 2.0 (the "License");

package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

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
	intent := Intent{Container: "family", Branch: "main", Generation: 7, Kind: "dehydrate", FilesystemBranch: "capture"}
	if err := queue.Enqueue(intent); err != nil {
		t.Fatal(err)
	}
	if err := queue.Activate("family", "main", 7); err != nil {
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
		if completed.Generation != 7 {
			t.Fatalf("completed %+v", completed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("recovered queue did not retry")
	}
}

func TestCancelWithoutPendingGenerationIsNoop(t *testing.T) {
	publisher := &testPublisher{completed: make(chan Intent, 1)}
	root := t.TempDir()
	queue, err := Open(root, publisher)
	if err != nil {
		t.Fatal(err)
	}
	defer queue.Close()
	if err := queue.Cancel("family", "reused", 3); err != nil {
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

func TestCancelBranchRemovesSupersededGenerations(t *testing.T) {
	publisher := &testPublisher{failed: true, completed: make(chan Intent, 2)}
	queue, err := Open(t.TempDir(), publisher)
	if err != nil {
		t.Fatal(err)
	}
	defer queue.Close()
	for _, generation := range []uint64{1, 2} {
		intent := Intent{Container: "family", Branch: "main", Generation: generation, Kind: "stop", FilesystemBranch: "capture"}
		if err := queue.Enqueue(intent); err != nil {
			t.Fatal(err)
		}
	}
	if err := queue.Enqueue(Intent{Container: "family", Branch: "peer", Generation: 3, Kind: "stop", FilesystemBranch: "capture"}); err != nil {
		t.Fatal(err)
	}
	if err := queue.CancelBranch("family", "main"); err != nil {
		t.Fatal(err)
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
	intent := Intent{Container: "family", Branch: "main", Generation: 1, Kind: "stop", FilesystemBranch: "capture"}
	if err := queue.Enqueue(intent); err != nil {
		t.Fatal(err)
	}
	if err := queue.Activate("family", "main", 1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-publisher.started:
	case <-time.After(5 * time.Second):
		t.Fatal("publication did not start")
	}
	canceled := make(chan error, 1)
	go func() { canceled <- queue.Cancel("family", "main", 1) }()
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
	intent := Intent{Container: "family", Branch: "main", Generation: 9, Kind: "stop", FilesystemBranch: "capture"}
	if err := queue.Enqueue(intent); err != nil {
		t.Fatal(err)
	}
	if err := queue.Cancel("family", "main", 9); err == nil {
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

func TestOpenRejectsIncompleteRecoveredCapture(t *testing.T) {
	root := t.TempDir()
	publisher := &testPublisher{completed: make(chan Intent, 1)}
	queue, err := Open(root, publisher)
	if err != nil {
		t.Fatal(err)
	}
	intent := Intent{Container: "family", Branch: "main", Generation: 9, Kind: "stop", FilesystemBranch: "capture"}
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
		if cleaned.Generation != 9 {
			t.Fatalf("cleaned %+v", cleaned)
		}
	default:
		t.Fatal("incomplete capture was not cleaned")
	}
}
