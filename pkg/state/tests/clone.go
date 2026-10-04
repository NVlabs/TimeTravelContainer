// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import "context"

// cloneNode is a node of a graph whose aliases a clone keeps.
//
// +stateify savable
type cloneNode struct {
	next *cloneNode
	val  int
	// peer points into another node.
	peer *int
	buf  []byte
	// view shares buf's array.
	view []byte
	m    map[string]*cloneNode
	// any holds a node, or a value.
	any any
	// loaded counts the node's afterLoad calls.
	loaded int `state:"nosave"`
	// waited is the val of the node that next was when afterLoad ran.
	waited int `state:"nosave"`
	// waitFor must be complete before the node's afterLoad.
	waitFor *cloneLeaf `state:"wait"`
}

func (n *cloneNode) afterLoad(context.Context) {
	n.loaded++
	if n.waitFor != nil {
		n.waited = n.waitFor.loaded
	}
}

// cloneLeaf is what a cloneNode waits for.
//
// +stateify savable
type cloneLeaf struct {
	loaded int `state:"nosave"`
}

func (l *cloneLeaf) afterLoad(context.Context) {
	l.loaded++
}

// cloneRoot holds a graph of cloneNodes.
//
// +stateify savable
type cloneRoot struct {
	a, b  *cloneNode
	arr   [2]cloneNode
	inArr *cloneNode
	inner cloneValue
}

// cloneValue is a struct held by value.
//
// +stateify savable
type cloneValue struct {
	x int
	p *cloneNode
}

// cloneRich is a struct of many fields, as a kernel's objects are.
//
// +stateify savable
type cloneRich struct {
	a, b, c int64
	d, e    uint32
	f       bool
	name    string
	next    *cloneRich
	prev    *cloneRich
	vals    []int32
	inner   cloneValue
	arr     [4]uint64
	m       map[int]*cloneRich
	iface   any
}

// cloneRegistry is what cloneRegistrants register with as they are saved, as
// the endpoints of a network stack do.
//
// +stateify savable
type cloneRegistry struct {
	entries []*cloneRegistrant
}

// cloneRegistrant registers with its registry as it is saved.
//
// +stateify savable
type cloneRegistrant struct {
	reg *cloneRegistry
	id  int
}

func (r *cloneRegistrant) beforeSave() {
	r.reg.entries = append(r.reg.entries, r)
}

// cloneRegistryRoot holds a registry, before the registrants.
//
// +stateify savable
type cloneRegistryRoot struct {
	reg         *cloneRegistry
	registrants []*cloneRegistrant
}

func newCloneRegistryRoot() *cloneRegistryRoot {
	// A full slice: the registrants' appends move its array.
	reg := &cloneRegistry{entries: make([]*cloneRegistrant, 1)}
	reg.entries[0] = &cloneRegistrant{reg: reg, id: 100}
	root := &cloneRegistryRoot{reg: reg}
	for i := 0; i < 3; i++ {
		root.registrants = append(root.registrants, &cloneRegistrant{reg: reg, id: i})
	}
	return root
}

// cloneShape is an interface with methods.
type cloneShape interface {
	area() int
}

// cloneSquare is a shape an interface holds by pointer.
//
// +stateify savable
type cloneSquare struct {
	side int
	next *cloneSquare
}

func (s *cloneSquare) area() int { return s.side * s.side }

// cloneCircle is a shape an interface holds by value.
//
// +stateify savable
type cloneCircle struct {
	r int
}

func (c cloneCircle) area() int { return 3 * c.r * c.r }

// cloneShapes holds shapes in interfaces with methods.
//
// +stateify savable
type cloneShapes struct {
	square, circle, none cloneShape
	sq                   *cloneSquare
}
