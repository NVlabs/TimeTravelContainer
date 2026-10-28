// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"sync"
	"testing"
	"time"
	"unsafe"

	"gvisor.dev/gvisor/pkg/state"
)

// cloneAliases is a graph of cloneNodes with cycles, pointers into other
// objects, slices that share arrays, shared maps, and interfaces.
type cloneAliases struct {
	root *cloneRoot
	a, b *cloneNode
	leaf *cloneLeaf
}

func newCloneAliases() cloneAliases {
	leaf := &cloneLeaf{}
	r := &cloneRoot{}
	a := &cloneNode{val: 1, waitFor: leaf}
	b := &cloneNode{val: 2, next: a, waitFor: leaf}
	a.next = b
	a.peer = &b.val
	a.buf = make([]byte, 8, 16)
	copy(a.buf, "abcdefgh")
	a.view = a.buf[2:6]
	a.m = map[string]*cloneNode{"b": b}
	b.m = a.m
	a.any = b
	b.any = cloneValue{x: 7, p: a}
	r.a, r.b = a, b
	r.arr[1].val = 3
	r.arr[1].next = a
	r.inArr = &r.arr[1]
	r.inner = cloneValue{x: 9, p: &r.arr[0]}
	return cloneAliases{root: r, a: a, b: b, leaf: leaf}
}

// check checks that c is a copy of the graph as newCloneAliases made it,
// which keeps what points where, all within the copy.
func (g cloneAliases) check(t *testing.T, c *cloneRoot) {
	t.Helper()
	a, b := g.a, g.b
	if c.a == a || c.b == b || c.a == nil || c.b == nil {
		t.Fatalf("the clone's nodes are the source's, or nil: %p %p", c.a, c.b)
	}
	if c.a.val != 1 || c.b.val != 2 || c.arr[1].val != 3 {
		t.Errorf("the nodes' values are %d, %d, %d, want 1, 2, 3", c.a.val, c.b.val, c.arr[1].val)
	}
	if c.a.next != c.b || c.b.next != c.a {
		t.Errorf("the clone's cycle is broken")
	}
	if c.a.peer != &c.b.val {
		t.Errorf("a pointer into another node points to %p, want %p", c.a.peer, &c.b.val)
	}
	if string(c.a.buf) != "abcdefgh" || cap(c.a.buf) != 16 {
		t.Errorf("buf is %q of capacity %d", c.a.buf, cap(c.a.buf))
	}
	if len(c.a.view) != 4 || cap(c.a.view) != 14 || &c.a.view[0] != &c.a.buf[2] {
		t.Errorf("view does not share buf's array")
	}
	if reflect.ValueOf(c.a.m).UnsafePointer() != reflect.ValueOf(c.b.m).UnsafePointer() ||
		reflect.ValueOf(c.a.m).UnsafePointer() == reflect.ValueOf(a.m).UnsafePointer() {
		t.Errorf("the nodes' map is not one map of the clone's")
	}
	if len(c.a.m) != 1 || c.a.m["b"] != c.b {
		t.Errorf("the map is %v, want b alone", c.a.m)
	}
	if c.a.any != any(c.b) {
		t.Errorf("an interface holds %v, want the clone's b", c.a.any)
	}
	if v, ok := c.b.any.(cloneValue); !ok || v.x != 7 || v.p != c.a {
		t.Errorf("an interface holds %#v, want a value that points to the clone's a", c.b.any)
	}
	if c.inArr != &c.arr[1] || c.arr[1].next != c.a {
		t.Errorf("a pointer into an array points to %p, want %p", c.inArr, &c.arr[1])
	}
	if c.inner.x != 9 || c.inner.p != &c.arr[0] {
		t.Errorf("a struct held by value is %#v", c.inner)
	}
	if c.a.loaded != 1 || c.b.loaded != 1 || c.arr[1].loaded != 1 {
		t.Errorf("afterLoad ran %d, %d, %d times, want once each", c.a.loaded, c.b.loaded, c.arr[1].loaded)
	}
	if c.a.waitFor == g.leaf || c.a.waitFor != c.b.waitFor || c.a.waitFor.loaded != 1 {
		t.Errorf("the leaf is not the clone's, or ran afterLoad %d times", c.a.waitFor.loaded)
	}
	if c.a.waited != 1 || c.b.waited != 1 {
		t.Errorf("a node's afterLoad ran before its leaf's")
	}
	// Nothing of the clone is the source's.
	for _, p := range []unsafe.Pointer{unsafe.Pointer(c.a.next), unsafe.Pointer(c.b.next), unsafe.Pointer(c.a.peer), unsafe.Pointer(&c.a.buf[0])} {
		for _, q := range []unsafe.Pointer{unsafe.Pointer(a), unsafe.Pointer(b), unsafe.Pointer(&b.val), unsafe.Pointer(&a.buf[0])} {
			if p == q {
				t.Errorf("the clone points to the source's %p", q)
			}
		}
	}
}

