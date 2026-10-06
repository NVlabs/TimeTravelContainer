// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package storage coordinates crash-recoverable publication from node-local
// active state to the shared cold repository.
package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Intent is a durable, immutable-identity-fenced request to publish a branch.
type Intent struct {
	ID        string `json:"id"`
	Container string `json:"container"`
	Branch    string `json:"branch"`
	// AuthorityContainer and AuthorityBranch are the stable user-visible
	// identity in clustered mode. Container and Branch remain the immutable
	// physical capture identity used to find node-local data after migrations.
	AuthorityContainer string `json:"authority_container,omitempty"`
	AuthorityBranch    string `json:"authority_branch,omitempty"`
	PublicationID      string `json:"publication_id"`
	// SupersedesPublicationID is the previous control-plane publication, if
	// any. It is canceled only after this intent's complete local capture has
	// become the branch's current identity.
	SupersedesPublicationID string    `json:"supersedes_publication_id,omitempty"`
	Kind                    string    `json:"kind"`
	FilesystemBranch        string    `json:"filesystem_branch"`
	CaptureReady            bool      `json:"capture_ready"`
	ActivationRequested     bool      `json:"activation_requested,omitempty"`
	CreatedAt               time.Time `json:"created_at"`
	Attempts                int       `json:"attempts,omitempty"`
	LastError               string    `json:"last_error,omitempty"`
	RetryAt                 time.Time `json:"retry_at,omitempty"`
}

// ErrPublicationCommitted is what canceling a publication returns that
// committed first: its capture is the branch's durable state.
var ErrPublicationCommitted = errors.New("publication was committed")

// ErrPublicationSuperseded means an immutable work item is no longer the
// branch's current publication. It is terminal, not a transient storage
// failure: the queue cleans its private capture and removes the intent.
var ErrPublicationSuperseded = errors.New("publication was superseded")

// Result is the authoritative shared manifest produced by Publisher.
type Result struct {
	Manifest string `json:"manifest"`
}

// Publisher seals the TTC-specific repository and memory roots into shared
// immutable data. Publish must be idempotent for an Intent ID.
type Publisher interface {
	CaptureAvailable(Intent) (bool, error)
	Publish(context.Context, Intent) (Result, error)
	Complete(Intent, Result) error
	Failed(Intent, error, time.Time) error
	Cleanup(Intent) error
}

// errCompletionAbandoned is the error of an attempt that gave up completing
// for the publication's cancellation.
var errCompletionAbandoned = errors.New("publication completion gave way to its cancellation")

// completionGater is implemented by publishers whose Complete needs a lock
// that its holders may keep while they wait for this queue. The queue takes
// that lock before its own, never under it, and what BeginCompletion returns
// releases it. BeginCompletion gives up, returning false, once abandon
// reports true: a cancellation waits for the attempt holding a lock that the
// gate's holder may be waiting for.
type completionGater interface {
	BeginCompletion(abandon func() bool) (func(), bool)
}

// cancellationFencer is implemented by distributed publishers whose control
// plane can reject an in-flight Complete before physical cleanup is safe. The
// durable local tombstone is installed first; the authority then gives commit versus
// cancel one total order. FenceCancellation returns ErrPublicationCommitted if
// the commit came first.
type cancellationFencer interface {
	FenceCancellation(Intent) error
}

// Queue persists intents before returning to lifecycle commands and retries
// them after daemon restart.
type Queue struct {
	root      string
	publisher Publisher
	wake      chan struct{}
	done      chan struct{}
	stopped   chan struct{}
	once      sync.Once
	mu        sync.Mutex
	inFlight  map[string]struct{}
	// attempts ends the context of each publication attempt under way, once
	// its publication is canceled: the cancellation waits for the attempt,
	// which may wait for locks whose holders wait for the cancellation's
	// caller.
	attempts map[string]context.CancelFunc
}

