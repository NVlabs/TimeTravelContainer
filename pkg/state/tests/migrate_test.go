// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/state"
)

// The types below are written out by hand, as stateify would write them, so
// that two of them can be one type as two builds save it.

// payload is referenced only by a field that a later build drops.
type payload struct {
	V    int
	Next *payload
}

func (*payload) StateTypeName() string { return "pkg/state/tests.payload" }
func (*payload) StateFields() []string { return []string{"V", "Next"} }
func (p *payload) StateSave(s state.Sink) {
	s.Save(0, &p.V)
	s.Save(1, &p.Next)
}
func (p *payload) StateLoad(_ context.Context, s state.Source) {
	s.Load(0, &p.V)
	s.Load(1, &p.Next)
}

// oldWidget is a widget as an older build saved it.
type oldWidget struct {
	Kept    int
	Gone    *payload
	OldName string
}

func (*oldWidget) StateTypeName() string { return "pkg/state/tests.widget" }
func (*oldWidget) StateFields() []string { return []string{"Kept", "Gone", "OldName"} }
func (w *oldWidget) StateSave(s state.Sink) {
	s.Save(0, &w.Kept)
	s.Save(1, &w.Gone)
	s.Save(2, &w.OldName)
}
func (w *oldWidget) StateLoad(_ context.Context, s state.Source) {
	s.Load(0, &w.Kept)
	s.Load(1, &w.Gone)
	s.Load(2, &w.OldName)
}

// widget is the widget of this build: Gone was dropped, OldName became Name,
// and Zeroed, Doubled and Marker were added.
type widget struct {
	Kept    int
	Name    string
	Zeroed  int
	Doubled int
	Marker  struct{}

	// loadedDoubled is what afterLoad saw of Doubled.
	loadedDoubled int

	// lateFixed is set by the migration's FixAfterLoad, if it ran after
	// afterLoad.
	lateFixed bool
}

func (*widget) StateTypeName() string { return "pkg/state/tests.widget" }
func (*widget) StateFields() []string {
	return []string{"Kept", "Name", "Zeroed", "Doubled", "Marker"}
}
func (w *widget) StateSave(s state.Sink) {
	s.Save(0, &w.Kept)
	s.Save(1, &w.Name)
	s.Save(2, &w.Zeroed)
	s.Save(3, &w.Doubled)
	s.Save(4, &w.Marker)
}
func (w *widget) StateLoad(_ context.Context, s state.Source) {
	s.Load(0, &w.Kept)
	s.Load(1, &w.Name)
	s.Load(2, &w.Zeroed)
	s.Load(3, &w.Doubled)
	s.Load(4, &w.Marker)
	s.AfterLoad(func() { w.loadedDoubled = w.Doubled })
}

// oldGadget and gadget differ, and no migration says how.
type oldGadget struct{ A int }

func (*oldGadget) StateTypeName() string                         { return "pkg/state/tests.gadget" }
func (*oldGadget) StateFields() []string                         { return []string{"A"} }
func (g *oldGadget) StateSave(s state.Sink)                      { s.Save(0, &g.A) }
func (g *oldGadget) StateLoad(_ context.Context, s state.Source) { s.Load(0, &g.A) }

type gadget struct{ A, B int }

func (*gadget) StateTypeName() string { return "pkg/state/tests.gadget" }
func (*gadget) StateFields() []string { return []string{"A", "B"} }
func (g *gadget) StateSave(s state.Sink) {
	s.Save(0, &g.A)
	s.Save(1, &g.B)
}
func (g *gadget) StateLoad(_ context.Context, s state.Source) {
	s.Load(0, &g.A)
	s.Load(1, &g.B)
}

// oldThing was renamed thing; holder holds one in an interface.
type oldThing struct{ X int }

func (*oldThing) StateTypeName() string                         { return "pkg/state/tests.oldThing" }
func (*oldThing) StateFields() []string                         { return []string{"X"} }
func (t *oldThing) StateSave(s state.Sink)                      { s.Save(0, &t.X) }
func (t *oldThing) StateLoad(_ context.Context, s state.Source) { s.Load(0, &t.X) }

type thing struct{ X int }

func (*thing) StateTypeName() string                         { return "pkg/state/tests.thing" }
func (*thing) StateFields() []string                         { return []string{"X"} }
func (t *thing) StateSave(s state.Sink)                      { s.Save(0, &t.X) }
func (t *thing) StateLoad(_ context.Context, s state.Source) { s.Load(0, &t.X) }

type holder struct{ V any }

func (*holder) StateTypeName() string                         { return "pkg/state/tests.holder" }
func (*holder) StateFields() []string                         { return []string{"V"} }
func (h *holder) StateSave(s state.Sink)                      { s.Save(0, &h.V) }
func (h *holder) StateLoad(_ context.Context, s state.Source) { s.Load(0, &h.V) }