// TestCloneAliases checks that a clone keeps what points where.
func TestCloneAliases(t *testing.T) {
	g := newCloneAliases()
	var c cloneRoot
	if _, err := state.Clone(context.Background(), context.Background(), g.root, &c, state.SaveOptions{}); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	g.check(t, &c)
}

// TestSaveKeepsSharedArrays checks that a save leaves alone the unused
// capacity of a slice where another slice of the same array holds live
// elements, and that a load keeps them and the slices' sharing. (The encoder,
// unlike a clone, has no way to refer to an array from inside another one, so
// the slices start together.)
func TestSaveKeepsSharedArrays(t *testing.T) {
	n := &cloneNode{buf: make([]byte, 8, 16)}
	copy(n.buf, "abcdefgh")
	n.view = n.buf[:4]
	var saved bytes.Buffer
	if _, err := state.Save(context.Background(), &saved, n); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if string(n.buf) != "abcdefgh" {
		t.Errorf("the save changed buf to %q", n.buf)
	}
	var c cloneNode
	if _, err := state.Load(context.Background(), &saved, &c); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(c.buf) != "abcdefgh" || cap(c.buf) != 16 {
		t.Errorf("loaded buf is %q of capacity %d", c.buf, cap(c.buf))
	}
	if string(c.view) != "abcd" || cap(c.view) != 16 || &c.view[0] != &c.buf[0] {
		t.Errorf("loaded view %q of capacity %d does not share buf's array", c.view, cap(c.view))
	}
}

// TestRecordingCopies checks that copies of a recording, made at once, are
// each a copy of the source as it was recorded, of its own.
func TestRecordingCopies(t *testing.T) {
	g := newCloneAliases()
	rec, err := state.Record(context.Background(), g.root, state.SaveOptions{})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	// What changes after the recording is no copy's.
	g.a.val, g.b.val, g.root.arr[1].val = 10, 20, 30
	copy(g.a.buf, "ABCDEFGH")
	g.a.m["c"] = g.a
	g.b.any = cloneValue{x: 70}

	copies := make([]cloneRoot, 4)
	errs := make([]error, len(copies))
	var wg sync.WaitGroup
	for i := range copies {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = rec.Copy(context.Background(), &copies[i])
		}()
	}
	wg.Wait()
	for i := range copies {
		if errs[i] != nil {
			t.Fatalf("Copy %d: %v", i, errs[i])
		}
		g.check(t, &copies[i])
	}
	c0, c1 := &copies[0], &copies[1]
	if c0.a == c1.a || &c0.a.buf[0] == &c1.a.buf[0] || c0.inArr == c1.inArr ||
		reflect.ValueOf(c0.a.m).UnsafePointer() == reflect.ValueOf(c1.a.m).UnsafePointer() {
		t.Errorf("copies share objects")
	}
	var wrong cloneValue
	if _, err := rec.Copy(context.Background(), &wrong); err == nil {
		t.Errorf("Copy of a cloneRoot into a cloneValue succeeded")
	}
}

// TestCloneBinaryTime checks that a clone copies a time.Time, which the
// encoder saves through its binary codec, as a save and a load do: the same
// instant and zone offset, without the monotonic reading, and pointers to it
// still point to it.
func TestCloneBinaryTime(t *testing.T) {
	for name, timestamp := range map[string]time.Time{
		"zero":      {},
		"zone":      time.Date(2026, time.October, 7, 12, 0, 0, 1, time.FixedZone("offset", 3661)),
		"monotonic": time.Now(),
	} {
		t.Run(name, func(t *testing.T) {
			original := timeContainer{timestamp: timestamp}
			original.pointer = &original.timestamp
			var c timeContainer
			if _, err := state.Clone(context.Background(), context.Background(), &original, &c, state.SaveOptions{}); err != nil {
				t.Fatalf("Clone: %v", err)
			}
			if !c.timestamp.Equal(timestamp) {
				t.Errorf("timestamp = %v, want %v", c.timestamp, timestamp)
			}
			_, wantOffset := timestamp.Zone()
			if _, gotOffset := c.timestamp.Zone(); gotOffset != wantOffset {
				t.Errorf("zone offset = %d, want %d", gotOffset, wantOffset)
			}
			if c.timestamp != c.timestamp.Round(0) {
				t.Errorf("cloned timestamp contains a monotonic reading: %v", c.timestamp)
			}
			if c.pointer != &c.timestamp {
				t.Errorf("pointer = %p, want alias %p", c.pointer, &c.timestamp)
			}
		})
	}
}

// TestCloneRejectsSecondRoot checks that a clone, like a save, refuses a
// graph that reaches another object of the root's type.
func TestCloneRejectsSecondRoot(t *testing.T) {
	other := &cloneRoot{}
	r := &cloneRoot{a: &cloneNode{any: other}}
	var c cloneRoot
	if _, err := state.Clone(context.Background(), context.Background(), r, &c, state.SaveOptions{RejectMultipleRootObjects: true}); err == nil {
		t.Errorf("Clone reached a second root")
	}
}

