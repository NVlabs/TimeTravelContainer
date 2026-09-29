// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package vmplatform

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

var closedApplicationRunDone = func() <-chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}()

// applicationRunContext is a reusable cancellation boundary for application
// vCPU entries. context.WithCancel allocates a cancelCtx and a done channel for
// every Linux syscall even though the normal path never observes Done. This
// context allocates its done channel and parent hook only if an engine actually
// waits for cancellation (for example, while all vCPUs are resident elsewhere).
//
// reset and finish are called by the platform Context owner. cancel may run
// concurrently from Context.Interrupt or a parent cancellation hook.
type applicationRunContext struct {
	parent   context.Context
	canceled atomic.Bool

	mu         sync.Mutex
	done       chan struct{}
	generation uint64
	stopParent func() bool
}

func (c *applicationRunContext) reset(parent context.Context) {
	if parent == nil {
		parent = context.Background()
	}
	c.mu.Lock()
	stopParent := c.stopParent
	c.stopParent = nil
	wasCanceled := c.canceled.Swap(true)
	if c.done != nil && !wasCanceled {
		close(c.done)
	}
	c.done = nil
	c.generation++
	c.parent = parent
	c.canceled.Store(false)
	c.mu.Unlock()
	if stopParent != nil {
		stopParent()
	}
}

func (c *applicationRunContext) finish() {
	c.mu.Lock()
	stopParent := c.stopParent
	c.stopParent = nil
	wasCanceled := c.canceled.Swap(true)
	if c.done != nil && !wasCanceled {
		close(c.done)
	}
	c.mu.Unlock()
	if stopParent != nil {
		stopParent()
	}
}

func (c *applicationRunContext) cancel() {
	if c.canceled.Swap(true) {
		return
	}
	c.mu.Lock()
	if c.done != nil {
		close(c.done)
	}
	c.mu.Unlock()
}

func (c *applicationRunContext) cancelGeneration(generation uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation != generation || c.canceled.Swap(true) {
		return
	}
	if c.done != nil {
		close(c.done)
	}
}

func (c *applicationRunContext) Deadline() (time.Time, bool) {
	return c.parent.Deadline()
}

func (c *applicationRunContext) Done() <-chan struct{} {
	if c.canceled.Load() {
		return closedApplicationRunDone
	}
	c.mu.Lock()
	if c.canceled.Load() {
		c.mu.Unlock()
		return closedApplicationRunDone
	}
	if c.done != nil {
		done := c.done
		c.mu.Unlock()
		return done
	}
	done := make(chan struct{})
	c.done = done
	generation := c.generation
	parent := c.parent
	c.mu.Unlock()

	if parent.Done() == nil {
		return done
	}
	stopParent := context.AfterFunc(parent, func() {
		c.cancelGeneration(generation)
	})
	c.mu.Lock()
	if c.generation == generation && !c.canceled.Load() {
		c.stopParent = stopParent
		stopParent = nil
	}
	c.mu.Unlock()
	if stopParent != nil {
		stopParent()
	}
	return done
}

func (c *applicationRunContext) Err() error {
	if err := c.parent.Err(); err != nil {
		return err
	}
	if c.canceled.Load() {
		return context.Canceled
	}
	return nil
}

func (c *applicationRunContext) Value(key any) any {
	return c.parent.Value(key)
}