// Open opens a node-local queue and starts its worker.
func Open(localRoot string, publisher Publisher) (*Queue, error) {
	if localRoot == "" || publisher == nil {
		return nil, errors.New("publication queue requires a local root and publisher")
	}
	root := filepath.Join(localRoot, "publication-queue")
	for _, name := range []string{"pending", "canceled", "complete", "quarantine"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0700); err != nil {
			return nil, err
		}
	}
	q := &Queue{
		root: root, publisher: publisher, wake: make(chan struct{}, 1),
		done: make(chan struct{}), stopped: make(chan struct{}),
		inFlight: make(map[string]struct{}), attempts: make(map[string]context.CancelFunc),
	}
	if err := q.recoverCaptures(); err != nil {
		return nil, err
	}
	if err := q.pruneTerminalMarkers(); err != nil {
		return nil, err
	}
	go q.run()
	q.signal()
	return q, nil
}

// Activate commits the local capture fence. Workers never inspect the
// snapshot roots before this durable transition.
func (q *Queue) Activate(container, branch, publicationID string) error {
	id := intentID(container, branch, publicationID)
	intent, err := q.read(id + ".json")
	if err != nil {
		return err
	}
	// Persist the request before consulting external authority. A transient
	// authority or filesystem error must be retried by this daemon, not postponed
	// until its next restart.
	intent.ActivationRequested = true
	if err := writeJSONAtomic(q.path("pending", id), intent); err != nil {
		return err
	}
	ready, err := q.publisher.CaptureAvailable(intent)
	if err != nil {
		q.signal()
		return err
	}
	if !ready {
		q.signal()
		return errors.New("publication capture fence is incomplete")
	}
	intent.CaptureReady = true
	if err := writeJSONAtomic(q.path("pending", id), intent); err != nil {
		return err
	}
	q.signal()
	return nil
}

// Enqueue durably records an intent. The lifecycle operation may return once
// this succeeds because daemon restart will resume publication.
func (q *Queue) Enqueue(intent Intent) error {
	if intent.Container == "" || intent.Branch == "" || intent.PublicationID == "" || intent.Kind == "" || intent.FilesystemBranch == "" {
		return errors.New("invalid publication intent")
	}
	if (intent.AuthorityContainer == "") != (intent.AuthorityBranch == "") {
		return errors.New("incomplete publication authority identity")
	}
	intent.ID = intentID(intent.Container, intent.Branch, intent.PublicationID)
	if intent.CreatedAt.IsZero() {
		intent.CreatedAt = time.Now().UTC()
	}
	if err := writeJSONAtomic(q.path("pending", intent.ID), intent); err != nil {
		return fmt.Errorf("persisting publication intent: %w", err)
	}
	q.signal()
	return nil
}

// Cancel fences one exact publication before removing its pending intent. A
// publication that committed first stays committed.
func (q *Queue) Cancel(container, branch, publicationID string) error {
	if err := q.CancelUncommitted(container, branch, publicationID); !errors.Is(err, ErrPublicationCommitted) {
		return err
	}
	return nil
}

// CancelUncommitted is Cancel, for a caller that must not take a publication
// that committed first for canceled, as one that goes on from the capture
// does: it returns ErrPublicationCommitted then, once the attempt that
// committed it has ended.
func (q *Queue) CancelUncommitted(container, branch, publicationID string) error {
	committed, err := q.cancel(container, branch, publicationID)
	if committed {
		return errors.Join(err, ErrPublicationCommitted)
	}
	return err
}

