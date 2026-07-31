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

// Package ttcremote contains the transport-independent state machine used by
// TTC distributed live fork.
package ttcremote

import (
	"errors"
	"fmt"
)

// PageIndex identifies a base page within one remote MemoryFile.
type PageIndex uint64

// TransferID identifies one attempt to transfer a page. IDs are unique within
// a snapshot epoch.
type TransferID uint64

// TransferKind describes why a transfer was started.
type TransferKind uint8

const (
	// TransferPull is initiated by a target fault or prefetch request.
	TransferPull TransferKind = iota + 1
	// TransferPush is initiated by the source policy.
	TransferPush
)

func (k TransferKind) String() string {
	switch k {
	case TransferPull:
		return "pull"
	case TransferPush:
		return "push"
	default:
		return fmt.Sprintf("TransferKind(%d)", k)
	}
}

// SourcePageState is the state of the source's remote-snapshot reference.
type SourcePageState uint8

const (
	// SourceRequired means that the target may still require this page.
	SourceRequired SourcePageState = iota + 1
	// SourceAcknowledged means that the target installed and acknowledged it.
	SourceAcknowledged
	// SourceCanceled means that the target canceled its dependency.
	SourceCanceled
)

func (s SourcePageState) String() string {
	switch s {
	case SourceRequired:
		return "required"
	case SourceAcknowledged:
		return "acknowledged"
	case SourceCanceled:
		return "canceled"
	default:
		return fmt.Sprintf("SourcePageState(%d)", s)
	}
}

// TargetPageState is the target's installation state.
type TargetPageState uint8

const (
	// TargetMissing means that the target has not selected data to install.
	TargetMissing TargetPageState = iota + 1
	// TargetInstalling means that exactly one arrived transfer won installation.
	TargetInstalling
	// TargetPresent means that the fork-time page is installed.
	TargetPresent
	// TargetCanceled means that the target no longer uses this snapshot.
	TargetCanceled
)

func (s TargetPageState) String() string {
	switch s {
	case TargetMissing:
		return "missing"
	case TargetInstalling:
		return "installing"
	case TargetPresent:
		return "present"
	case TargetCanceled:
		return "canceled"
	default:
		return fmt.Sprintf("TargetPageState(%d)", s)
	}
}

// EventKind is a model transition.
type EventKind string

const (
	EventSourceWrite      EventKind = "source-write"
	EventBeginPull        EventKind = "begin-pull"
	EventBeginPush        EventKind = "begin-push"
	EventTransferComplete EventKind = "transfer-complete"
	EventDataArrive       EventKind = "data-arrive"
	EventInstall          EventKind = "install"
	EventAck              EventKind = "ack"
	EventCancel           EventKind = "cancel"
)

// Event describes one replayable protocol action.
//
// Epoch and Page are required by every event. Transfer is required by transfer
// events. Value is the new live value for EventSourceWrite and the received
// payload value for EventDataArrive.
type Event struct {
	Kind     EventKind  `json:"kind"`
	Epoch    uint64     `json:"epoch"`
	Page     PageIndex  `json:"page"`
	Transfer TransferID `json:"transfer,omitempty"`
	Value    uint64     `json:"value,omitempty"`
}

// Transfer is one page-data attempt. A completed transfer no longer pins the
// source page: its immutable bytes are then owned by the transport buffer.
type Transfer struct {
	ID        TransferID
	Page      PageIndex
	Kind      TransferKind
	Value     uint64
	Completed bool
	Arrived   bool
}

// PageState is the complete model state for one base page.
type PageState struct {
	SnapshotValue uint64
	SourceValue   uint64

	Source         SourcePageState
	SnapshotRef    bool
	TransferRefs   uint32
	ReferenceDrops uint32
	SourceDetached bool
	Orphaned       bool

	Target       TargetPageState
	TargetValue  uint64
	InstallingBy TransferID
	InstalledBy  TransferID
	EverPresent  bool
}

// Model represents a single source and a single remote target for one snapshot
// epoch. It is intentionally deterministic and transport-independent so traces
// from stress tests can be replayed as unit tests.
type Model struct {
	epoch     uint64
	pages     []PageState
	transfers map[TransferID]*Transfer
}

var (
	// ErrStaleEpoch indicates that an event belongs to another snapshot.
	ErrStaleEpoch = errors.New("stale snapshot epoch")
	// ErrCorruptData indicates that received bytes differ from the immutable
	// fork-time contents.
	ErrCorruptData = errors.New("remote page data differs from snapshot")
)

