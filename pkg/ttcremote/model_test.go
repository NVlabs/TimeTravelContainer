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

package ttcremote

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
)

func applyEvents(t *testing.T, m *Model, events ...Event) {
	t.Helper()
	for i, event := range events {
		if err := m.Apply(event); err != nil {
			t.Fatalf("event %d (%+v): %v", i, event, err)
		}
	}
}

func event(kind EventKind, page PageIndex, transfer TransferID, value uint64) Event {
	return Event{Kind: kind, Epoch: 7, Page: page, Transfer: transfer, Value: value}
}

func TestPullAckReleasesSnapshotReference(t *testing.T) {
	m := NewModel(7, []uint64{0x1234})
	applyEvents(t, m,
		event(EventBeginPull, 0, 1, 0),
		event(EventTransferComplete, 0, 1, 0),
		event(EventDataArrive, 0, 1, 0x1234),
		event(EventInstall, 0, 1, 0),
		event(EventAck, 0, 0, 0),
		event(EventAck, 0, 0, 0), // Duplicate ACK.
		event(EventSourceWrite, 0, 0, 0x5678),
	)
	p, _ := m.Page(0)
	if p.Source != SourceAcknowledged || p.SnapshotRef || p.ReferenceDrops != 1 {
		t.Fatalf("source state after ACK: %+v", p)
	}
	if p.Orphaned {
		t.Fatalf("write after ACK incorrectly orphaned the old page: %+v", p)
	}
	if p.TargetValue != 0x1234 || p.SourceValue != 0x5678 {
		t.Fatalf("branch values: target=%#x source=%#x", p.TargetValue, p.SourceValue)
	}
	if snapshot, transfer := m.SnapshotReferences(); snapshot != 0 || transfer != 0 {
		t.Fatalf("references after ACK: snapshot=%d transfer=%d", snapshot, transfer)
	}
}

func TestPullPushRace(t *testing.T) {
	for _, first := range []TransferID{1, 2} {
		t.Run(fmt.Sprintf("transfer-%d-wins", first), func(t *testing.T) {
			m := NewModel(7, []uint64{0x1111})
			applyEvents(t, m,
				event(EventBeginPull, 0, 1, 0),
				event(EventSourceWrite, 0, 0, 0x2222),
				event(EventBeginPush, 0, 2, 0),
				event(EventDataArrive, 0, first, 0x1111),
				event(EventDataArrive, 0, 3-first, 0x1111),
				event(EventInstall, 0, first, 0),
				event(EventInstall, 0, 3-first, 0),
				event(EventAck, 0, 0, 0),
				event(EventTransferComplete, 0, 1, 0),
				event(EventTransferComplete, 0, 2, 0),
			)
			p, _ := m.Page(0)
			if !p.Orphaned || p.InstalledBy != first || p.TargetValue != 0x1111 || p.SourceValue != 0x2222 {
				t.Fatalf("unexpected final state: %+v", p)
			}
			if p.ReferenceDrops != 1 {
				t.Fatalf("snapshot reference dropped %d times", p.ReferenceDrops)
			}
		})
	}
}

func TestCancelAndDelayedMessages(t *testing.T) {
	m := NewModel(7, []uint64{9})
	applyEvents(t, m,
		event(EventBeginPull, 0, 1, 0),
		event(EventCancel, 0, 0, 0),
		event(EventCancel, 0, 0, 0),
		event(EventTransferComplete, 0, 1, 0),
		event(EventDataArrive, 0, 1, 9),
		event(EventInstall, 0, 1, 0),
	)
	p, _ := m.Page(0)
	if p.Source != SourceCanceled || p.Target != TargetCanceled || p.ReferenceDrops != 1 {
		t.Fatalf("state after cancel and delayed data: %+v", p)
	}
}

