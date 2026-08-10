// Copyright 2018 The gVisor Authors.
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

package waiter

import (
	"bytes"
	"context"
	"testing"

	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/state"
)

type queueStateRoot struct {
	owner *queueStateOwner
}

func (*queueStateRoot) StateTypeName() string { return "pkg/waiter.queueStateRoot" }
func (*queueStateRoot) StateFields() []string { return []string{"owner"} }
func (r *queueStateRoot) StateSave(s state.Sink) {
	s.Save(0, &r.owner)
}
func (r *queueStateRoot) StateLoad(_ context.Context, s state.Source) {
	s.Load(0, &r.owner)
}

type queueStateOwner struct {
	queue    Queue
	notified EventMask
}

func (*queueStateOwner) StateTypeName() string { return "pkg/waiter.queueStateOwner" }
func (*queueStateOwner) StateFields() []string { return []string{"queue"} }
func (o *queueStateOwner) StateSave(s state.Sink) {
	s.Save(0, &o.queue)
}
func (o *queueStateOwner) StateLoad(_ context.Context, s state.Source) {
	s.Load(0, &o.queue)
}
func (o *queueStateOwner) NotifyEvent(mask EventMask) {
	o.notified |= mask
}

func init() {
	state.Register((*queueStateRoot)(nil))
	state.Register((*queueStateOwner)(nil))
}

func TestQueueStateRestoresListenerCycle(t *testing.T) {
	owner := &queueStateOwner{}
	entry := &Entry{}
	entry.Init(owner, ReadableEvents)
	owner.queue.EventRegister(entry)
	source := queueStateRoot{owner: owner}

	var image bytes.Buffer
	if _, err := state.Save(context.Background(), &image, &source); err != nil {
		t.Fatalf("saving Queue with cyclic listener: %v", err)
	}
	var restored queueStateRoot
	if _, err := state.Load(context.Background(), &image, &restored); err != nil {
		t.Fatalf("loading Queue with cyclic listener: %v", err)
	}
	if restored.owner == nil {
		t.Fatal("restored listener owner is nil")
	}
	if got, want := restored.owner.queue.Events(), ReadableEvents; got != want {
		t.Fatalf("restored events=%#x, want %#x", got, want)
	}
	restored.owner.queue.Notify(ReadableEvents)
	if got, want := restored.owner.notified, ReadableEvents; got != want {
		t.Fatalf("listener notification=%#x, want %#x", got, want)
	}
}

func TestQueueStateOmitsTransientChannelWaiter(t *testing.T) {
	var source Queue
	entry, _ := NewTransientChannelEntry(ReadableEvents | EventErr | EventHUp)
	source.EventRegister(&entry)

	var image bytes.Buffer
	if _, err := state.Save(context.Background(), &image, &source); err != nil {
		t.Fatalf("saving Queue with channel waiter: %v", err)
	}
	if got, want := source.Events(), ReadableEvents|EventErr|EventHUp; got != want {
		t.Fatalf("source events after save=%#x, want %#x", got, want)
	}
	var restored Queue
	if _, err := state.Load(context.Background(), &image, &restored); err != nil {
		t.Fatalf("loading Queue with channel waiter: %v", err)
	}
	if !restored.IsEmpty() {
		t.Fatalf("restored Queue retained transient events %#x", restored.Events())
	}
}

func TestQueueStateRejectsUnmarkedChannelWaiter(t *testing.T) {
	var source Queue
	entry, _ := NewChannelEntry(ReadableEvents)
	source.EventRegister(&entry)

	var image bytes.Buffer
	if _, err := state.Save(context.Background(), &image, &source); err == nil {
		t.Fatal("saving Queue with unmarked channel waiter unexpectedly succeeded")
	}
	if got, want := source.Events(), ReadableEvents; got != want {
		t.Fatalf("source events after rejected save=%#x, want %#x", got, want)
	}
}