func BenchmarkClone(b *testing.B) {
	for objName, objInfo := range allObjects {
		b.Run(objName, func(b *testing.B) {
			v := objInfo.New(1024)
			b.ReportAllocs()
			for b.Loop() {
				// v points to the object.
				dst := reflect.New(reflect.TypeOf(v).Elem())
				if _, err := state.Clone(context.Background(), context.Background(), v, dst.Interface(), state.SaveOptions{}); err != nil {
					b.Errorf("clone failed: %v", err)
				}
			}
		})
	}
}

// buildRich builds a ring of n cloneRich, which refer to each other.
func buildRich(n int) any {
	nodes := make([]*cloneRich, n)
	for i := range nodes {
		nodes[i] = &cloneRich{a: int64(i), b: 2, c: 3, d: 4, e: 5, f: true, name: "node", vals: []int32{1, 2, 3}, arr: [4]uint64{1, 2, 3, 4}}
	}
	for i, n := range nodes {
		n.next = nodes[(i+1)%len(nodes)]
		n.prev = nodes[(i+len(nodes)-1)%len(nodes)]
		n.inner = cloneValue{x: i, p: nil}
		if i%16 == 0 {
			n.m = map[int]*cloneRich{i: n.next}
		}
		n.iface = n.prev
	}
	return &nodes
}

func BenchmarkRich(b *testing.B) {
	const n = 1000
	v := buildRich(n)
	b.Run("encode", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := state.Save(context.Background(), io.Discard, v); err != nil {
				b.Fatal(err)
			}
		}
	})
	var buf bytes.Buffer
	if _, err := state.Save(context.Background(), &buf, v); err != nil {
		b.Fatal(err)
	}
	b.Run("decode", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			dst := reflect.New(reflect.TypeOf(v).Elem())
			if _, err := state.Load(context.Background(), bytes.NewReader(buf.Bytes()), dst.Interface()); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("clone", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			dst := reflect.New(reflect.TypeOf(v).Elem())
			if _, err := state.Clone(context.Background(), context.Background(), v, dst.Interface(), state.SaveOptions{}); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// TestCloneTakesValuesWhenSaved checks that a clone takes what a struct's
// StateSave method saved as it was then, as the encoder does, though what
// another object's StateSave method does later changes it.
func TestCloneTakesValuesWhenSaved(t *testing.T) {
	var buf bytes.Buffer
	if _, err := state.Save(context.Background(), &buf, newCloneRegistryRoot()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	var loaded cloneRegistryRoot
	if _, err := state.Load(context.Background(), &buf, &loaded); err != nil {
		t.Fatalf("Load: %v", err)
	}
	var cloned cloneRegistryRoot
	if _, err := state.Clone(context.Background(), context.Background(), newCloneRegistryRoot(), &cloned, state.SaveOptions{}); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if !reflect.DeepEqual(loaded, cloned) {
		t.Errorf("the clone differs from the load:\n\tloaded: %#v\n\tcloned: %#v", loaded, cloned)
	}
	if len(cloned.reg.entries) != len(loaded.reg.entries) {
		t.Errorf("the clone's registry has %d entries, the load's %d", len(cloned.reg.entries), len(loaded.reg.entries))
	}
}

// TestCloneInterfacesWithMethods checks that the copies of interfaces with
// methods hold copies of what the source's hold, by pointer and by value,
// and copies of a recording too.
func TestCloneInterfacesWithMethods(t *testing.T) {
	sq := &cloneSquare{side: 3}
	sq.next = sq
	src := &cloneShapes{square: sq, circle: cloneCircle{r: 2}, sq: sq}
	check := func(c *cloneShapes) {
		t.Helper()
		square, ok := c.square.(*cloneSquare)
		if !ok || square == sq || square != c.sq || square.next != square || c.square.area() != 9 {
			t.Errorf("the square is %#v, want a copy of the source's that the clone points to", c.square)
		}
		if circle, ok := c.circle.(cloneCircle); !ok || circle.r != 2 || c.circle.area() != 12 {
			t.Errorf("the circle is %#v, want cloneCircle{r: 2}", c.circle)
		}
		if c.none != nil {
			t.Errorf("an interface that held nothing holds %#v", c.none)
		}
	}
	var c cloneShapes
	if _, err := state.Clone(context.Background(), context.Background(), src, &c, state.SaveOptions{}); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	check(&c)
	rec, err := state.Record(context.Background(), src, state.SaveOptions{})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	copies := make([]cloneShapes, 3)
	for i := range copies {
		if _, err := rec.Copy(context.Background(), &copies[i]); err != nil {
			t.Fatalf("Copy %d: %v", i, err)
		}
		check(&copies[i])
	}
	if copies[0].sq == copies[1].sq {
		t.Errorf("copies share a square")
	}
}
