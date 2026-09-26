// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package vfs

import (
	"bytes"
	"context"
	"testing"

	"gvisor.dev/gvisor/pkg/state"
)

// TestDentryRestoreDropsTransientLock verifies the live-fork invariant that a
// synchronization lock held by the source is never inherited by the child.
// There is no corresponding owner goroutine in the restored kernel, so copying
// this lock would permanently hang ordinary close and unlink operations.
func TestDentryRestoreDropsTransientLock(t *testing.T) {
	source := &Dentry{dead: true, evictable: true}
	source.mu.Lock()
	var image bytes.Buffer
	if _, err := state.Save(context.Background(), &image, source); err != nil {
		source.mu.Unlock()
		t.Fatalf("state.Save: %v", err)
	}
	source.mu.Unlock()

	var restored Dentry
	// State loading may populate an object supplied by a restore resource. Make
	// the invariant explicit even for that case rather than relying on fresh,
	// zeroed allocation by the decoder.
	restored.mu.Lock()
	if _, err := state.Load(context.Background(), bytes.NewReader(image.Bytes()), &restored); err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	if !restored.mu.TryLock() {
		t.Fatal("restored dentry retained the source's transient mutex state")
	}
	restored.mu.Unlock()
	if !restored.dead || !restored.evictable {
		t.Fatalf("restored state=(dead=%t, evictable=%t), want (true, true)", restored.dead, restored.evictable)
	}
}
