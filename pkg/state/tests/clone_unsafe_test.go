// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"testing"
	"unsafe"

	"gvisor.dev/gvisor/pkg/state"
)

// TestCloneAliases checks that a clone keeps what points where: cycles,
// pointers into other objects, slices that share arrays, shared maps, and
// interfaces, all within the clone.
func TestCloneAliases(t *testing.T) {
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

	var c cloneRoot
	if _, err := state.Clone(context.Background(), context.Background(), r, &c, state.SaveOptions{}); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if c.a == a || c.b == b || c.a == nil || c.b == nil {
		t.Fatalf("the clone's nodes are the source's, or nil: %p %p", c.a, c.b)
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
	if c.a.m["b"] != c.b {
		t.Errorf("the map holds %p, want %p", c.a.m["b"], c.b)
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
	if c.a.waitFor == leaf || c.a.waitFor != c.b.waitFor || c.a.waitFor.loaded != 1 {
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
			b.StopTimer()
			n, v := buildObjects(b.N, objInfo.New)
			b.ReportAllocs()
			b.StartTimer()
			for i := 0; i < n; i++ {
				// v points to the object.
				dst := reflect.New(reflect.TypeOf(v).Elem())
				if _, err := state.Clone(context.Background(), context.Background(), v, dst.Interface(), state.SaveOptions{}); err != nil {
					b.Errorf("clone failed: %v", err)
				}
			}
			b.StopTimer()
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