func TestInvalidEventsAreAtomic(t *testing.T) {
	tests := []struct {
		name  string
		event Event
		want  error
	}{
		{"stale epoch", Event{Kind: EventSourceWrite, Epoch: 8, Page: 0, Value: 2}, ErrStaleEpoch},
		{"bad page", event(EventSourceWrite, 2, 0, 2), nil},
		{"unknown transfer", event(EventDataArrive, 0, 99, 1), nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := NewModel(7, []uint64{1})
			before, _ := m.Page(0)
			err := m.Apply(test.event)
			if err == nil {
				t.Fatalf("Apply(%+v) succeeded", test.event)
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("Apply error %v, want %v", err, test.want)
			}
			after, _ := m.Page(0)
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("invalid event changed page state: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestCorruptDataRejectedAtomically(t *testing.T) {
	m := NewModel(7, []uint64{0xaaaa})
	applyEvents(t, m, event(EventBeginPull, 0, 1, 0))
	before, _ := m.Page(0)
	if err := m.Apply(event(EventDataArrive, 0, 1, 0xbbbb)); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("corrupt arrival error: %v, want %v", err, ErrCorruptData)
	}
	after, _ := m.Page(0)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("corrupt data changed page state: before=%+v after=%+v", before, after)
	}
	transfer, _ := m.Transfer(1)
	if transfer.Arrived {
		t.Fatalf("corrupt transfer marked arrived: %+v", transfer)
	}
}

type scheduleAction struct {
	name  string
	event Event
}

func TestBoundedPullPushInterleavings(t *testing.T) {
	actions := []scheduleAction{
		{"source-write", event(EventSourceWrite, 0, 0, 0x20)},
		{"pull-complete", event(EventTransferComplete, 0, 1, 0)},
		{"push-complete", event(EventTransferComplete, 0, 2, 0)},
		{"pull-arrive", event(EventDataArrive, 0, 1, 0x10)},
		{"push-arrive", event(EventDataArrive, 0, 2, 0x10)},
	}
	var schedules int
	forEachPermutation(actions, func(order []scheduleAction) {
		schedules++
		m := NewModel(7, []uint64{0x10})
		applyEvents(t, m,
			event(EventBeginPull, 0, 1, 0),
			event(EventBeginPush, 0, 2, 0),
		)
		firstArrival := TransferID(0)
		for _, action := range order {
			if action.event.Kind == EventDataArrive && firstArrival == 0 {
				firstArrival = action.event.Transfer
			}
			if err := m.Apply(action.event); err != nil {
				t.Fatalf("schedule %v, action %s: %v", actionNames(order), action.name, err)
			}
		}
		other := TransferID(3) - firstArrival
		applyEvents(t, m,
			event(EventInstall, 0, firstArrival, 0),
			event(EventInstall, 0, other, 0),
			event(EventAck, 0, 0, 0),
		)
		p, _ := m.Page(0)
		if p.TargetValue != 0x10 || p.SourceValue != 0x20 || p.ReferenceDrops != 1 {
			t.Fatalf("schedule %v produced %+v", actionNames(order), p)
		}
	})
	if schedules != 120 {
		t.Fatalf("tested %d schedules, want 120", schedules)
	}
}

func forEachPermutation[T any](values []T, fn func([]T)) {
	work := append([]T(nil), values...)
	var visit func(int)
	visit = func(i int) {
		if i == len(work) {
			fn(append([]T(nil), work...))
			return
		}
		for j := i; j < len(work); j++ {
			work[i], work[j] = work[j], work[i]
			visit(i + 1)
			work[i], work[j] = work[j], work[i]
		}
	}
	visit(0)
}

func actionNames(actions []scheduleAction) []string {
	names := make([]string, len(actions))
	for i := range actions {
		names[i] = actions[i].name
	}
	return names
}

func TestRandomizedSchedules(t *testing.T) {
	const schedules = 10_000
	for seed := int64(0); seed < schedules; seed++ {
		rng := rand.New(rand.NewSource(seed))
		values := []uint64{0x100, 0x101, 0x102, 0x103}
		m := NewModel(7, values)
		trace := NewTrace(7, values)
		nextTransfer := TransferID(1)
		for step := 0; step < 64; step++ {
			page := PageIndex(rng.Intn(len(values)))
			p, _ := m.Page(page)
			var e Event
			switch rng.Intn(8) {
			case 0:
				e = event(EventSourceWrite, page, 0, uint64(seed)<<16|uint64(step+1))
			case 1, 2:
				kind := EventBeginPull
				if rng.Intn(2) == 0 {
					kind = EventBeginPush
				}
				e = event(kind, page, nextTransfer, 0)
				nextTransfer++
			case 3, 4, 5:
				if nextTransfer == 1 {
					continue
				}
				id := TransferID(1 + rng.Intn(int(nextTransfer-1)))
				transfer, ok := m.Transfer(id)
				if !ok {
					continue
				}
				page = transfer.Page
				kind := []EventKind{EventTransferComplete, EventDataArrive, EventInstall}[rng.Intn(3)]
				value := uint64(0)
				if kind == EventDataArrive {
					value = transfer.Value
				}
				e = event(kind, page, id, value)
			case 6:
				if p.Target != TargetPresent {
					continue
				}
				e = event(EventAck, page, 0, 0)
			case 7:
				e = event(EventCancel, page, 0, 0)
			}
			if err := trace.Append(m, e); err != nil {
				// Random schedules intentionally generate actions that lose a
				// race. Rejection is valid, but the model must remain sound.
				if checkErr := m.Check(); checkErr != nil {
					t.Fatalf("seed %d step %d rejected %+v with %v, then invariant failed: %v", seed, step, e, err, checkErr)
				}
			}
		}
		for page := range values {
			_ = trace.Append(m, event(EventCancel, PageIndex(page), 0, 0))
		}
		for id := TransferID(1); id < nextTransfer; id++ {
			transfer, ok := m.Transfer(id)
			if ok && !transfer.Completed {
				if err := trace.Append(m, event(EventTransferComplete, transfer.Page, id, 0)); err != nil {
					t.Fatalf("seed %d completing transfer %d: %v", seed, id, err)
				}
			}
		}
		if snapshot, transfer := m.SnapshotReferences(); snapshot != 0 || transfer != 0 {
			var encoded bytes.Buffer
			_ = EncodeTrace(&encoded, trace)
			t.Fatalf("seed %d leaked references: snapshot=%d transfer=%d\n%s", seed, snapshot, transfer, encoded.String())
		}
		if _, err := trace.Replay(); err != nil {
			t.Fatalf("seed %d trace did not replay: %v", seed, err)
		}
	}
}

func TestTraceRoundTrip(t *testing.T) {
	m := NewModel(7, []uint64{4})
	trace := NewTrace(7, []uint64{4})
	for _, e := range []Event{
		event(EventBeginPull, 0, 1, 0),
		event(EventDataArrive, 0, 1, 4),
		event(EventInstall, 0, 1, 0),
		event(EventAck, 0, 0, 0),
		event(EventTransferComplete, 0, 1, 0),
	} {
		if err := trace.Append(m, e); err != nil {
			t.Fatal(err)
		}
	}
	var encoded bytes.Buffer
	if err := EncodeTrace(&encoded, trace); err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeTrace(&encoded)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := decoded.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := replayed.pages, m.pages; !reflect.DeepEqual(got, want) {
		t.Fatalf("replayed pages %+v, want %+v", got, want)
	}
}

func FuzzDecodeAndReplayTrace(f *testing.F) {
	seed := Trace{
		Version:       traceVersion,
		Epoch:         1,
		InitialValues: []uint64{1},
		Events: []Event{
			{Kind: EventBeginPull, Epoch: 1, Page: 0, Transfer: 1},
			{Kind: EventDataArrive, Epoch: 1, Page: 0, Transfer: 1, Value: 1},
		},
	}
	data, _ := json.Marshal(seed)
	f.Add(data)
	f.Fuzz(func(t *testing.T, data []byte) {
		trace, err := DecodeTrace(bytes.NewReader(data))
		if err != nil {
			return
		}
		model, err := trace.Replay()
		if err == nil {
			if err := model.Check(); err != nil {
				t.Fatalf("Replay succeeded with broken invariants: %v", err)
			}
		}
	})
}
