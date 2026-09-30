// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcsnapshot

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
)

const (
	bTreeMaxEntries  = 31
	bTreeMaxKeyBytes = 4096
	bTreeMaxDepth    = 64
)

type bTreeNode struct {
	leaf     bool
	keys     [][]byte
	children []ObjectID
}

func validBTreeKind(kind ObjectKind) bool {
	return kind == ObjectKindInodeNode || kind == ObjectKindDirectoryNode
}

func encodeBTreeNode(node bTreeNode) ([]byte, []Edge, error) {
	if len(node.keys) == 0 || len(node.keys) != len(node.children) || len(node.keys) > bTreeMaxEntries {
		return nil, nil, errors.New("invalid TTC B-tree node cardinality")
	}
	data := make([]byte, 3)
	if node.leaf {
		data[0] = 1
	}
	binary.BigEndian.PutUint16(data[1:], uint16(len(node.keys)))
	edges := make([]Edge, len(node.children))
	for index, key := range node.keys {
		if len(key) == 0 || len(key) > bTreeMaxKeyBytes || (index != 0 && bytes.Compare(node.keys[index-1], key) >= 0) {
			return nil, nil, errors.New("invalid TTC B-tree key ordering")
		}
		var length [2]byte
		binary.BigEndian.PutUint16(length[:], uint16(len(key)))
		data = append(data, length[:]...)
		data = append(data, key...)
		edges[index] = Edge{Slot: uint16(index), Child: node.children[index]}
	}
	return data, edges, nil
}

func decodeBTreeNode(object Object, kind ObjectKind) (bTreeNode, error) {
	if object.ID.Kind != kind || !validBTreeKind(kind) || len(object.Data) < 3 || object.Data[0] > 1 {
		return bTreeNode{}, fmt.Errorf("object %s is not a TTC B-tree node of kind %d", object.ID.key(), kind)
	}
	count := int(binary.BigEndian.Uint16(object.Data[1:3]))
	if count == 0 || count > bTreeMaxEntries || len(object.Children) != count {
		return bTreeNode{}, fmt.Errorf("object %s has invalid TTC B-tree cardinality", object.ID.key())
	}
	node := bTreeNode{leaf: object.Data[0] == 1, keys: make([][]byte, 0, count), children: make([]ObjectID, count)}
	offset := 3
	for index := 0; index < count; index++ {
		if offset+2 > len(object.Data) {
			return bTreeNode{}, fmt.Errorf("object %s has truncated TTC B-tree key", object.ID.key())
		}
		length := int(binary.BigEndian.Uint16(object.Data[offset : offset+2]))
		offset += 2
		if length == 0 || length > bTreeMaxKeyBytes || offset+length > len(object.Data) {
			return bTreeNode{}, fmt.Errorf("object %s has invalid TTC B-tree key length", object.ID.key())
		}
		// A key shares the object's data, which no one changes.
		key := object.Data[offset : offset+length : offset+length]
		offset += length
		if index != 0 && bytes.Compare(node.keys[index-1], key) >= 0 {
			return bTreeNode{}, fmt.Errorf("object %s has unordered TTC B-tree keys", object.ID.key())
		}
		if object.Children[index].Slot != uint16(index) {
			return bTreeNode{}, fmt.Errorf("object %s has noncanonical TTC B-tree edge slots", object.ID.key())
		}
		node.keys = append(node.keys, key)
		node.children[index] = object.Children[index].Child
	}
	if offset != len(object.Data) {
		return bTreeNode{}, fmt.Errorf("object %s has trailing TTC B-tree metadata", object.ID.key())
	}
	return node, nil
}