func TestEmptyQueue(t *testing.T) {
	var q Queue

	// Notify the zero-value of a queue.
	q.Notify(EventIn)

	// Register then unregister a waiter, then notify the queue.
	cnt := 0
	e := NewFunctionEntry(EventIn, func(EventMask) { cnt++ })
	q.EventRegister(&e)
	q.EventUnregister(&e)
	q.Notify(EventIn)
	if cnt != 0 {
		t.Errorf("Callback was called when it shouldn't have been")
	}
}

func TestMask(t *testing.T) {
	// Register a waiter.
	var q Queue
	var cnt int
	e := NewFunctionEntry(EventIn|EventErr, func(EventMask) { cnt++ })
	q.EventRegister(&e)

	// Notify with an overlapping mask.
	cnt = 0
	q.Notify(EventIn | EventOut)
	if cnt != 1 {
		t.Errorf("Callback wasn't called when it should have been")
	}

	// Notify with a subset mask.
	cnt = 0
	q.Notify(EventIn)
	if cnt != 1 {
		t.Errorf("Callback wasn't called when it should have been")
	}

	// Notify with a superset mask.
	cnt = 0
	q.Notify(EventIn | EventErr | EventOut)
	if cnt != 1 {
		t.Errorf("Callback wasn't called when it should have been")
	}

	// Notify with the exact same mask.
	cnt = 0
	q.Notify(EventIn | EventErr)
	if cnt != 1 {
		t.Errorf("Callback wasn't called when it should have been")
	}

	// Notify with a disjoint mask.
	cnt = 0
	q.Notify(EventOut | EventHUp)
	if cnt != 0 {
		t.Errorf("Callback was called when it shouldn't have been")
	}
}

func TestConcurrentRegistration(t *testing.T) {
	var q Queue
	var cnt int
	const concurrency = 1000

	ch1 := make(chan struct{})
	ch2 := make(chan struct{})
	ch3 := make(chan struct{})

	// Create goroutines that will all register/unregister concurrently.
	for i := 0; i < concurrency; i++ {
		go func() {
			e := NewFunctionEntry(EventIn|EventErr, func(mask EventMask) {
				cnt++
				if mask != EventIn {
					t.Errorf("mask = %#x want %#x", mask, EventIn)
				}
			})

			// Wait for notification, then register.
			<-ch1
			q.EventRegister(&e)

			// Tell main goroutine that we're done registering.
			ch2 <- struct{}{}

			// Wait for notification, then unregister.
			<-ch3
			q.EventUnregister(&e)

			// Tell main goroutine that we're done unregistering.
			ch2 <- struct{}{}
		}()
	}

	// Let the goroutines register.
	close(ch1)
	for i := 0; i < concurrency; i++ {
		<-ch2
	}

	// Issue a notification.
	q.Notify(EventIn)
	if cnt != concurrency {
		t.Errorf("cnt = %d, want %d", cnt, concurrency)
	}

	// Let the goroutine unregister.
	close(ch3)
	for i := 0; i < concurrency; i++ {
		<-ch2
	}

	// Issue a notification.
	q.Notify(EventIn)
	if cnt != concurrency {
		t.Errorf("cnt = %d, want %d", cnt, concurrency)
	}
}

func TestConcurrentNotification(t *testing.T) {
	var q Queue
	var cnt atomicbitops.Int32
	const concurrency = 1000
	const waiterCount = 1000

	// Register waiters.
	for i := 0; i < waiterCount; i++ {
		e := NewFunctionEntry(EventIn|EventErr, func(mask EventMask) {
			cnt.Add(1)
			if mask != EventIn {
				t.Errorf("mask = %#x want %#x", mask, EventIn)
			}
		})

		q.EventRegister(&e)
	}

	// Launch notifiers.
	ch1 := make(chan struct{})
	ch2 := make(chan struct{})
	for i := 0; i < concurrency; i++ {
		go func() {
			<-ch1
			q.Notify(EventIn)
			ch2 <- struct{}{}
		}()
	}

	// Let notifiers go.
	close(ch1)
	for i := 0; i < concurrency; i++ {
		<-ch2
	}

	// Check the count.
	if cnt.Load() != concurrency*waiterCount {
		t.Errorf("cnt = %d, want %d", cnt.Load(), concurrency*waiterCount)
	}
}