func (q *Queue) cancel(container, branch, publicationID string) (committed bool, _ error) {
	id := intentID(container, branch, publicationID)
	intent, readErr := q.read(id + ".json")
	if errors.Is(readErr, os.ErrNotExist) {
		readErr = nil
	}
	if err := writeJSONAtomic(q.path("canceled", id), map[string]any{"id": id, "canceled_at": time.Now().UTC()}); err != nil {
		return false, err
	}
	if readErr == nil && intent.ID != "" {
		if fencer, ok := q.publisher.(cancellationFencer); ok {
			if err := fencer.FenceCancellation(intent); errors.Is(err, ErrPublicationCommitted) {
				committed = true
			} else if err != nil {
				q.signal()
				return false, err
			}
		}
	}
	q.mu.Lock()
	_, inFlight := q.inFlight[id]
	if endAttempt := q.attempts[id]; endAttempt != nil {
		endAttempt()
	}
	q.mu.Unlock()
	// The worker owns cleanup once publication has started. It observes the
	// durable cancellation fence before completing or retrying the intent.
	if !inFlight && readErr == nil && intent.ID != "" {
		if cleanupErr := q.publisher.Cleanup(intent); cleanupErr != nil {
			readErr = cleanupErr
			intent.Attempts++
			intent.LastError = cleanupErr.Error()
			intent.RetryAt = time.Now().Add(time.Second << min(intent.Attempts-1, 8)).UTC()
			if err := writeJSONAtomic(q.path("pending", id), intent); err != nil {
				readErr = errors.Join(readErr, err)
			}
		} else if err := q.finishCancellation(id); err != nil {
			readErr = err
		}
	}
	if !inFlight && (readErr == nil || errors.Is(readErr, os.ErrNotExist)) && intent.ID == "" {
		if err := os.Remove(q.path("canceled", id)); err != nil && !errors.Is(err, os.ErrNotExist) {
			readErr = errors.Join(readErr, err)
		}
	}
	q.signal()
	if inFlight {
		// Wait for the attempt the cancellation raced, and hold the intent
		// against the worker once it ends. An attempt that completed or saw
		// the fence has removed the intent; one that failed, as a capture that
		// the fence made unusable fails, left it for a retry, and this
		// cancellation finishes it.
		for {
			q.mu.Lock()
			if _, active := q.inFlight[id]; !active {
				q.inFlight[id] = struct{}{}
				q.mu.Unlock()
				break
			}
			q.mu.Unlock()
			time.Sleep(10 * time.Millisecond)
		}
		defer func() {
			q.mu.Lock()
			delete(q.inFlight, id)
			q.mu.Unlock()
		}()
		pending, err := q.read(id + ".json")
		if errors.Is(err, os.ErrNotExist) {
			return committed, readErr
		}
		if err != nil {
			return committed, errors.Join(readErr, err)
		}
		if err := q.publisher.Cleanup(pending); err != nil {
			pending.Attempts++
			pending.LastError = err.Error()
			pending.RetryAt = time.Now().Add(time.Second << min(pending.Attempts-1, 8)).UTC()
			return committed, errors.Join(readErr, err, writeJSONAtomic(q.path("pending", id), pending))
		}
		return committed, errors.Join(readErr, q.finishCancellation(id))
	}
	return committed, readErr
}

// CancelPending cancels a publication only when it still has a durable pending
// intent. Successfully completed publications need no tombstone and may have
// already discarded their local capture.
func (q *Queue) CancelPending(container, branch, publicationID string) error {
	id := intentID(container, branch, publicationID)
	if _, err := q.read(id + ".json"); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return q.Cancel(container, branch, publicationID)
}

// CancelBranch fences and removes all pending publications for one branch.
// This is intentionally broader than the identity recorded in current branch
// metadata: a previously failed publication must not keep daemon drain
// alive after a later publication supersedes it or the branch is removed.
// It returns how many there were: with none, no publication of the branch
// was under way to change its record.
func (q *Queue) CancelBranch(container, branch string) (int, error) {
	intents, err := q.Pending()
	if err != nil {
		return 0, err
	}
	canceled := 0
	var result error
	for _, intent := range intents {
		if intent.Container == container && intent.Branch == branch {
			canceled++
			result = errors.Join(result, q.Cancel(intent.Container, intent.Branch, intent.PublicationID))
		}
	}
	return canceled, result
}

