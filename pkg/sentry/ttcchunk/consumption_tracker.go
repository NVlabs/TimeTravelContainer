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

package ttcchunk

import (
	"fmt"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
)

const consumptionBatchObjects = 64

// ConsumptionTracker batches completed 1 MiB installs away from the page
// fault path. Its updates are monotonic; an update failure only over-retains
// immutable backing and is safe to retry on the next batch.
type ConsumptionTracker struct {
	mu           sync.Mutex
	repository   *MemoryRepository
	branch       string
	manifest     ManifestID
	objects      []ID
	pending      map[ID]struct{}
	active       bool
	decided      bool
	readerClosed bool
	wake         chan struct{}
	done         chan struct{}
	lease        *Lease
	closeErr     error
}

func newConsumptionTracker(repository *MemoryRepository, branch string, manifest ManifestID, objects []ID) (*ConsumptionTracker, error) {
	if repository == nil || branch == "" || manifest == (ManifestID{}) {
		return nil, fmt.Errorf("invalid sparse consumption tracker identity")
	}
	// The branch root becomes progressively weaker as loaded objects are marked
	// consumed. Keep the complete manifest rooted independently for as long as
	// this reader may still open an object. This is required even on local
	// filesystems for correctness, and prevents NFS from silly-renaming an open
	// chunk when a sibling branch is removed concurrently.
	lease, err := repository.AcquireLease(manifest, DefaultLeaseTTL)
	if err != nil {
		return nil, fmt.Errorf("leasing sparse reader manifest %s: %w", manifest, err)
	}
	t := &ConsumptionTracker{
		repository: repository, branch: branch, manifest: manifest,
		objects: append([]ID(nil), objects...), pending: make(map[ID]struct{}), wake: make(chan struct{}, 1),
		done: make(chan struct{}), lease: lease,
	}
	go t.run()
	return t, nil
}

func (t *ConsumptionTracker) signal() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

// Record buffers every complete object covered by a successful read.
func (t *ConsumptionTracker) Record(offset, length uint64) {
	if t == nil || length == 0 || offset%pgalloc.SparseChunkBytes != 0 || length%pgalloc.SparseChunkBytes != 0 {
		return
	}
	first := offset / pgalloc.SparseChunkBytes
	last := first + length/pgalloc.SparseChunkBytes
	t.mu.Lock()
	if last <= uint64(len(t.objects)) && (!t.decided || t.active) {
		for index := first; index < last; index++ {
			t.pending[t.objects[index]] = struct{}{}
		}
	}
	wake := t.active && len(t.pending) >= consumptionBatchObjects
	t.mu.Unlock()
	if wake {
		t.signal()
	}
}

// Activate permits durable updates after the restored graph commits.
func (t *ConsumptionTracker) Activate(alreadyResident []ID) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if !t.decided {
		t.decided = true
		t.active = true
		for _, object := range alreadyResident {
			t.pending[object] = struct{}{}
		}
	}
	t.mu.Unlock()
	t.signal()
}

// Abort discards buffered updates from a failed restore.
func (t *ConsumptionTracker) Abort() {
	if t == nil {
		return
	}
	t.mu.Lock()
	if !t.decided {
		t.decided = true
		t.pending = make(map[ID]struct{})
	}
	t.mu.Unlock()
	t.signal()
}

// ReaderClosed flushes consumption, releases the loader lease, and closes the
// donated repository before returning. Waiting here ensures branch GC cannot
// race a descriptor that the reader still owns.
func (t *ConsumptionTracker) ReaderClosed() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	if !t.decided {
		t.decided = true
		t.pending = make(map[ID]struct{})
	}
	t.readerClosed = true
	t.mu.Unlock()
	t.signal()
	<-t.done
	t.mu.Lock()
	err := t.closeErr
	t.mu.Unlock()
	return err
}

func (t *ConsumptionTracker) run() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	nextRenewal := time.Now().Add(DefaultLeaseTTL / 3)
	for {
		select {
		case <-ticker.C:
		case <-t.wake:
		}
		t.mu.Lock()
		active := t.active
		closed := t.readerClosed && t.decided
		batch := make([]ID, 0, len(t.pending))
		if active {
			for object := range t.pending {
				batch = append(batch, object)
			}
			t.pending = make(map[ID]struct{})
		}
		t.mu.Unlock()
		if len(batch) != 0 {
			if err := t.repository.MarkConsumed(t.branch, t.manifest, batch); err != nil {
				log.Warningf("recording TTC sparse memory consumption for %q failed; backing is retained: %v", t.branch, err)
			}
		}
		if !closed && !time.Now().Before(nextRenewal) {
			if err := t.lease.Renew(); err != nil {
				log.Warningf("renewing TTC sparse memory loader lease for %q failed: %v", t.branch, err)
			}
			nextRenewal = time.Now().Add(DefaultLeaseTTL / 3)
		}
		if closed {
			var closeErr error
			if err := t.lease.Release(); err != nil {
				closeErr = fmt.Errorf("releasing sparse memory loader lease: %w", err)
			}
			if err := t.repository.Close(); err != nil && closeErr == nil {
				closeErr = fmt.Errorf("closing sparse memory repository: %w", err)
			}
			t.mu.Lock()
			t.closeErr = closeErr
			close(t.done)
			t.mu.Unlock()
			return
		}
	}
}
