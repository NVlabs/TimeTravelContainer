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
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

const filesystemRootMagic = "TTCFS1"

// InodeRecord is immutable guest inode metadata. File is set only for regular
// files and identifies the whole-file data object.
type InodeRecord struct {
	Number      uint64
	Type        string
	Mode        uint32
	UID         uint32
	GID         uint32
	Nlink       uint32
	Size        int64
	File        ObjectID
	Directory   ObjectID
	Target      string
	DeviceMajor uint32
	DeviceMinor uint32
	AtimeNsec   int64
	BtimeNsec   int64
	CtimeNsec   int64
	MtimeNsec   int64
	Xattrs      map[string][]byte
}

// DirectoryEntryRecord maps one name in Parent to a stable guest inode.
type DirectoryEntryRecord struct {
	Parent uint64
	Name   string
	Inode  uint64
}

// FilesystemOperation is one member of an atomic filesystem-root mutation.
// Exactly one pointer/delete selector must be set.
type FilesystemOperation struct {
	PutInode    *InodeRecord
	DeleteInode uint64
	PutEntry    *DirectoryEntryRecord
	DeleteEntry *DirectoryEntryRecord
}

type filesystemRoot struct {
	inodes ObjectID
}

func decodeFilesystemRoot(object Object) (filesystemRoot, error) {
	if object.ID.Kind != ObjectKindMetadata || string(object.Data) != filesystemRootMagic {
		return filesystemRoot{}, fmt.Errorf("object %s is not a filesystem root", object.ID.key())
	}
	var root filesystemRoot
	for _, edge := range object.Children {
		switch edge.Slot {
		case 0:
			root.inodes = edge.Child
		default:
			return filesystemRoot{}, fmt.Errorf("filesystem root %s has invalid edge slot %d", object.ID.key(), edge.Slot)
		}
	}
	return root, nil
}

func directoryKey(parent uint64, name string) ([]byte, error) {
	if parent == 0 || name == "" || len(name) > 255 || bytes.IndexByte([]byte(name), 0) >= 0 {
		return nil, errors.New("invalid TTC directory entry")
	}
	key := make([]byte, 8+len(name))
	binary.BigEndian.PutUint64(key, parent)
	copy(key[8:], name)
	return key, nil
}

func encodeRecord(value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(data) > maxObjectDataBytes {
		return nil, errors.New("TTC filesystem metadata record is too large")
	}
	return data, nil
}

func inodeChildren(inode InodeRecord) ([]Edge, error) {
	var children []Edge
	if !inode.File.IsZero() {
		if inode.File.Kind != ObjectKindFile {
			return nil, errors.New("inode content is not a whole-file object")
		}
		children = append(children, Edge{Slot: 0, Child: inode.File})
	}
	if !inode.Directory.IsZero() {
		if inode.Directory.Kind != ObjectKindDirectoryNode {
			return nil, errors.New("inode directory is not a directory-tree object")
		}
		children = append(children, Edge{Slot: 1, Child: inode.Directory})
	}
	return children, nil
}

func (builder *bTreeBuilder) set(root ObjectID, key []byte, value ObjectID) (ObjectID, error) {
	if root.IsZero() {
		return builder.makeNode(bTreeNode{leaf: true, keys: [][]byte{append([]byte(nil), key...)}, children: []ObjectID{value}})
	}
	ids, minima, err := builder.insert(root, key, value)
	if err != nil {
		return ObjectID{}, err
	}
	if len(ids) == 1 {
		return ids[0], nil
	}
	return builder.makeNode(bTreeNode{keys: minima, children: ids})
}

func (builder *bTreeBuilder) delete(root ObjectID, key []byte) (ObjectID, bool, error) {
	if root.IsZero() {
		return ObjectID{}, false, nil
	}
	updated, _, changed, err := builder.remove(root, key)
	return updated, changed, err
}

