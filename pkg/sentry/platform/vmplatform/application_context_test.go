// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package vmplatform

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestApplicationRunContextLazyCancellation(t *testing.T) {
	var run applicationRunContext
	run.reset(context.Background())
	if err := run.Err(); err != nil {
		t.Fatalf("fresh context error: %v", err)
	}
	done := run.Done()
	select {
	case <-done:
		t.Fatal("fresh context is already done")
	default:
	}
	run.cancel()
	if !errors.Is(run.Err(), context.Canceled) {
		t.Fatalf("canceled context error = %v", run.Err())
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancel did not close Done")
	}
}

func TestApplicationRunContextParentAndReset(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	var run applicationRunContext
	run.reset(parent)
	oldDone := run.Done()
	cancelParent()
	select {
	case <-oldDone:
	case <-time.After(time.Second):
		t.Fatal("parent cancellation did not close Done")
	}
	if !errors.Is(run.Err(), context.Canceled) {
		t.Fatalf("parent-canceled context error = %v", run.Err())
	}

	run.reset(context.Background())
	if err := run.Err(); err != nil {
		t.Fatalf("reset context retained cancellation: %v", err)
	}
	select {
	case <-oldDone:
	default:
		t.Fatal("reset left the previous generation open")
	}
	run.finish()
	if !errors.Is(run.Err(), context.Canceled) {
		t.Fatalf("finished context error = %v", run.Err())
	}
}

func TestApplicationRunContextCancelBeforeDone(t *testing.T) {
	var run applicationRunContext
	run.reset(context.Background())
	run.cancel()
	select {
	case <-run.Done():
	default:
		t.Fatal("Done returned an open channel after cancellation")
	}
}