// Close stops after the current publication boundary. Pending intents remain
// durable for the next daemon.
func (q *Queue) Close() {
	q.once.Do(func() { close(q.done); <-q.stopped })
}

// Drain waits until every durable intent has either committed or been
// canceled. Retryable errors remain visible and continue to be retried: each
// at once first, rather than when its backoff, which grows to minutes, ends.
func (q *Queue) Drain(ctx context.Context) error {
	q.retryNow()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		pending, err := q.Pending()
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			return nil
		}
		q.signal()
		select {
		case <-ctx.Done():
			var details []string
			for _, intent := range pending {
				if intent.LastError == "" {
					continue
				}
				details = append(details, fmt.Sprintf("%s@%s (%s): %s", intent.Container, intent.Branch, intent.ID, intent.LastError))
				if len(details) == 3 {
					break
				}
			}
			if len(details) == 0 {
				return fmt.Errorf("%d publications remain pending: %w", len(pending), ctx.Err())
			}
			return fmt.Errorf("%d publications remain pending: %s: %w", len(pending), strings.Join(details, "; "), ctx.Err())
		case <-ticker.C:
		}
	}
}

// retryNow makes every pending intent that waits to be retried due now.
func (q *Queue) retryNow() {
	intents, err := q.Pending()
	if err != nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, intent := range intents {
		if _, active := q.inFlight[intent.ID]; active || intent.RetryAt.IsZero() {
			continue
		}
		// The worker writes an intent under the lock: read it again under it.
		current, err := q.read(intent.ID + ".json")
		if err != nil || current.RetryAt.IsZero() {
			continue
		}
		current.RetryAt = time.Time{}
		_ = writeJSONAtomic(q.path("pending", current.ID), current)
	}
}

// Pending returns stable intent snapshots for drain/status reporting.
func (q *Queue) Pending() ([]Intent, error) {
	entries, err := os.ReadDir(filepath.Join(q.root, "pending"))
	if err != nil {
		return nil, err
	}
	intents := make([]Intent, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		intent, err := q.read(entry.Name())
		if err != nil {
			q.quarantine(entry.Name(), err)
			continue
		}
		intents = append(intents, intent)
	}
	sort.Slice(intents, func(i, j int) bool { return intents[i].CreatedAt.Before(intents[j].CreatedAt) })
	return intents, nil
}

func (q *Queue) run() {
	defer close(q.stopped)
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		next := q.process()
		if next <= 0 {
			next = time.Hour
		}
		timer.Reset(next)
		select {
		case <-q.done:
			return
		case <-q.wake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
		}
	}
}

