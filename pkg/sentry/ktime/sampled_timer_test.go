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

package ktime

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"gvisor.dev/gvisor/pkg/state"
)

func TestDestroyRestoredTimerBeforeResume(t *testing.T) {
	timer := &SampledTimer{pauseState: timerPaused}
	timer.Destroy()
	if got, want := timer.pauseState, timerDestroyed; got != want {
		t.Fatalf("pauseState after Destroy: got %v, want %v", got, want)
	}
}

func TestSampledTimerStateSaveConcurrentMutation(t *testing.T) {
	timer := &SampledTimer{}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := int64(0); ; i++ {
			select {
			case <-done:
				return
			default:
			}
			timer.mu.Lock()
			timer.setting = Setting{Enabled: i&1 != 0, Next: FromNanoseconds(i), Period: 2}
			timer.pauseState = timerPauseState(i % int64(timerDestroyed+1))
			timer.mu.Unlock()
		}
	}()
	for i := 0; i < 100; i++ {
		var buf bytes.Buffer
		if _, err := state.Save(context.Background(), &buf, timer); err != nil {
			close(done)
			wg.Wait()
			t.Fatalf("state.Save: %v", err)
		}
	}
	close(done)
	wg.Wait()
}
