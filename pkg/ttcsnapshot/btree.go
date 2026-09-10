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

package ttcsnapshot

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
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
		key := append([]byte(nil), object.Data[offset:offset+length]...)
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
	current := root
	for depth := 0; depth < bTreeMaxDepth; depth++ {
		object, ok := s.state.Objects[current.key()]
		if !ok {
			return ObjectID{}, false, fmt.Errorf("TTC B-tree node %s does not exist", current.key())
		}
		node, err := decodeBTreeNode(object, kind)
		if err != nil {
			return ObjectID{}, false, err
		}
		index := bTreePosition(node.keys, key)
		if node.leaf {
			if !bytes.Equal(node.keys[index], key) {
				return ObjectID{}, false, nil
			}
			return node.children[index], true, nil
		}
		current = node.children[index]
	}
	return ObjectID{}, false, errors.New("TTC B-tree exceeds maximum structural depth")
}

type graphMutation struct {
	store    *Store
	snapshot Snapshot
	objects  map[string]Object
}

func newGraphMutation(store *Store, origin SnapshotID) (*graphMutation, error) {
	snapshot, ok := store.state.Snapshots[snapshotKey(origin)]
	if !ok || snapshot.State != SnapshotWorking {
		return nil, fmt.Errorf("snapshot %d is not working", origin)
	}
	return &graphMutation{store: store, snapshot: snapshot, objects: make(map[string]Object)}, nil
}

func (mutation *graphMutation) object(id ObjectID) (Object, bool) {
	if object, ok := mutation.objects[id.key()]; ok {
		return object, true
	}
	object, ok := mutation.store.state.Objects[id.key()]
	return object, ok
}

func (mutation *graphMutation) put(object Object) {
	mutation.objects[object.ID.key()] = object
}

func (mutation *graphMutation) retain(id ObjectID) error {
	object, ok := mutation.object(id)
	if !ok || object.State != ObjectLive || object.Refs == math.MaxUint64 {
		return fmt.Errorf("object %s cannot be retained", id.key())
	}
	object.Refs++
	mutation.put(object)
	return nil
}

func (mutation *graphMutation) create(kind ObjectKind, data []byte, children []Edge) (ObjectID, error) {
	if mutation.snapshot.NextOrdinal == 0 || mutation.snapshot.NextOrdinal == math.MaxUint64 {
		return ObjectID{}, fmt.Errorf("snapshot %d object ordinal space exhausted", mutation.snapshot.ID)
	}
	id := ObjectID{Snapshot: mutation.snapshot.ID, Ordinal: mutation.snapshot.NextOrdinal, Kind: kind}
	mutation.snapshot.NextOrdinal++
	object := Object{ID: id, State: ObjectLive, Data: append([]byte(nil), data...), Children: append([]Edge(nil), children...)}
	if err := validateObject(object); err != nil {
		return ObjectID{}, err
	}
	for _, edge := range object.Children {
		if err := mutation.retain(edge.Child); err != nil {
			return ObjectID{}, err
		}
	}
	mutation.put(object)
	return id, nil
}

func (mutation *graphMutation) setRoot(name string, id ObjectID) error {
	if err := mutation.retain(id); err != nil {
		return err
	}
	if oldID, ok := mutation.store.state.Roots[name]; ok {
		oldObject, exists := mutation.object(oldID)
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
	keys := make([]string, 0, len(mutation.objects))
	for key := range mutation.objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	ops := make([]journalOp, 0, len(keys)+2)
	for _, key := range keys {
		object := mutation.objects[key]
		ops = append(ops, journalOp{PutObject: &object})
	}
	ops = append(ops, journalOp{PutSnapshot: &mutation.snapshot}, journalOp{RootName: rootName, PutRoot: &root})
	return mutation.store.commitLocked(ops)
}

type bTreeBuilder struct {
	mutation *graphMutation
	kind     ObjectKind
}

func (builder *bTreeBuilder) makeNode(node bTreeNode) (ObjectID, error) {
	data, edges, err := encodeBTreeNode(node)
	if err != nil {
		return ObjectID{}, err
	}
	return builder.mutation.create(builder.kind, data, edges)
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
	if object, ok := s.state.Objects[value.key()]; !ok || object.State != ObjectLive {
		return ObjectID{}, fmt.Errorf("B-tree value %s is not live", value.key())
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
			return false, fmt.Errorf("B-tree root %s is missing", oldRoot.key())
		}
		old, err := releaseObject(oldObject)
		if err != nil {
			return false, err
		}
		mutation.put(old)
		keys := make([]string, 0, len(mutation.objects))
		for key := range mutation.objects {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		ops := make([]journalOp, 0, len(keys)+2)
		for _, key := range keys {
			object := mutation.objects[key]
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