// NewModel creates a snapshot whose source and target pages contain the given
// logical values. Logical values stand in for complete page contents.
func NewModel(epoch uint64, values []uint64) *Model {
	m := &Model{
		epoch:     epoch,
		pages:     make([]PageState, len(values)),
		transfers: make(map[TransferID]*Transfer),
	}
	for i, value := range values {
		m.pages[i] = PageState{
			SnapshotValue: value,
			SourceValue:   value,
			Source:        SourceRequired,
			SnapshotRef:   true,
			Target:        TargetMissing,
		}
	}
	return m
}

// Epoch returns the model's snapshot epoch.
func (m *Model) Epoch() uint64 {
	return m.epoch
}

// PageCount returns the number of pages in the snapshot.
func (m *Model) PageCount() int {
	return len(m.pages)
}

// Page returns a copy of the state for page.
func (m *Model) Page(page PageIndex) (PageState, bool) {
	if uint64(page) >= uint64(len(m.pages)) {
		return PageState{}, false
	}
	return m.pages[page], true
}

// Transfer returns a copy of a transfer.
func (m *Model) Transfer(id TransferID) (Transfer, bool) {
	t, ok := m.transfers[id]
	if !ok {
		return Transfer{}, false
	}
	return *t, true
}

// SnapshotReferences returns the number of remote-snapshot references still
// held by the source. TransferReferences returns temporary references held by
// active senders.
func (m *Model) SnapshotReferences() (snapshot, transfer uint64) {
	for i := range m.pages {
		if m.pages[i].SnapshotRef {
			snapshot++
		}
		transfer += uint64(m.pages[i].TransferRefs)
	}
	return snapshot, transfer
}

// Apply applies e atomically. Invalid events return an error without changing
// model state. Duplicate completion, arrival, installation, ACK, and cancel
// events are idempotent.
func (m *Model) Apply(e Event) error {
	if e.Epoch != m.epoch {
		return fmt.Errorf("%w: got %d, want %d", ErrStaleEpoch, e.Epoch, m.epoch)
	}
	if uint64(e.Page) >= uint64(len(m.pages)) {
		return fmt.Errorf("page %d out of range [0, %d)", e.Page, len(m.pages))
	}
	p := &m.pages[e.Page]

	switch e.Kind {
	case EventSourceWrite:
		if !p.SourceDetached {
			p.SourceDetached = true
			if p.Source == SourceRequired {
				p.Orphaned = true
			}
		}
		p.SourceValue = e.Value

	case EventBeginPull, EventBeginPush:
		if e.Transfer == 0 {
			return errors.New("transfer ID 0 is reserved")
		}
		if _, ok := m.transfers[e.Transfer]; ok {
			return fmt.Errorf("transfer %d already exists", e.Transfer)
		}
		if p.Source != SourceRequired {
			return fmt.Errorf("page %d is no longer required: %s", e.Page, p.Source)
		}
		if p.Target == TargetCanceled {
			return fmt.Errorf("target page %d is canceled", e.Page)
		}
		kind := TransferPull
		if e.Kind == EventBeginPush {
			kind = TransferPush
		}
		m.transfers[e.Transfer] = &Transfer{
			ID:    e.Transfer,
			Page:  e.Page,
			Kind:  kind,
			Value: p.SnapshotValue,
		}
		p.TransferRefs++

	case EventTransferComplete:
		t, err := m.transferForEvent(e)
		if err != nil {
			return err
		}
		if !t.Completed {
			if p.TransferRefs == 0 {
				return fmt.Errorf("page %d transfer reference underflow", e.Page)
			}
			t.Completed = true
			p.TransferRefs--
		}

	case EventDataArrive:
		t, err := m.transferForEvent(e)
		if err != nil {
			return err
		}
		if e.Value != t.Value {
			return fmt.Errorf("%w: page %d transfer %d got %#x, want %#x", ErrCorruptData, e.Page, e.Transfer, e.Value, t.Value)
		}
		t.Arrived = true
		switch p.Target {
		case TargetMissing:
			p.Target = TargetInstalling
			p.InstallingBy = e.Transfer
		case TargetInstalling, TargetPresent, TargetCanceled:
			// Another identical transfer may have won, or the target no longer
			// needs this data. The payload is safely discarded.
		}

	case EventInstall:
		t, err := m.transferForEvent(e)
		if err != nil {
			return err
		}
		if !t.Arrived {
			return fmt.Errorf("transfer %d has not arrived", e.Transfer)
		}
		switch p.Target {
		case TargetInstalling:
			if p.InstallingBy != e.Transfer {
				// An identical transfer already owns installation.
				return nil
			}
			p.Target = TargetPresent
			p.TargetValue = t.Value
			p.InstalledBy = e.Transfer
			p.InstallingBy = 0
			p.EverPresent = true
		case TargetPresent, TargetCanceled:
			return nil
		case TargetMissing:
			return fmt.Errorf("page %d has no installing transfer", e.Page)
		}

	case EventAck:
		if p.Target != TargetPresent && !p.EverPresent {
			return fmt.Errorf("page %d cannot be acknowledged before installation", e.Page)
		}
		m.releaseSnapshotReference(p, SourceAcknowledged)

	case EventCancel:
		p.Target = TargetCanceled
		p.InstallingBy = 0
		m.releaseSnapshotReference(p, SourceCanceled)

	default:
		return fmt.Errorf("unknown event kind %q", e.Kind)
	}
	return m.Check()
}