func (q *Queue) process() time.Duration {
	intents, err := q.Pending()
	if err != nil {
		return time.Second
	}
	now := time.Now()
	next := time.Hour
	for _, intent := range intents {
		q.mu.Lock()
		if intent.RetryAt.After(now) {
			q.mu.Unlock()
			if delay := time.Until(intent.RetryAt); delay < next {
				next = delay
			}
			continue
		}
		if q.canceled(intent.ID) {
			q.inFlight[intent.ID] = struct{}{}
			q.mu.Unlock()
			cleanupErr := q.publisher.Cleanup(intent)
			q.mu.Lock()
			if cleanupErr == nil {
				_ = q.finishCancellation(intent.ID)
			} else {
				intent.Attempts++
				intent.LastError = cleanupErr.Error()
				delay := time.Second << min(intent.Attempts-1, 8)
				intent.RetryAt = time.Now().Add(delay).UTC()
				_ = writeJSONAtomic(q.path("pending", intent.ID), intent)
				if delay < next {
					next = delay
				}
			}
			delete(q.inFlight, intent.ID)
			q.mu.Unlock()
			continue
		}
		if !intent.CaptureReady {
			if !intent.ActivationRequested {
				q.mu.Unlock()
				continue
			}
			q.inFlight[intent.ID] = struct{}{}
			q.mu.Unlock()
			ready, readyErr := q.publisher.CaptureAvailable(intent)
			if errors.Is(readyErr, ErrPublicationSuperseded) {
				// It will never be ready: as a superseded publication below.
				if readyErr = q.publisher.Cleanup(intent); readyErr == nil {
					q.mu.Lock()
					_ = os.Remove(q.path("pending", intent.ID))
					delete(q.inFlight, intent.ID)
					q.mu.Unlock()
					continue
				}
			}
			q.mu.Lock()
			if readyErr == nil && ready {
				intent.CaptureReady = true
				intent.LastError = ""
				intent.RetryAt = time.Time{}
				_ = writeJSONAtomic(q.path("pending", intent.ID), intent)
				q.signal()
			} else if readyErr != nil {
				intent.Attempts++
				intent.LastError = readyErr.Error()
				delay := time.Second << min(intent.Attempts-1, 8)
				intent.RetryAt = time.Now().Add(delay).UTC()
				_ = q.publisher.Failed(intent, readyErr, intent.RetryAt)
				_ = writeJSONAtomic(q.path("pending", intent.ID), intent)
				if delay < next {
					next = delay
				}
			}
			delete(q.inFlight, intent.ID)
			q.mu.Unlock()
			continue
		}
		q.inFlight[intent.ID] = struct{}{}
		ctx, endAttempt := context.WithCancel(context.Background())
		q.attempts[intent.ID] = endAttempt
		q.mu.Unlock()

		result, publishErr := q.publisher.Publish(ctx, intent)
		q.mu.Lock()
		delete(q.attempts, intent.ID)
		q.mu.Unlock()
		endAttempt()
		endCompletion := func() {}
		if gater, ok := q.publisher.(completionGater); ok && publishErr == nil {
			// A removal canceling this publication waits for this attempt
			// holding the lifecycle lock, which a capture holding the gate
			// may wait for: once canceled, stop waiting for the gate.
			end, gated := gater.BeginCompletion(func() bool { return q.canceled(intent.ID) })
			if gated {
				endCompletion = end
			} else {
				publishErr = errCompletionAbandoned
			}
		}
		q.mu.Lock()
		canceled := q.canceled(intent.ID)
		if publishErr == nil && !canceled {
			publishErr = q.publisher.Complete(intent, result)
			canceled = q.canceled(intent.ID)
		}
		q.mu.Unlock()
		endCompletion()

		if canceled {
			cleanupErr := q.publisher.Cleanup(intent)
			q.mu.Lock()
			if cleanupErr == nil {
				_ = q.finishCancellation(intent.ID)
			} else {
				intent.Attempts++
				intent.LastError = cleanupErr.Error()
				delay := time.Second << min(intent.Attempts-1, 8)
				intent.RetryAt = time.Now().Add(delay).UTC()
				_ = writeJSONAtomic(q.path("pending", intent.ID), intent)
				if delay < next {
					next = delay
				}
			}
			delete(q.inFlight, intent.ID)
			q.mu.Unlock()
			continue
		}
		if errors.Is(publishErr, ErrPublicationSuperseded) {
			publishErr = q.publisher.Cleanup(intent)
			if publishErr == nil {
				q.mu.Lock()
				_ = os.Remove(q.path("pending", intent.ID))
				delete(q.inFlight, intent.ID)
				q.mu.Unlock()
				continue
			}
		}
		if publishErr == nil {
			publishErr = q.publisher.Cleanup(intent)
		}

		q.mu.Lock()
		canceled = q.canceled(intent.ID)
		if canceled {
			_ = q.finishCancellation(intent.ID)
			delete(q.inFlight, intent.ID)
			q.mu.Unlock()
			continue
		}
		if publishErr == nil {
			_ = os.Remove(q.path("pending", intent.ID))
			delete(q.inFlight, intent.ID)
			q.mu.Unlock()
			continue
		}
		intent.Attempts++
		intent.LastError = publishErr.Error()
		delay := time.Second << min(intent.Attempts-1, 8)
		intent.RetryAt = time.Now().Add(delay).UTC()
		_ = q.publisher.Failed(intent, publishErr, intent.RetryAt)
		_ = writeJSONAtomic(q.path("pending", intent.ID), intent)
		delete(q.inFlight, intent.ID)
		q.mu.Unlock()
		if delay < next {
			next = delay
		}
	}
	return next
}