// searchBTreeNode is decodeBTreeNode and then bTreePosition, for a lookup: it
// finds, in the encoded node, the entry key falls under and that entry's key,
// reading the node's keys only as far as it must, and copying none.
func searchBTreeNode(object Object, kind ObjectKind, key []byte) (leaf bool, entry []byte, child ObjectID, err error) {
	if object.ID.Kind != kind || !validBTreeKind(kind) || len(object.Data) < 3 || object.Data[0] > 1 {
		return false, nil, ObjectID{}, fmt.Errorf("object %s is not a TTC B-tree node of kind %d", object.ID.key(), kind)
	}
	count := int(binary.BigEndian.Uint16(object.Data[1:3]))
	if count == 0 || count > bTreeMaxEntries || len(object.Children) != count {
		return false, nil, ObjectID{}, fmt.Errorf("object %s has invalid TTC B-tree cardinality", object.ID.key())
	}
	index, offset := 0, 3
	var previous []byte
	for i := 0; i < count; i++ {
		if offset+2 > len(object.Data) {
			return false, nil, ObjectID{}, fmt.Errorf("object %s has truncated TTC B-tree key", object.ID.key())
		}
		length := int(binary.BigEndian.Uint16(object.Data[offset : offset+2]))
		offset += 2
		if length == 0 || length > bTreeMaxKeyBytes || offset+length > len(object.Data) {
			return false, nil, ObjectID{}, fmt.Errorf("object %s has invalid TTC B-tree key length", object.ID.key())
		}
		current := object.Data[offset : offset+length]
		offset += length
		if i != 0 && bytes.Compare(previous, current) >= 0 {
			return false, nil, ObjectID{}, fmt.Errorf("object %s has unordered TTC B-tree keys", object.ID.key())
		}
		if i == 0 {
			entry = current
		}
		if bytes.Compare(current, key) > 0 {
			break
		}
		index, entry, previous = i, current, current
	}
	if object.Children[index].Slot != uint16(index) {
		return false, nil, ObjectID{}, fmt.Errorf("object %s has noncanonical TTC B-tree edge slots", object.ID.key())
	}
	return object.Data[0] == 1, entry, object.Children[index].Child, nil
}

func bTreePosition(keys [][]byte, key []byte) int {
	index := sort.Search(len(keys), func(i int) bool { return bytes.Compare(keys[i], key) > 0 }) - 1
	if index < 0 {
		return 0
	}
	return index
}