func init() {
	state.Register((*payload)(nil))
	state.Register((*thing)(nil))
	state.Register((*oldThing)(nil))
	state.RegisterMigration(state.Migration{
		Type:    "pkg/state/tests.widget",
		Fields:  []string{"Kept", "Gone", "OldName"},
		Renamed: map[string]string{"OldName": "Name"},
		Dropped: []string{"Gone"},
		Zero:    []string{"Zeroed"},
		Set:     []string{"Doubled"},
		Fix:     func(obj any, _ map[string]any) { w := obj.(*widget); w.Doubled = 2 * w.Kept },
		FixAfterLoad: func(obj any, _ map[string]any) {
			w := obj.(*widget)
			w.lateFixed = w.loadedDoubled == 42
		},
	})
	state.RegisterMigration(state.Migration{
		Type:    "pkg/state/tests.thing",
		OldName: "pkg/state/tests.oldThing",
	})
}

// saved returns what Save writes of obj.
func saved(t *testing.T, obj any) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := state.Save(context.Background(), &buf, obj); err != nil {
		t.Fatalf("saving %T: %v", obj, err)
	}
	return buf.Bytes()
}

// A type that an older build saved with other fields loads under its
// migration: renamed fields keep their values, dropped ones go with what only
// they referenced, and added ones get the migration's values before the
// type's own afterLoad sees them.
func TestMigrationLoadsAnOlderShape(t *testing.T) {
	data := saved(t, &oldWidget{Kept: 21, Gone: &payload{V: 7, Next: &payload{V: 8}}, OldName: "w"})
	var w widget
	if _, err := state.Load(context.Background(), bytes.NewReader(data), &w); err != nil {
		t.Fatalf("loading: %v", err)
	}
	if w.Kept != 21 || w.Name != "w" || w.Zeroed != 0 || w.Doubled != 42 {
		t.Errorf("loaded %+v, want Kept 21, Name w, Zeroed 0, Doubled 42", w)
	}
	if w.loadedDoubled != 42 {
		t.Errorf("afterLoad saw Doubled %d, want 42: the migration's fix runs first", w.loadedDoubled)
	}
	if !w.lateFixed {
		t.Error("FixAfterLoad did not run after afterLoad")
	}
}