// finishCancellation deletes the work item before its tombstone. This order is
// the cancellation fence: after a crash, a surviving pending intent still sees
// the tombstone, while a surviving tombstone with no pending intent is inert
// and can be pruned at startup.
func (q *Queue) finishCancellation(id string) error {
	if err := os.Remove(q.path("pending", id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(q.path("canceled", id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (q *Queue) pruneTerminalMarkers() error {
	for _, kind := range []string{"canceled", "complete"} {
		entries, err := os.ReadDir(filepath.Join(q.root, kind))
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			if kind == "canceled" {
				if _, err := os.Stat(filepath.Join(q.root, "pending", entry.Name())); err == nil {
					continue
				} else if !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
			if err := os.Remove(filepath.Join(q.root, kind, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

func (q *Queue) recoverCaptures() error {
	intents, err := q.Pending()
	if err != nil {
		return err
	}
	for _, intent := range intents {
		if intent.CaptureReady || q.canceled(intent.ID) {
			continue
		}
		ready, readyErr := q.publisher.CaptureAvailable(intent)
		if readyErr == nil && ready {
			intent.CaptureReady = true
			if err := writeJSONAtomic(q.path("pending", intent.ID), intent); err != nil {
				return err
			}
			continue
		}
		cause := errors.New("daemon stopped before the local publication capture fence completed")
		if readyErr != nil {
			cause = fmt.Errorf("recovering publication capture: %w", readyErr)
		}
		_ = q.publisher.Failed(intent, cause, time.Time{})
		if err := q.Cancel(intent.Container, intent.Branch, intent.PublicationID); err != nil {
			return err
		}
	}
	return nil
}

func (q *Queue) read(name string) (Intent, error) {
	data, err := os.ReadFile(filepath.Join(q.root, "pending", name))
	if err != nil {
		return Intent{}, err
	}
	var intent Intent
	if err := json.Unmarshal(data, &intent); err != nil {
		return Intent{}, err
	}
	if intent.ID == "" || name != intent.ID+".json" {
		return Intent{}, errors.New("publication intent identity mismatch")
	}
	return intent, nil
}

func (q *Queue) quarantine(name string, cause error) {
	from := filepath.Join(q.root, "pending", name)
	to := filepath.Join(q.root, "quarantine", name+"-"+fmt.Sprint(time.Now().UnixNano()))
	_ = os.Rename(from, to)
	_ = os.WriteFile(to+".error", []byte(cause.Error()+"\n"), 0600)
}

func (q *Queue) canceled(id string) bool {
	_, err := os.Stat(q.path("canceled", id))
	return err == nil
}

func (q *Queue) path(kind, id string) string { return filepath.Join(q.root, kind, id+".json") }

func (q *Queue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func intentID(container, branch, publicationID string) string {
	raw := hex.EncodeToString([]byte(container)) + "-" + hex.EncodeToString([]byte(branch)) + "-" + hex.EncodeToString([]byte(publicationID))
	// Keep existing compact identities readable and stable, but bound the
	// filename when a migration transaction name is the physical branch. The
	// JSON intent retains all three unhashed identity fields; this digest is only
	// the node-local queue key and is not used as protocol authority.
	if len(raw)+len(".json") <= 240 {
		return raw
	}
	digest := sha256.Sum256([]byte(raw))
	return "sha256-" + hex.EncodeToString(digest[:])
}

func writeJSONAtomic(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".intent-*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