// BTreeLookup resolves key directly through immutable nodes. Its work depends
// on tree height, never snapshot or branch ancestry depth.
func (s *Store) BTreeLookup(root ObjectID, kind ObjectKind, key []byte) (ObjectID, bool, error) {
	if !validBTreeKind(kind) || len(key) == 0 || len(key) > bTreeMaxKeyBytes {
		return ObjectID{}, false, errors.New("invalid TTC B-tree lookup")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return bTreeLookupObjects(root, kind, key, func(id ObjectID) (Object, bool) {
		object, ok := s.state.Objects[id]
		return object, ok
	})
}

type graphMutation struct {
	store    *Store
	snapshot Snapshot
	objects  map[ObjectID]Object
	// lent holds, for each object of the graph that a copy the mutation made
	// of it replaces, the indexes of its edges whose references the copy took
	// rather than retaining their children anew, in ascending order. See
	// bTreeBuilder.lends and collectFilesystemMutationGarbage.
	lent map[ObjectID][]int
	err  error
}

func newGraphMutation(store *Store, origin SnapshotID) (*graphMutation, error) {
	snapshot, ok := store.state.Snapshots[origin]
	if !ok || snapshot.State != SnapshotWorking {
		return nil, fmt.Errorf("snapshot %d is not working", origin)
	}
	return &graphMutation{store: store, snapshot: snapshot, objects: make(map[ObjectID]Object)}, nil
}

func (mutation *graphMutation) object(id ObjectID) (Object, bool) {
	if object, ok := mutation.objects[id]; ok {
		return object, true
	}
	object, ok, nextOrdinal, err := mutation.store.resolveDeferredLockedAfter(id, mutation.snapshot.NextOrdinal)
	if err != nil {
		mutation.err = errors.Join(mutation.err, err)
		return Object{}, false
	}
	if mutation.snapshot.NextOrdinal < nextOrdinal {
		mutation.snapshot.NextOrdinal = nextOrdinal
	}
	return object, ok
}

func (mutation *graphMutation) rawObject(id ObjectID) (Object, bool) {
	if object, ok := mutation.objects[id]; ok {
		return object, true
	}
	object, ok := mutation.store.state.Objects[id]
	return object, ok
}

func (mutation *graphMutation) put(object Object) {
	mutation.objects[object.ID] = object
}

func (mutation *graphMutation) retain(id ObjectID) error {
	object, ok := mutation.rawObject(id)
	if !ok || object.State != ObjectLive || object.Refs == math.MaxUint64 {
		return errors.Join(mutation.err, fmt.Errorf("object %s cannot be retained (found=%t state=%q refs=%d deferred=%t)", id.key(), ok, object.State, object.Refs, object.Deferred))
	}
	object.Refs++
	mutation.put(object)
	return nil
}

// create adds an object of kind, with data and children, which it takes: the
// caller does not change them after.
func (mutation *graphMutation) create(kind ObjectKind, data []byte, children []Edge) (ObjectID, error) {
	return mutation.createFrom(kind, data, children, ObjectID{}, nil)
}

// createFrom is create for a copy of lender, unless lender is zero: each
// edge whose from is not negative takes the reference of lender's edge of
// that index to the same child, rather than retaining it anew.
func (mutation *graphMutation) createFrom(kind ObjectKind, data []byte, children []Edge, lender ObjectID, from []int) (ObjectID, error) {
	if mutation.snapshot.NextOrdinal == 0 || mutation.snapshot.NextOrdinal == math.MaxUint64 {
		return ObjectID{}, fmt.Errorf("snapshot %d object ordinal space exhausted", mutation.snapshot.ID)
	}
	id := ObjectID{Snapshot: mutation.snapshot.ID, Ordinal: mutation.snapshot.NextOrdinal, Kind: kind}
	mutation.snapshot.NextOrdinal++
	object := Object{ID: id, State: ObjectLive, Data: data, Children: children}
	if err := validateObject(object); err != nil {
		return ObjectID{}, err
	}
	for index, edge := range object.Children {
		if !lender.IsZero() && from[index] >= 0 {
			if mutation.lent == nil {
				mutation.lent = make(map[ObjectID][]int)
			}
			lent := mutation.lent[lender]
			if len(lent) != 0 && lent[len(lent)-1] >= from[index] {
				return ObjectID{}, fmt.Errorf("object %s lends its edge %d out of order", lender, from[index])
			}
			mutation.lent[lender] = append(lent, from[index])
			continue
		}
		if err := mutation.retain(edge.Child); err != nil {
			return ObjectID{}, err
		}
	}
	mutation.put(object)
	return id, nil
}

// putReserved adds object under an ID reserved in the mutation's snapshot with
// ReserveObjects, and retains its children.
func (mutation *graphMutation) putReserved(object Object) error {
	id := object.ID
	if id.Snapshot != mutation.snapshot.ID || id.Ordinal >= mutation.snapshot.NextOrdinal {
		return fmt.Errorf("object ID %s is not reserved in snapshot %d", id.key(), mutation.snapshot.ID)
	}
	if _, exists := mutation.rawObject(id); exists {
		return fmt.Errorf("object %s already exists", id.key())
	}
	if err := validateObject(object); err != nil {
		return err
	}
	for _, edge := range object.Children {
		if err := mutation.retain(edge.Child); err != nil {
			return err
		}
	}
	mutation.put(object)
	return nil
}

func (mutation *graphMutation) setRoot(name string, id ObjectID) error {
	return mutation.setRoots(map[string]ObjectID{name: id})
}

// setRoots points each named root at its new tree. Every new tree is retained
// before any old one is released, so trees that share nodes can be replaced
// in one transaction.
func (mutation *graphMutation) setRoots(roots map[string]ObjectID) error {
	names := sortedRootNames(roots)
	for _, name := range names {
		if err := mutation.retain(roots[name]); err != nil {
			return err
		}
	}
	for _, name := range names {
		oldID, ok := mutation.store.state.Roots[name]
		if !ok {
			continue
		}
		oldObject, exists := mutation.rawObject(oldID)
		if !exists {
			return fmt.Errorf("root %q references missing object %s", name, oldID.key())
		}
		old, err := releaseObject(oldObject)
		if err != nil {
			return err
		}
		mutation.put(old)
	}
	// A multi-operation transaction may path-copy the same tree more than
	// once. Intermediate immutable nodes were never published as roots; queue
	// them for ordinary cascading GC in the same atomic commit.
	for key, object := range mutation.objects {
		if object.Refs == 0 {
			object.State = ObjectReclaiming
			mutation.objects[key] = object
		}
	}
	return nil
}

func (mutation *graphMutation) commit(rootName string, root ObjectID) error {
	return mutation.commitRoots(map[string]ObjectID{rootName: root})
}

// commitRoots writes the mutation and the new trees of roots, already set with
// setRoots, as one transaction.
func (mutation *graphMutation) commitRoots(roots map[string]ObjectID) error {
	ids := sortedIDs(mutation.objects)
	ops := make([]journalOp, 0, len(ids)+2)
	references := make(map[ObjectID]Object)
	for _, id := range ids {
		object := mutation.objects[id]
		if previous, ok := mutation.store.state.Objects[id]; ok && previous.ID == object.ID && previous.BulkLength == object.BulkLength && bytes.Equal(previous.Data, object.Data) && slices.Equal(previous.Children, object.Children) {
			if previous.Refs != object.Refs || previous.State != object.State {
				references[id] = object
			}
			continue
		}
		ops = append(ops, journalOp{PutObject: &object})
	}
	ops = append(ops, journalOp{PutSnapshot: &mutation.snapshot})
	for _, name := range sortedRootNames(roots) {
		root := roots[name]
		ops = append(ops, journalOp{RootName: name, PutRoot: &root})
	}
	return mutation.store.commitLockedWithMaterializedReferences(ops, references)
}

func sortedRootNames(roots map[string]ObjectID) []string {
	names := make([]string, 0, len(roots))
	for name := range roots {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

type bTreeBuilder struct {
	mutation *graphMutation
	kind     ObjectKind
	// final is whether the nodes apply makes are the last copies the
	// transaction makes of their tree: none is copied again before it
	// commits. See lends.
	final bool
}

type bTreeEdit struct {
	key    []byte
	value  ObjectID
	delete bool
}

func (builder *bTreeBuilder) makeNode(node bTreeNode) (ObjectID, error) {
	return builder.makeNodeFrom(node, ObjectID{}, nil)
}

// makeNodeFrom is makeNode for a copy of lender: see createFrom.
func (builder *bTreeBuilder) makeNodeFrom(node bTreeNode, lender ObjectID, from []int) (ObjectID, error) {
	data, edges, err := encodeBTreeNode(node)
	if err != nil {
		return ObjectID{}, err
	}
	return builder.mutation.createFrom(builder.kind, data, edges, lender, from)
}

// lends reports whether the copies apply makes of current, whose object is
// object, take its references to the children they keep. They do if they
// are the transaction's last copies of their tree and current is the
// graph's, referenced by nothing but the node it was reached from: current
// then goes as that node goes, and its copies keep what it referenced
// without each reference being counted up by retaining it anew, then down
// by collecting current. A current that stays after all keeps its
// references, and its copies take their own: see
// collectFilesystemMutationGarbage. Counting those references up and down
// took almost half of what creating files 128 a transaction took, and of
// what replaying the journal of their creation took.
func (builder *bTreeBuilder) lends(current ObjectID, object Object) bool {
	if !builder.final || object.Refs != 1 {
		return false
	}
	_, committed := builder.mutation.store.state.Objects[current]
	return committed
}

// apply path-copies every node touched by a sorted set of edits exactly once.
// Calling set/delete repeatedly in one logical filesystem transaction creates
// a complete obsolete path for every operation; package managers routinely
// turn that into tens of thousands of short-lived metadata objects. Batched
// descent instead partitions edits at each bounded-fanout node and shares all
// unaffected children.
func (builder *bTreeBuilder) apply(root ObjectID, edits []bTreeEdit) (ObjectID, error) {
	if len(edits) == 0 {
		return root, nil
	}
	sort.Slice(edits, func(i, j int) bool { return bytes.Compare(edits[i].key, edits[j].key) < 0 })
	for index := range edits {
		if len(edits[index].key) == 0 || len(edits[index].key) > bTreeMaxKeyBytes {
			return ObjectID{}, errors.New("invalid TTC B-tree batch key")
		}
		if index != 0 && bytes.Equal(edits[index-1].key, edits[index].key) {
			return ObjectID{}, errors.New("duplicate TTC B-tree batch key")
		}
		if !edits[index].delete && edits[index].value.IsZero() {
			return ObjectID{}, errors.New("TTC B-tree batch put has zero value")
		}
	}
	if root.IsZero() {
		var keys [][]byte
		var children []ObjectID
		for _, edit := range edits {
			if edit.delete {
				continue
			}
			keys = append(keys, append([]byte(nil), edit.key...))
			children = append(children, edit.value)
		}
		return builder.buildTree(true, keys, children)
	}
	ids, minima, changed, err := builder.applyNode(root, edits)
	if err != nil || !changed {
		return root, err
	}
	return builder.buildUpper(ids, minima)
}

func (builder *bTreeBuilder) buildTree(leaf bool, keys [][]byte, children []ObjectID) (ObjectID, error) {
	if len(keys) == 0 {
		return ObjectID{}, nil
	}
	ids, minima, err := builder.makeLevel(leaf, keys, children, ObjectID{}, nil)
	if err != nil {
		return ObjectID{}, err
	}
	return builder.buildUpper(ids, minima)
}

func (builder *bTreeBuilder) buildUpper(ids []ObjectID, minima [][]byte) (ObjectID, error) {
	for len(ids) > 1 {
		var err error
		ids, minima, err = builder.makeLevel(false, minima, ids, ObjectID{}, nil)
		if err != nil {
			return ObjectID{}, err
		}
	}
	if len(ids) == 0 {
		return ObjectID{}, nil
	}
	return ids[0], nil
}

// makeLevel makes the nodes that hold keys and children in order. They are
// copies of lender, unless it is zero, and each child's from is the index of
// lender's edge to it, or -1: see createFrom.
func (builder *bTreeBuilder) makeLevel(leaf bool, keys [][]byte, children []ObjectID, lender ObjectID, from []int) ([]ObjectID, [][]byte, error) {
	if len(keys) != len(children) || (!lender.IsZero() && len(from) != len(children)) {
		return nil, nil, errors.New("invalid TTC B-tree batch level")
	}
	ids := make([]ObjectID, 0, (len(keys)+bTreeMaxEntries-1)/bTreeMaxEntries)
	minima := make([][]byte, 0, cap(ids))
	for start := 0; start < len(keys); start += bTreeMaxEntries {
		end := min(start+bTreeMaxEntries, len(keys))
		var kept []int
		if !lender.IsZero() {
			kept = from[start:end]
		}
		id, err := builder.makeNodeFrom(bTreeNode{leaf: leaf, keys: keys[start:end], children: children[start:end]}, lender, kept)
		if err != nil {
			return nil, nil, err
		}
		ids = append(ids, id)
		minima = append(minima, append([]byte(nil), keys[start]...))
	}
	return ids, minima, nil
}

func (builder *bTreeBuilder) applyNode(current ObjectID, edits []bTreeEdit) ([]ObjectID, [][]byte, bool, error) {
	object, ok := builder.mutation.object(current)
	if !ok {
		return nil, nil, false, fmt.Errorf("TTC B-tree node %s does not exist", current.key())
	}
	node, err := decodeBTreeNode(object, builder.kind)
	if err != nil {
		return nil, nil, false, err
	}
	// from holds, for each child of the node's copies, the index of the
	// node's edge to it, or -1 for a child the node did not have.
	var lender ObjectID
	if builder.lends(current, object) {
		lender = current
	}
	if node.leaf {
		keys := make([][]byte, 0, len(node.keys)+len(edits))
		children := make([]ObjectID, 0, len(node.children)+len(edits))
		from := make([]int, 0, cap(children))
		changed := false
		oldIndex, editIndex := 0, 0
		for oldIndex < len(node.keys) || editIndex < len(edits) {
			if editIndex == len(edits) || (oldIndex < len(node.keys) && bytes.Compare(node.keys[oldIndex], edits[editIndex].key) < 0) {
				keys = append(keys, node.keys[oldIndex])
				children = append(children, node.children[oldIndex])
				from = append(from, oldIndex)
				oldIndex++
				continue
			}
			if oldIndex == len(node.keys) || bytes.Compare(edits[editIndex].key, node.keys[oldIndex]) < 0 {
				if !edits[editIndex].delete {
					keys = append(keys, append([]byte(nil), edits[editIndex].key...))
					children = append(children, edits[editIndex].value)
					from = append(from, -1)
					changed = true
				}
				editIndex++
				continue
			}
			// Equal keys replace or remove the existing value.
			if edits[editIndex].delete {
				changed = true
			} else if value := edits[editIndex].value; value == node.children[oldIndex] {
				keys = append(keys, node.keys[oldIndex])
				children = append(children, value)
				from = append(from, oldIndex)
			} else {
				keys = append(keys, node.keys[oldIndex])
				children = append(children, value)
				from = append(from, -1)
				changed = true
			}
			oldIndex++
			editIndex++
		}
		if !changed {
			return []ObjectID{current}, [][]byte{append([]byte(nil), node.keys[0]...)}, false, nil
		}
		ids, minima, err := builder.makeLevel(true, keys, children, lender, from)
		return ids, minima, true, err
	}

	grouped := make([][]bTreeEdit, len(node.children))
	for _, edit := range edits {
		index := bTreePosition(node.keys, edit.key)
		grouped[index] = append(grouped[index], edit)
	}
	keys := make([][]byte, 0, len(node.keys)+len(edits))
	children := make([]ObjectID, 0, len(node.children)+len(edits))
	from := make([]int, 0, cap(children))
	changed := false
	for index, child := range node.children {
		if len(grouped[index]) == 0 {
			keys = append(keys, node.keys[index])
			children = append(children, child)
			from = append(from, index)
			continue
		}
		ids, minima, childChanged, err := builder.applyNode(child, grouped[index])
		if err != nil {
			return nil, nil, false, err
		}
		changed = changed || childChanged
		keys = append(keys, minima...)
		children = append(children, ids...)
		// An unchanged child is the node's; a changed one is replaced by its
		// copies, or by its one remaining child, which is not the node's.
		for range ids {
			if childChanged {
				from = append(from, -1)
			} else {
				from = append(from, index)
			}
		}
	}
	if !changed {
		return []ObjectID{current}, [][]byte{append([]byte(nil), node.keys[0]...)}, false, nil
	}
	if len(children) == 1 {
		childObject, ok := builder.mutation.object(children[0])
		if !ok {
			return nil, nil, false, fmt.Errorf("TTC B-tree child %s does not exist", children[0].key())
		}
		childNode, err := decodeBTreeNode(childObject, builder.kind)
		if err != nil {
			return nil, nil, false, err
		}
		return []ObjectID{children[0]}, [][]byte{append([]byte(nil), childNode.keys[0]...)}, true, nil
	}
	ids, minima, err := builder.makeLevel(false, keys, children, lender, from)
	return ids, minima, true, err
}

func splitBTreeNode(node bTreeNode) (bTreeNode, bTreeNode) {
	middle := len(node.keys) / 2
	return bTreeNode{leaf: node.leaf, keys: node.keys[:middle], children: node.children[:middle]},
		bTreeNode{leaf: node.leaf, keys: node.keys[middle:], children: node.children[middle:]}
}

func insertKey(keys [][]byte, children []ObjectID, index int, key []byte, child ObjectID) ([][]byte, []ObjectID) {
	keys = append(keys, nil)
	copy(keys[index+1:], keys[index:])
	keys[index] = append([]byte(nil), key...)
	children = append(children, ObjectID{})
	copy(children[index+1:], children[index:])
	children[index] = child
	return keys, children
}

func (builder *bTreeBuilder) insert(current ObjectID, key []byte, value ObjectID) ([]ObjectID, [][]byte, error) {
	object, ok := builder.mutation.object(current)
	if !ok {
		return nil, nil, fmt.Errorf("TTC B-tree node %s does not exist", current.key())
	}
	node, err := decodeBTreeNode(object, builder.kind)
	if err != nil {
		return nil, nil, err
	}
	if node.leaf {
		index := sort.Search(len(node.keys), func(i int) bool { return bytes.Compare(node.keys[i], key) >= 0 })
		if index < len(node.keys) && bytes.Equal(node.keys[index], key) {
			node.children[index] = value
		} else {
			node.keys, node.children = insertKey(node.keys, node.children, index, key, value)
		}
	} else {
		index := bTreePosition(node.keys, key)
		ids, minima, err := builder.insert(node.children[index], key, value)
		if err != nil {
			return nil, nil, err
		}
		node.keys = append(node.keys[:index], node.keys[index+1:]...)
		node.children = append(node.children[:index], node.children[index+1:]...)
		for offset := range ids {
			node.keys, node.children = insertKey(node.keys, node.children, index+offset, minima[offset], ids[offset])
		}
	}
	if len(node.keys) <= bTreeMaxEntries {
		id, err := builder.makeNode(node)
		return []ObjectID{id}, [][]byte{append([]byte(nil), node.keys[0]...)}, err
	}
	left, right := splitBTreeNode(node)
	leftID, err := builder.makeNode(left)
	if err != nil {
		return nil, nil, err
	}
	rightID, err := builder.makeNode(right)
	if err != nil {
		return nil, nil, err
	}
	return []ObjectID{leftID, rightID}, [][]byte{append([]byte(nil), left.keys[0]...), append([]byte(nil), right.keys[0]...)}, nil
}

func (builder *bTreeBuilder) remove(current ObjectID, key []byte) (ObjectID, []byte, bool, error) {
	object, ok := builder.mutation.object(current)
	if !ok {
		return ObjectID{}, nil, false, fmt.Errorf("TTC B-tree node %s does not exist", current.key())
	}
	node, err := decodeBTreeNode(object, builder.kind)
	if err != nil {
		return ObjectID{}, nil, false, err
	}
	if node.leaf {
		index := sort.Search(len(node.keys), func(i int) bool { return bytes.Compare(node.keys[i], key) >= 0 })
		if index == len(node.keys) || !bytes.Equal(node.keys[index], key) {
			return current, append([]byte(nil), node.keys[0]...), false, nil
		}
		node.keys = append(node.keys[:index], node.keys[index+1:]...)
		node.children = append(node.children[:index], node.children[index+1:]...)
	} else {
		index := bTreePosition(node.keys, key)
		child, minimum, changed, err := builder.remove(node.children[index], key)
		if err != nil || !changed {
			return current, append([]byte(nil), node.keys[0]...), changed, err
		}
		if child.IsZero() {
			node.keys = append(node.keys[:index], node.keys[index+1:]...)
			node.children = append(node.children[:index], node.children[index+1:]...)
		} else {
			node.keys[index] = minimum
			node.children[index] = child
		}
	}
	if len(node.keys) == 0 {
		return ObjectID{}, nil, true, nil
	}
	if !node.leaf && len(node.keys) == 1 {
		childObject, ok := builder.mutation.object(node.children[0])
		if !ok {
			return ObjectID{}, nil, false, fmt.Errorf("TTC B-tree child %s does not exist", node.children[0].key())
		}
		childNode, err := decodeBTreeNode(childObject, builder.kind)
		if err != nil {
			return ObjectID{}, nil, false, err
		}
		return node.children[0], append([]byte(nil), childNode.keys[0]...), true, nil
	}
	id, err := builder.makeNode(node)
	return id, append([]byte(nil), node.keys[0]...), true, err
}

// BTreeSet path-copies a bounded-fanout inode or directory tree and atomically
// swaps rootName. value is an inode object for an inode tree, or an inode
// object referenced by a directory entry tree.
func (s *Store) BTreeSet(origin SnapshotID, rootName string, kind ObjectKind, key []byte, value ObjectID) (ObjectID, error) {
	if err := validateRootName(rootName); err != nil {
		return ObjectID{}, err
	}
	if !validBTreeKind(kind) || len(key) == 0 || len(key) > bTreeMaxKeyBytes {
		return ObjectID{}, errors.New("invalid TTC B-tree update")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if object, ok := s.state.Objects[value]; !ok || object.State != ObjectLive {
		return ObjectID{}, fmt.Errorf("b-tree value %s is not live", value.key())
	}
	mutation, err := newGraphMutation(s, origin)
	if err != nil {
		return ObjectID{}, err
	}
	builder := bTreeBuilder{mutation: mutation, kind: kind}
	var root ObjectID
	if oldRoot, ok := s.state.Roots[rootName]; ok {
		ids, minima, err := builder.insert(oldRoot, key, value)
		if err != nil {
			return ObjectID{}, err
		}
		if len(ids) == 1 {
			root = ids[0]
		} else {
			root, err = builder.makeNode(bTreeNode{keys: minima, children: ids})
			if err != nil {
				return ObjectID{}, err
			}
		}
	} else {
		root, err = builder.makeNode(bTreeNode{leaf: true, keys: [][]byte{append([]byte(nil), key...)}, children: []ObjectID{value}})
		if err != nil {
			return ObjectID{}, err
		}
	}
	if err := mutation.setRoot(rootName, root); err != nil {
		return ObjectID{}, err
	}
	if err := mutation.commit(rootName, root); err != nil {
		return ObjectID{}, err
	}
	return root, nil
}

// BTreeDelete path-copies and removes key. It reports false without changing
// the graph if key is absent. An empty result removes rootName.
func (s *Store) BTreeDelete(origin SnapshotID, rootName string, kind ObjectKind, key []byte) (bool, error) {
	if err := validateRootName(rootName); err != nil {
		return false, err
	}
	if !validBTreeKind(kind) || len(key) == 0 || len(key) > bTreeMaxKeyBytes {
		return false, errors.New("invalid TTC B-tree deletion")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	oldRoot, ok := s.state.Roots[rootName]
	if !ok {
		return false, nil
	}
	mutation, err := newGraphMutation(s, origin)
	if err != nil {
		return false, err
	}
	builder := bTreeBuilder{mutation: mutation, kind: kind}
	root, _, changed, err := builder.remove(oldRoot, key)
	if err != nil || !changed {
		return changed, err
	}
	if root.IsZero() {
		oldObject, exists := mutation.object(oldRoot)
		if !exists {
			return false, fmt.Errorf("b-tree root %s is missing", oldRoot.key())
		}
		old, err := releaseObject(oldObject)
		if err != nil {
			return false, err
		}
		mutation.put(old)
		ids := sortedIDs(mutation.objects)
		ops := make([]journalOp, 0, len(ids)+2)
		for _, id := range ids {
			object := mutation.objects[id]
			ops = append(ops, journalOp{PutObject: &object})
		}
		ops = append(ops, journalOp{PutSnapshot: &mutation.snapshot}, journalOp{RootName: rootName, DeleteRoot: true})
		return true, s.commitLocked(ops)
	}
	if err := mutation.setRoot(rootName, root); err != nil {
		return false, err
	}
	return true, mutation.commit(rootName, root)
}

// InodeKey returns the canonical big-endian key used by inode trees.
func InodeKey(inode uint64) []byte {
	var key [8]byte
	binary.BigEndian.PutUint64(key[:], inode)
	return key[:]
}