func (m *Model) transferForEvent(e Event) (*Transfer, error) {
	t, ok := m.transfers[e.Transfer]
	if !ok {
		return nil, fmt.Errorf("unknown transfer %d", e.Transfer)
	}
	if t.Page != e.Page {
		return nil, fmt.Errorf("transfer %d belongs to page %d, not %d", e.Transfer, t.Page, e.Page)
	}
	return t, nil
}

func (m *Model) releaseSnapshotReference(p *PageState, terminal SourcePageState) {
	if p.Source != SourceRequired {
		return
	}
	p.Source = terminal
	p.SnapshotRef = false
	p.ReferenceDrops++
}

// Check verifies all model invariants.
func (m *Model) Check() error {
	inflightByPage := make([]uint32, len(m.pages))
	for id, t := range m.transfers {
		if id == 0 || id != t.ID {
			return fmt.Errorf("invalid transfer identity: map key %d, transfer %d", id, t.ID)
		}
		if uint64(t.Page) >= uint64(len(m.pages)) {
			return fmt.Errorf("transfer %d has out-of-range page %d", id, t.Page)
		}
		if t.Value != m.pages[t.Page].SnapshotValue {
			return fmt.Errorf("transfer %d value changed", id)
		}
		if !t.Completed {
			inflightByPage[t.Page]++
		}
	}

	for i := range m.pages {
		p := &m.pages[i]
		if p.TransferRefs != inflightByPage[i] {
			return fmt.Errorf("page %d has %d transfer refs, want %d", i, p.TransferRefs, inflightByPage[i])
		}
		if p.SnapshotRef != (p.Source == SourceRequired) {
			return fmt.Errorf("page %d snapshot ref=%t in source state %s", i, p.SnapshotRef, p.Source)
		}
		wantDrops := uint32(0)
		if p.Source != SourceRequired {
			wantDrops = 1
		}
		if p.ReferenceDrops != wantDrops {
			return fmt.Errorf("page %d has %d snapshot reference drops, want %d", i, p.ReferenceDrops, wantDrops)
		}
		if p.Orphaned && !p.SourceDetached {
			return fmt.Errorf("page %d is orphaned before source CoW", i)
		}
		switch p.Target {
		case TargetMissing:
			if p.InstallingBy != 0 || p.InstalledBy != 0 || p.EverPresent {
				return fmt.Errorf("missing target page %d retains installation state", i)
			}
		case TargetInstalling:
			t, ok := m.transfers[p.InstallingBy]
			if !ok || t.Page != PageIndex(i) || !t.Arrived {
				return fmt.Errorf("page %d has invalid installing transfer %d", i, p.InstallingBy)
			}
		case TargetPresent:
			t, ok := m.transfers[p.InstalledBy]
			if !ok || t.Page != PageIndex(i) || !t.Arrived {
				return fmt.Errorf("page %d has invalid installed transfer %d", i, p.InstalledBy)
			}
			if p.TargetValue != p.SnapshotValue || !p.EverPresent {
				return fmt.Errorf("page %d installed %#x, snapshot is %#x", i, p.TargetValue, p.SnapshotValue)
			}
		case TargetCanceled:
			if p.InstallingBy != 0 {
				return fmt.Errorf("canceled target page %d is still installing", i)
			}
		default:
			return fmt.Errorf("page %d has invalid target state %d", i, p.Target)
		}
		if p.Source == SourceAcknowledged && !p.EverPresent {
			return fmt.Errorf("page %d acknowledged without ever being present", i)
		}
	}
	return nil
}
