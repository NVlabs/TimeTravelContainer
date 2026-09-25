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

//go:build !pagesize_64k

package pgalloc

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

// TestSaveKeepsASharedMemoryFileLocked saves a MemoryFile with a second
// owner, as a branch's Kernel shares its family's with its siblings, which
// go on allocating and freeing while it is saved. No other goroutine may take
// the MemoryFile's lock while SaveTo scans it.
func TestSaveKeepsASharedMemoryFileLocked(t *testing.T) {
	mf, _ := newRemoteTestMemoryFile(t, 1)
	// Allocations of alternating kinds, each a segment of its own to scan.
	var frs []memmap.FileRange
	defer func() {
		for _, fr := range frs {
			mf.DecRef(fr)
		}
	}()
	for i := 0; i < 16384; i++ {
		kind := usage.Anonymous
		if i%2 == 1 {
			kind = usage.Tmpfs
		}
		fr, err := mf.Allocate(hostarch.PageSize, AllocOpts{Kind: kind, Mode: AllocateAndWritePopulate})
		if err != nil {
			t.Fatal(err)
		}
		frs = append(frs, fr)
	}
	mf.IncOwner()
	defer mf.Destroy()

	stop := make(chan struct{})
	var sawSaving atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			mf.mu.Lock()
			if mf.isSaving {
				sawSaving.Store(true)
			}
			mf.mu.Unlock()
		}
	}()
	err := mf.SaveTo(context.Background(), io.Discard, &SaveOpts{ExcludeCommittedZeroPages: true})
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if sawSaving.Load() {
		t.Error("another goroutine took the shared MemoryFile's lock while SaveTo scanned it")
	}
}