// ApplyFilesystem atomically applies operations by path-copying the affected
// inode and directory B-tree paths, then replacing one logical filesystem
// root. Forking that root remains one reference increment.
func (s *Store) ApplyFilesystem(origin SnapshotID, rootName string, operations []FilesystemOperation) (ObjectID, error) {
	if err := validateRootName(rootName); err != nil {
		return ObjectID{}, err
	}
	if len(operations) == 0 {
		return ObjectID{}, errors.New("empty TTC filesystem transaction")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	mutation, err := newGraphMutation(s, origin)
	if err != nil {
		return ObjectID{}, err
	}
	var root filesystemRoot
	if current, ok := s.state.Roots[rootName]; ok {
		object, ok := s.state.Objects[current.key()]
		if !ok {
			return ObjectID{}, fmt.Errorf("filesystem root object %s is missing", current.key())
		}
		root, err = decodeFilesystemRoot(object)
		if err != nil {
			return ObjectID{}, err
		}
	}
	inodeBuilder := bTreeBuilder{mutation: mutation, kind: ObjectKindInodeNode}
	for _, operation := range operations {
		selectors := 0
		if operation.PutInode != nil {
			selectors++
		}
		if operation.DeleteInode != 0 {
			selectors++
		}
		if operation.PutEntry != nil {
			selectors++
		}
		if operation.DeleteEntry != nil {
			selectors++
		}
		if selectors != 1 {
			return ObjectID{}, errors.New("invalid TTC filesystem operation selector")
		}
		switch {
		case operation.PutInode != nil:
			inode := *operation.PutInode
			if inode.Number == 0 || inode.Type == "" {
				return ObjectID{}, errors.New("invalid TTC inode record")
			}
			children, err := inodeChildren(inode)
			if err != nil {
				return ObjectID{}, err
			}
			data, err := encodeRecord(inode)
			if err != nil {
				return ObjectID{}, err
			}
			object, err := mutation.create(ObjectKindInode, data, children)
			if err != nil {
				return ObjectID{}, err
			}
			root.inodes, err = inodeBuilder.set(root.inodes, InodeKey(inode.Number), object)
			if err != nil {
				return ObjectID{}, err
			}
		case operation.DeleteInode != 0:
			root.inodes, _, err = inodeBuilder.delete(root.inodes, InodeKey(operation.DeleteInode))
			if err != nil {
				return ObjectID{}, err
			}
		case operation.PutEntry != nil:
			entry := *operation.PutEntry
			key, err := directoryKey(entry.Parent, entry.Name)
			if err != nil || entry.Inode == 0 {
				return ObjectID{}, errors.New("invalid TTC directory entry record")
			}
			if _, exists, lookupErr := bTreeLookupMutation(mutation, root.inodes, ObjectKindInodeNode, InodeKey(entry.Inode)); lookupErr != nil || !exists {
				return ObjectID{}, fmt.Errorf("looking up directory entry inode %d: %w", entry.Inode, lookupErr)
			}
			parentID, ok, err := bTreeLookupMutation(mutation, root.inodes, ObjectKindInodeNode, InodeKey(entry.Parent))
			if err != nil || !ok {
				return ObjectID{}, fmt.Errorf("looking up parent inode %d: %w", entry.Parent, err)
			}
			parentObject, _ := mutation.object(parentID)
			var parent InodeRecord
			if err := json.Unmarshal(parentObject.Data, &parent); err != nil {
				return ObjectID{}, err
			}
			if parent.Type != "directory" {
				return ObjectID{}, errors.New("directory entry parent is not a directory")
			}
			data, err := encodeRecord(entry)
			if err != nil {
				return ObjectID{}, err
			}
			object, err := mutation.create(ObjectKindMetadata, data, nil)
			if err != nil {
				return ObjectID{}, err
			}
			directoryBuilder := bTreeBuilder{mutation: mutation, kind: ObjectKindDirectoryNode}
			parent.Directory, err = directoryBuilder.set(parent.Directory, key[8:], object)
			if err != nil {
				return ObjectID{}, err
			}
			parentData, err := encodeRecord(parent)
			if err != nil {
				return ObjectID{}, err
			}
			parentEdges, err := inodeChildren(parent)
			if err != nil {
				return ObjectID{}, err
			}
			parentObjectID, err := mutation.create(ObjectKindInode, parentData, parentEdges)
			if err != nil {
				return ObjectID{}, err
			}
			root.inodes, err = inodeBuilder.set(root.inodes, InodeKey(parent.Number), parentObjectID)
			if err != nil {
				return ObjectID{}, err
			}
		case operation.DeleteEntry != nil:
			key, err := directoryKey(operation.DeleteEntry.Parent, operation.DeleteEntry.Name)
			if err != nil {
				return ObjectID{}, err
			}
			parentID, ok, lookupErr := bTreeLookupMutation(mutation, root.inodes, ObjectKindInodeNode, InodeKey(operation.DeleteEntry.Parent))
			if lookupErr != nil || !ok {
				return ObjectID{}, fmt.Errorf("looking up parent inode %d: %w", operation.DeleteEntry.Parent, lookupErr)
			}
			parentObject, _ := mutation.object(parentID)
			var parent InodeRecord
			if err := json.Unmarshal(parentObject.Data, &parent); err != nil {
				return ObjectID{}, err
			}
			directoryBuilder := bTreeBuilder{mutation: mutation, kind: ObjectKindDirectoryNode}
			parent.Directory, _, err = directoryBuilder.delete(parent.Directory, key[8:])
			if err != nil {
				return ObjectID{}, err
			}
			parentData, err := encodeRecord(parent)
			if err != nil {
				return ObjectID{}, err
			}
			parentEdges, err := inodeChildren(parent)
			if err != nil {
				return ObjectID{}, err
			}
			parentObjectID, err := mutation.create(ObjectKindInode, parentData, parentEdges)
			if err != nil {
				return ObjectID{}, err
			}
			root.inodes, err = inodeBuilder.set(root.inodes, InodeKey(parent.Number), parentObjectID)
			if err != nil {
				return ObjectID{}, err
			}
		}
	}
	var edges []Edge
	if !root.inodes.IsZero() {
		edges = append(edges, Edge{Slot: 0, Child: root.inodes})
	}
	if len(edges) == 0 {
		return ObjectID{}, errors.New("TTC filesystem transaction produced an empty filesystem")
	}
	logicalRoot, err := mutation.create(ObjectKindMetadata, []byte(filesystemRootMagic), edges)
	if err != nil {
		return ObjectID{}, err
	}
	if err := mutation.setRoot(rootName, logicalRoot); err != nil {
		return ObjectID{}, err
	}
	if err := mutation.commit(rootName, logicalRoot); err != nil {
		return ObjectID{}, err
	}
	return logicalRoot, nil
}

func bTreeLookupState(state graphState, root ObjectID, kind ObjectKind, key []byte) (ObjectID, bool, error) {
	return bTreeLookupObjects(root, kind, key, func(id ObjectID) (Object, bool) {
		object, ok := state.Objects[id.key()]
		return object, ok
	})
}

func bTreeLookupMutation(mutation *graphMutation, root ObjectID, kind ObjectKind, key []byte) (ObjectID, bool, error) {
	return bTreeLookupObjects(root, kind, key, mutation.object)
}

func bTreeLookupObjects(root ObjectID, kind ObjectKind, key []byte, objectFor func(ObjectID) (Object, bool)) (ObjectID, bool, error) {
	if root.IsZero() {
		return ObjectID{}, false, nil
	}
	current := root
	for depth := 0; depth < 64; depth++ {
		object, ok := objectFor(current)
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

// FilesystemInode resolves immutable inode metadata.
func (s *Store) FilesystemInode(rootID ObjectID, number uint64) (InodeRecord, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rootObject, ok := s.state.Objects[rootID.key()]
	if !ok {
		return InodeRecord{}, false, fmt.Errorf("filesystem root %s is missing", rootID.key())
	}
	root, err := decodeFilesystemRoot(rootObject)
	if err != nil {
		return InodeRecord{}, false, err
	}
	id, ok, err := bTreeLookupState(s.state, root.inodes, ObjectKindInodeNode, InodeKey(number))
	if err != nil || !ok {
		return InodeRecord{}, ok, err
	}
	object := s.state.Objects[id.key()]
	if object.ID.Kind != ObjectKindInode {
		return InodeRecord{}, false, errors.New("inode tree references non-inode object")
	}
	var inode InodeRecord
	if err := json.Unmarshal(object.Data, &inode); err != nil {
		return InodeRecord{}, false, err
	}
	return inode, true, nil
}

// FilesystemLookup resolves a directory entry without an ancestry walk.
func (s *Store) FilesystemLookup(rootID ObjectID, parent uint64, name string) (DirectoryEntryRecord, bool, error) {
	key, err := directoryKey(parent, name)
	if err != nil {
		return DirectoryEntryRecord{}, false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	rootObject, ok := s.state.Objects[rootID.key()]
	if !ok {
		return DirectoryEntryRecord{}, false, fmt.Errorf("filesystem root %s is missing", rootID.key())
	}
	root, err := decodeFilesystemRoot(rootObject)
	if err != nil {
		return DirectoryEntryRecord{}, false, err
	}
	parentID, ok, err := bTreeLookupState(s.state, root.inodes, ObjectKindInodeNode, InodeKey(parent))
	if err != nil || !ok {
		return DirectoryEntryRecord{}, ok, err
	}
	parentObject := s.state.Objects[parentID.key()]
	var parentInode InodeRecord
	if err := json.Unmarshal(parentObject.Data, &parentInode); err != nil {
		return DirectoryEntryRecord{}, false, err
	}
	id, ok, err := bTreeLookupState(s.state, parentInode.Directory, ObjectKindDirectoryNode, key[8:])
	if err != nil || !ok {
		return DirectoryEntryRecord{}, ok, err
	}
	object := s.state.Objects[id.key()]
	var entry DirectoryEntryRecord
	if object.ID.Kind != ObjectKindMetadata {
		return entry, false, errors.New("directory tree references non-metadata object")
	}
	if err := json.Unmarshal(object.Data, &entry); err != nil {
		return entry, false, err
	}
	return entry, true, nil
}

// FilesystemDirectory lists the entries immediately beneath parent. Work is
// proportional to the requested directory, not the recursive filesystem.
func (s *Store) FilesystemDirectory(rootID ObjectID, parent uint64) ([]DirectoryEntryRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rootObject, ok := s.state.Objects[rootID.key()]
	if !ok {
		return nil, fmt.Errorf("filesystem root %s is missing", rootID.key())
	}
	root, err := decodeFilesystemRoot(rootObject)
	if err != nil {
		return nil, err
	}
	parentID, ok, err := bTreeLookupState(s.state, root.inodes, ObjectKindInodeNode, InodeKey(parent))
	if err != nil || !ok {
		return nil, err
	}
	parentObject := s.state.Objects[parentID.key()]
	var parentInode InodeRecord
	if err := json.Unmarshal(parentObject.Data, &parentInode); err != nil {
		return nil, err
	}
	if parentInode.Directory.IsZero() {
		return nil, nil
	}
	var result []DirectoryEntryRecord
	var visit func(ObjectID) error
	visit = func(id ObjectID) error {
		object, ok := s.state.Objects[id.key()]
		if !ok {
			return fmt.Errorf("directory node %s is missing", id.key())
		}
		node, err := decodeBTreeNode(object, ObjectKindDirectoryNode)
		if err != nil {
			return err
		}
		if !node.leaf {
			for _, child := range node.children {
				if err := visit(child); err != nil {
					return err
				}
			}
			return nil
		}
		for index := range node.keys {
			value := s.state.Objects[node.children[index].key()]
			var entry DirectoryEntryRecord
			if err := json.Unmarshal(value.Data, &entry); err != nil {
				return err
			}
			result = append(result, entry)
		}
		return nil
	}
	if err := visit(parentInode.Directory); err != nil {
		return nil, err
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

// FilesystemRecords returns a consistent decoded view of a filesystem root.
// It is intended for export, diagnostics, and adapters that are being moved
// from a flat metadata representation. Normal pathname operations should use
// FilesystemInode, FilesystemLookup, and FilesystemDirectory so their work is
// independent of the total filesystem size.
func (s *Store) FilesystemRecords(rootID ObjectID) ([]InodeRecord, []DirectoryEntryRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rootObject, ok := s.state.Objects[rootID.key()]
	if !ok {
		return nil, nil, fmt.Errorf("filesystem root %s is missing", rootID.key())
	}
	root, err := decodeFilesystemRoot(rootObject)
	if err != nil {
		return nil, nil, err
	}
	var inodeIDs []ObjectID
	if !root.inodes.IsZero() {
		inodeIDs, err = bTreeValuesState(s.state, root.inodes, ObjectKindInodeNode)
		if err != nil {
			return nil, nil, err
		}
	}
	inodes := make([]InodeRecord, 0, len(inodeIDs))
	var entries []DirectoryEntryRecord
	for _, id := range inodeIDs {
		object, ok := s.state.Objects[id.key()]
		if !ok || object.ID.Kind != ObjectKindInode {
			return nil, nil, fmt.Errorf("inode tree references missing or invalid object %s", id.key())
		}
		var inode InodeRecord
		if err := json.Unmarshal(object.Data, &inode); err != nil {
			return nil, nil, err
		}
		inodes = append(inodes, inode)
		if inode.Directory.IsZero() {
			continue
		}
		entryIDs, err := bTreeValuesState(s.state, inode.Directory, ObjectKindDirectoryNode)
		if err != nil {
			return nil, nil, err
		}
		for _, entryID := range entryIDs {
			entryObject, ok := s.state.Objects[entryID.key()]
			if !ok || entryObject.ID.Kind != ObjectKindMetadata {
				return nil, nil, fmt.Errorf("directory tree references missing or invalid object %s", entryID.key())
			}
			var entry DirectoryEntryRecord
			if err := json.Unmarshal(entryObject.Data, &entry); err != nil {
				return nil, nil, err
			}
			entries = append(entries, entry)
		}
	}
	sort.Slice(inodes, func(i, j int) bool { return inodes[i].Number < inodes[j].Number })
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Parent != entries[j].Parent {
			return entries[i].Parent < entries[j].Parent
		}
		return entries[i].Name < entries[j].Name
	})
	return inodes, entries, nil
}

func bTreeValuesState(state graphState, root ObjectID, kind ObjectKind) ([]ObjectID, error) {
	var values []ObjectID
	var visit func(ObjectID, int) error
	visit = func(id ObjectID, depth int) error {
		if depth >= bTreeMaxDepth {
			return errors.New("TTC B-tree exceeds maximum structural depth")
		}
		object, ok := state.Objects[id.key()]
		if !ok {
			return fmt.Errorf("TTC B-tree node %s is missing", id.key())
		}
		node, err := decodeBTreeNode(object, kind)
		if err != nil {
			return err
		}
		if node.leaf {
			values = append(values, node.children...)
			return nil
		}
		for _, child := range node.children {
			if err := visit(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(root, 0); err != nil {
		return nil, err
	}
	return values, nil
}