// An older shape that no migration covers is refused, with an error that
// names the type and the fields that differ, and no stack.
func TestAnOlderShapeWithoutAMigrationIsRefused(t *testing.T) {
	data := saved(t, &oldGadget{A: 1})
	var g gadget
	_, err := state.Load(context.Background(), bytes.NewReader(data), &g)
	if !state.IsSchemaError(err) {
		t.Fatalf("loading an older gadget: %v, want a schema error", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "pkg/state/tests.gadget") || !strings.Contains(msg, "B") || strings.Contains(msg, "goroutine") {
		t.Errorf("error %q should name the type and the field, and carry no stack", msg)
	}
}

// A renamed type loads as its new name, here held in an interface.
func TestARenamedTypeLoadsAsItsNewName(t *testing.T) {
	data := saved(t, &holder{V: &oldThing{X: 5}})
	var h holder
	if _, err := state.Load(context.Background(), bytes.NewReader(data), &h); err != nil {
		t.Fatalf("loading: %v", err)
	}
	if got, ok := h.V.(*thing); !ok || got.X != 5 {
		t.Errorf("loaded %#v, want &thing{X: 5}", h.V)
	}
}

// CheckMigration answers, without an image, whether this build loads a type
// that an older build saved with some fields.
func TestCheckMigration(t *testing.T) {
	for _, c := range []struct {
		name   string
		fields []string
		ok     bool
	}{
		{"pkg/state/tests.thing", []string{"X"}, true},
		{"pkg/state/tests.oldThing", []string{"X"}, true},
		{"pkg/state/tests.thing", []string{"X", "Y"}, false},
		{"pkg/state/tests.gone", []string{"X"}, false},
	} {
		if err := state.CheckMigration(c.name, state.SchemaType{Fields: c.fields}); (err == nil) != c.ok {
			t.Errorf("CheckMigration(%s, %v) = %v, want ok %t", c.name, c.fields, err, c.ok)
		}
	}
}

// oldMarked and marked differ only in marked's empty marker.
type oldMarked struct{ A int }

func (*oldMarked) StateTypeName() string                         { return "pkg/state/tests.marked" }
func (*oldMarked) StateFields() []string                         { return []string{"A"} }
func (m *oldMarked) StateSave(s state.Sink)                      { s.Save(0, &m.A) }
func (m *oldMarked) StateLoad(_ context.Context, s state.Source) { s.Load(0, &m.A) }

type marked struct {
	A      int
	Marker struct{}
}

func (*marked) StateTypeName() string { return "pkg/state/tests.marked" }
func (*marked) StateFields() []string { return []string{"A", "Marker"} }
func (m *marked) StateSave(s state.Sink) {
	s.Save(0, &m.A)
	s.Save(1, &m.Marker)
}
func (m *marked) StateLoad(_ context.Context, s state.Source) {
	s.Load(0, &m.A)
	s.Load(1, &m.Marker)
}

// A field of an empty struct type that a build adds, as a marker embedded for
// its methods, has no value to choose: it needs no migration.
func TestAnAddedEmptyFieldNeedsNoMigration(t *testing.T) {
	data := saved(t, &oldMarked{A: 3})
	var m marked
	if _, err := state.Load(context.Background(), bytes.NewReader(data), &m); err != nil || m.A != 3 {
		t.Errorf("loading: %+v, %v; want A 3", m, err)
	}
}

// oldCounter saved a count and its time apart; counter packs them into Count.
type oldCounter struct {
	Count uint32
	Time  int64
}

func (*oldCounter) StateTypeName() string { return "pkg/state/tests.counter" }
func (*oldCounter) StateFields() []string { return []string{"Count", "Time"} }
func (c *oldCounter) StateSave(s state.Sink) {
	s.Save(0, &c.Count)
	s.Save(1, &c.Time)
}
func (c *oldCounter) StateLoad(_ context.Context, s state.Source) {
	s.Load(0, &c.Count)
	s.Load(1, &c.Time)
}

type counter struct{ Count uint64 }

func (*counter) StateTypeName() string                         { return "pkg/state/tests.counter" }
func (*counter) StateFields() []string                         { return []string{"Count"} }
func (c *counter) StateSave(s state.Sink)                      { s.Save(0, &c.Count) }
func (c *counter) StateLoad(_ context.Context, s state.Source) { s.Load(0, &c.Count) }

func init() {
	state.RegisterMigration(state.Migration{
		Type:   "pkg/state/tests.counter",
		Fields: []string{"Count", "Time"},
		Set:    []string{"Count"},
		Old:    map[string]any{"Count": new(uint32), "Time": new(int64)},
		Fix: func(obj any, old map[string]any) {
			obj.(*counter).Count = uint64(*old["Count"].(*uint32)) | uint64(*old["Time"].(*int64))<<8
		},
	})
}

// A field that a build saves as another type under the same name loads from
// the older value, through the migration's fix.
func TestMigrationConvertsAFieldThatChangedType(t *testing.T) {
	data := saved(t, &oldCounter{Count: 3, Time: 5})
	var c counter
	if _, err := state.Load(context.Background(), bytes.NewReader(data), &c); err != nil {
		t.Fatalf("loading: %v", err)
	}
	if want := uint64(3 | 5<<8); c.Count != want {
		t.Errorf("Count %#x, want %#x", c.Count, want)
	}
}

// flag is a bool kept atomically, as atomicbitops.Bool is.
type flag struct{ v uint32 }

func (*flag) StateTypeName() string                         { return "pkg/state/tests.flag" }
func (*flag) StateFields() []string                         { return []string{"v"} }
func (f *flag) StateSave(s state.Sink)                      { s.Save(0, &f.v) }
func (f *flag) StateLoad(_ context.Context, s state.Source) { s.Load(0, &f.v) }
func (f *flag) Store(b bool) {
	f.v = 0
	if b {
		f.v = 1
	}
}

// oldShapes saved a time as its UnixNano, a bool, and a set by value;
// shapes keeps a time, an atomic bool, and a pointer to the set.
type oldShapes struct {
	When int64
	On   bool
	Set  payload
}

func (*oldShapes) StateTypeName() string { return "pkg/state/tests.shapes" }
func (*oldShapes) StateFields() []string { return []string{"When", "On", "Set"} }
func (o *oldShapes) StateSave(s state.Sink) {
	s.Save(0, &o.When)
	s.Save(1, &o.On)
	s.Save(2, &o.Set)
}
func (o *oldShapes) StateLoad(_ context.Context, s state.Source) {
	s.Load(0, &o.When)
	s.Load(1, &o.On)
	s.Load(2, &o.Set)
}

type shapes struct {
	When time.Time
	On   flag
	Set  *payload
}

func (*shapes) StateTypeName() string { return "pkg/state/tests.shapes" }
func (*shapes) StateFields() []string { return []string{"When", "On", "Set"} }
func (o *shapes) StateSave(s state.Sink) {
	s.Save(0, &o.When)
	s.Save(1, &o.On)
	s.Save(2, &o.Set)
}
func (o *shapes) StateLoad(_ context.Context, s state.Source) {
	s.Load(0, &o.When)
	s.Load(1, &o.On)
	s.Load(2, &o.Set)
}

// What older builds saved as another type, where only one meaning is
// possible, loads into the type that this build keeps: a time saved as its
// UnixNano, a bool now kept atomically, a struct now kept by pointer.
func TestEncodingsThatChangedAreCoerced(t *testing.T) {
	when := time.Unix(1700000000, 123)
	data := saved(t, &oldShapes{When: when.UnixNano(), On: true, Set: payload{V: 9}})
	var o shapes
	if _, err := state.Load(context.Background(), bytes.NewReader(data), &o); err != nil {
		t.Fatalf("loading: %v", err)
	}
	if !o.When.Equal(when) || o.On.v != 1 || o.Set == nil || o.Set.V != 9 {
		t.Errorf("loaded %v, %+v, %+v; want %v, on, &{V: 9}", o.When, o.On, o.Set, when)
	}
}
