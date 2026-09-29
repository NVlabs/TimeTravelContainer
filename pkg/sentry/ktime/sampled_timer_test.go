// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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
