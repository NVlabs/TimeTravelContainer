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

// filesystemJournalMutation is the compact, deterministic WAL form of a
// filesystem update. Immutable B-tree nodes are reconstructed from these
// logical operations during recovery instead of serializing every path-copy
// node and reference-count adjustment.
type filesystemJournalMutation struct {
	Origin      SnapshotID
	RootName    string
	Operations  []FilesystemOperation
	ReleasePins []string
	Result      ObjectID
}

func validateFilesystemJournalMutation(mutation filesystemJournalMutation) error {
	if mutation.Origin == 0 || mutation.Result.IsZero() {
		return errors.New("invalid TTC filesystem journal identity")
	}
	if err := validateRootName(mutation.RootName); err != nil {
		return err
	}
	if len(mutation.Operations) == 0 {
		return errors.New("empty TTC filesystem journal transaction")
	}
	seenPins := make(map[string]struct{}, len(mutation.ReleasePins))
	for _, pin := range mutation.ReleasePins {
		if err := validateRootName(pin); err != nil {
			return fmt.Errorf("invalid released construction pin: %w", err)
		}
		if _, ok := seenPins[pin]; ok {
			return fmt.Errorf("filesystem journal repeats released pin %q", pin)
		}
		seenPins[pin] = struct{}{}
	}
	return nil
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
	return s.ApplyFilesystemAndReleasePins(origin, rootName, operations, nil)
}

// ApplyFilesystemAndReleasePins publishes a filesystem update and converts
// construction pins into immutable inode edges in the same transaction.
func (s *Store) ApplyFilesystemAndReleasePins(origin SnapshotID, rootName string, operations []FilesystemOperation, releasePins []string) (ObjectID, error) {
	if err := validateRootName(rootName); err != nil {
		return ObjectID{}, err
	}
	if len(operations) == 0 {
		return ObjectID{}, errors.New("empty TTC filesystem transaction")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	mutation, logicalRoot, err := buildFilesystemMutation(&s.state, origin, rootName, operations, releasePins)
	if err != nil {
		return ObjectID{}, err
	}
	journalMutation := filesystemJournalMutation{
		Origin: origin, RootName: rootName,
		Operations:  append([]FilesystemOperation(nil), operations...),
		ReleasePins: append([]string(nil), releasePins...), Result: logicalRoot,
	}
	err = s.commitLockedPrepared([]journalOp{{ApplyFilesystem: &journalMutation}}, nil, func(state *graphState) error {
		return installGraphMutation(state, mutation, rootName, logicalRoot, releasePins)
	})
	if err != nil {
		return ObjectID{}, err
	}
	return logicalRoot, nil
}

func buildFilesystemMutation(state *graphState, origin SnapshotID, rootName string, operations []FilesystemOperation, releasePins []string) (*graphMutation, ObjectID, error) {
	store := &Store{state: *state}
	mutation, err := newGraphMutation(store, origin)
	if err != nil {
		return nil, ObjectID{}, err
	}
	var root filesystemRoot
	if current, ok := state.Roots[rootName]; ok {
		object, ok := state.Objects[current.key()]
		if !ok {
			return nil, ObjectID{}, fmt.Errorf("filesystem root object %s is missing", current.key())
		}
		root, err = decodeFilesystemRoot(object)
		if err != nil {
			return nil, ObjectID{}, err
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
			return nil, ObjectID{}, errors.New("invalid TTC filesystem operation selector")
		}
		switch {
		case operation.PutInode != nil:
			inode := *operation.PutInode
			if inode.Number == 0 || inode.Type == "" {
				return nil, ObjectID{}, errors.New("invalid TTC inode record")
			}
			// Directory membership is maintained by PutEntry/DeleteEntry. The
			// public inode metadata used by filesystem clients intentionally
			// does not expose the internal directory-tree root, so an ordinary
			// chmod/chown/timestamp update carries a zero Directory field. Keep
			// the existing root instead of silently detaching every child.
			if inode.Type == "directory" && inode.Directory.IsZero() && !root.inodes.IsZero() {
				oldID, found, err := bTreeLookupMutation(mutation, root.inodes, ObjectKindInodeNode, InodeKey(inode.Number))
				if err != nil {
					return nil, ObjectID{}, err
				}
				if found {
					oldObject, ok := mutation.object(oldID)
					if !ok || oldObject.ID.Kind != ObjectKindInode {
						return nil, ObjectID{}, errors.New("inode tree references missing or invalid inode object")
					}
					var old InodeRecord
					if err := json.Unmarshal(oldObject.Data, &old); err != nil {
						return nil, ObjectID{}, err
					}
					inode.Directory = old.Directory
				}
			}
			children, err := inodeChildren(inode)
			if err != nil {
				return nil, ObjectID{}, err
			}
			data, err := encodeRecord(inode)
			if err != nil {
				return nil, ObjectID{}, err
			}
			object, err := mutation.create(ObjectKindInode, data, children)
			if err != nil {
				return nil, ObjectID{}, err
			}
			root.inodes, err = inodeBuilder.set(root.inodes, InodeKey(inode.Number), object)
			if err != nil {
				return nil, ObjectID{}, err
			}
		case operation.DeleteInode != 0:
			root.inodes, _, err = inodeBuilder.delete(root.inodes, InodeKey(operation.DeleteInode))
			if err != nil {
				return nil, ObjectID{}, err
			}
		case operation.PutEntry != nil:
			entry := *operation.PutEntry
			key, err := directoryKey(entry.Parent, entry.Name)
			if err != nil || entry.Inode == 0 {
				return nil, ObjectID{}, errors.New("invalid TTC directory entry record")
			}
			if _, exists, lookupErr := bTreeLookupMutation(mutation, root.inodes, ObjectKindInodeNode, InodeKey(entry.Inode)); lookupErr != nil || !exists {
				return nil, ObjectID{}, fmt.Errorf("looking up directory entry inode %d: %w", entry.Inode, lookupErr)
			}
			parentID, ok, err := bTreeLookupMutation(mutation, root.inodes, ObjectKindInodeNode, InodeKey(entry.Parent))
			if err != nil || !ok {
				return nil, ObjectID{}, fmt.Errorf("looking up parent inode %d: %w", entry.Parent, err)
			}
			parentObject, _ := mutation.object(parentID)
			var parent InodeRecord
			if err := json.Unmarshal(parentObject.Data, &parent); err != nil {
				return nil, ObjectID{}, err
			}
			if parent.Type != "directory" {
				return nil, ObjectID{}, errors.New("directory entry parent is not a directory")
			}
			data, err := encodeRecord(entry)
			if err != nil {
				return nil, ObjectID{}, err
			}
			object, err := mutation.create(ObjectKindMetadata, data, nil)
			if err != nil {
				return nil, ObjectID{}, err
			}
			directoryBuilder := bTreeBuilder{mutation: mutation, kind: ObjectKindDirectoryNode}
			parent.Directory, err = directoryBuilder.set(parent.Directory, key[8:], object)
			if err != nil {
				return nil, ObjectID{}, err
			}
			parentData, err := encodeRecord(parent)
			if err != nil {
				return nil, ObjectID{}, err
			}
			parentEdges, err := inodeChildren(parent)
			if err != nil {
				return nil, ObjectID{}, err
			}
			parentObjectID, err := mutation.create(ObjectKindInode, parentData, parentEdges)
			if err != nil {
				return nil, ObjectID{}, err
			}
			root.inodes, err = inodeBuilder.set(root.inodes, InodeKey(parent.Number), parentObjectID)
			if err != nil {
				return nil, ObjectID{}, err
			}
		case operation.DeleteEntry != nil:
			key, err := directoryKey(operation.DeleteEntry.Parent, operation.DeleteEntry.Name)
			if err != nil {
				return nil, ObjectID{}, err
			}
			parentID, ok, lookupErr := bTreeLookupMutation(mutation, root.inodes, ObjectKindInodeNode, InodeKey(operation.DeleteEntry.Parent))
			if lookupErr != nil || !ok {
				return nil, ObjectID{}, fmt.Errorf("looking up parent inode %d: %w", operation.DeleteEntry.Parent, lookupErr)
			}
			parentObject, _ := mutation.object(parentID)
			var parent InodeRecord
			if err := json.Unmarshal(parentObject.Data, &parent); err != nil {
				return nil, ObjectID{}, err
			}
			directoryBuilder := bTreeBuilder{mutation: mutation, kind: ObjectKindDirectoryNode}
			parent.Directory, _, err = directoryBuilder.delete(parent.Directory, key[8:])
			if err != nil {
				return nil, ObjectID{}, err
			}
			parentData, err := encodeRecord(parent)
			if err != nil {
				return nil, ObjectID{}, err
			}
			parentEdges, err := inodeChildren(parent)
			if err != nil {
				return nil, ObjectID{}, err
			}
			parentObjectID, err := mutation.create(ObjectKindInode, parentData, parentEdges)
			if err != nil {
				return nil, ObjectID{}, err
			}
			root.inodes, err = inodeBuilder.set(root.inodes, InodeKey(parent.Number), parentObjectID)
			if err != nil {
				return nil, ObjectID{}, err
			}
		}
	}
	var edges []Edge
	if !root.inodes.IsZero() {
		edges = append(edges, Edge{Slot: 0, Child: root.inodes})
	}
	if len(edges) == 0 {
		return nil, ObjectID{}, errors.New("TTC filesystem transaction produced an empty filesystem")
	}
	logicalRoot, err := mutation.create(ObjectKindMetadata, []byte(filesystemRootMagic), edges)
	if err != nil {
		return nil, ObjectID{}, err
	}
	if err := mutation.setRoot(rootName, logicalRoot); err != nil {
		return nil, ObjectID{}, err
	}
	for _, pin := range releasePins {
		id, ok := state.Pins[pin]
		if !ok {
			return nil, ObjectID{}, fmt.Errorf("construction pin %q does not exist", pin)
		}
		object, ok := mutation.object(id)
		if !ok {
			return nil, ObjectID{}, fmt.Errorf("construction pin %q references missing object %s", pin, id)
		}
		object, err = releaseObject(object)
		if err != nil {
			return nil, ObjectID{}, err
		}
		mutation.put(object)
	}
	return mutation, logicalRoot, nil
}

func installGraphMutation(state *graphState, mutation *graphMutation, rootName string, root ObjectID, releasePins []string) error {
	var reclaim []string
	for key, object := range mutation.objects {
		state.Objects[key] = cloneObject(object)
		if object.State == ObjectReclaiming {
			reclaim = append(reclaim, key)
		}
	}
	state.Snapshots[snapshotKey(mutation.snapshot.ID)] = mutation.snapshot
	state.Roots[rootName] = root
	for _, pin := range releasePins {
		delete(state.Pins, pin)
	}
	return collectFilesystemMutationGarbage(state, reclaim)
}

// collectFilesystemMutationGarbage eagerly collapses the obsolete path that
// the same root swap made unreachable. It never scans the graph: each visited
// object is reached by cascading from a zero-reference object touched by the
// transaction. Objects participating in an authority handoff retain the
// existing deferred-GC behavior.
func collectFilesystemMutationGarbage(state *graphState, queue []string) error {
	queued := make(map[string]struct{}, len(queue))
	for _, key := range queue {
		queued[key] = struct{}{}
	}
	for len(queue) != 0 {
		key := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		object, ok := state.Objects[key]
		if !ok || object.State != ObjectReclaiming || object.Refs != 0 {
			continue
		}
		if _, transferring := state.Handoffs[snapshotKey(object.ID.Snapshot)]; transferring {
			continue
		}
		delete(state.Objects, key)
		for _, edge := range object.Children {
			childKey := edge.Child.key()
			child, ok := state.Objects[childKey]
			if !ok || child.State != ObjectLive || child.Refs == 0 {
				return fmt.Errorf("reclaiming object %s references unavailable child %s", key, childKey)
			}
			child.Refs--
			if child.Refs == 0 {
				child.State = ObjectReclaiming
				if _, ok := queued[childKey]; !ok {
					queue = append(queue, childKey)
					queued[childKey] = struct{}{}
				}
			}
			state.Objects[childKey] = child
		}
	}
	return nil
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
	return filesystemInodeObjects(rootID, number, func(id ObjectID) (Object, bool) {
		object, ok := s.state.Objects[id.key()]
		return object, ok
	})
}

func filesystemInodeObjects(rootID ObjectID, number uint64, objectFor func(ObjectID) (Object, bool)) (InodeRecord, bool, error) {
	rootObject, ok := objectFor(rootID)
	if !ok {
		return InodeRecord{}, false, fmt.Errorf("filesystem root %s is missing", rootID.key())
	}
	root, err := decodeFilesystemRoot(rootObject)
	if err != nil {
		return InodeRecord{}, false, err
	}
	id, ok, err := bTreeLookupObjects(root.inodes, ObjectKindInodeNode, InodeKey(number), objectFor)
	if err != nil || !ok {
		return InodeRecord{}, ok, err
	}
	object, exists := objectFor(id)
	if !exists || object.ID.Kind != ObjectKindInode {
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
	s.mu.RLock()
	defer s.mu.RUnlock()
	return filesystemLookupObjects(rootID, parent, name, func(id ObjectID) (Object, bool) {
		object, ok := s.state.Objects[id.key()]
		return object, ok
	})
}

func filesystemLookupObjects(rootID ObjectID, parent uint64, name string, objectFor func(ObjectID) (Object, bool)) (DirectoryEntryRecord, bool, error) {
	key, err := directoryKey(parent, name)
	if err != nil {
		return DirectoryEntryRecord{}, false, err
	}
	rootObject, ok := objectFor(rootID)
	if !ok {
		return DirectoryEntryRecord{}, false, fmt.Errorf("filesystem root %s is missing", rootID.key())
	}
	root, err := decodeFilesystemRoot(rootObject)
	if err != nil {
		return DirectoryEntryRecord{}, false, err
	}
	parentID, ok, err := bTreeLookupObjects(root.inodes, ObjectKindInodeNode, InodeKey(parent), objectFor)
	if err != nil || !ok {
		return DirectoryEntryRecord{}, ok, err
	}
	parentObject, exists := objectFor(parentID)
	if !exists || parentObject.ID.Kind != ObjectKindInode {
		return DirectoryEntryRecord{}, false, errors.New("inode tree references missing or invalid parent inode")
	}
	var parentInode InodeRecord
	if err := json.Unmarshal(parentObject.Data, &parentInode); err != nil {
		return DirectoryEntryRecord{}, false, err
	}
	id, ok, err := bTreeLookupObjects(parentInode.Directory, ObjectKindDirectoryNode, key[8:], objectFor)
	if err != nil || !ok {
		return DirectoryEntryRecord{}, ok, err
	}
	object, exists := objectFor(id)
	var entry DirectoryEntryRecord
	if !exists || object.ID.Kind != ObjectKindMetadata {
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
	return filesystemDirectoryObjects(rootID, parent, func(id ObjectID) (Object, bool) {
		object, ok := s.state.Objects[id.key()]
		return object, ok
	})
}

func filesystemDirectoryObjects(rootID ObjectID, parent uint64, objectFor func(ObjectID) (Object, bool)) ([]DirectoryEntryRecord, error) {
	rootObject, ok := objectFor(rootID)
	if !ok {
		return nil, fmt.Errorf("filesystem root %s is missing", rootID.key())
	}
	root, err := decodeFilesystemRoot(rootObject)
	if err != nil {
		return nil, err
	}
	parentID, ok, err := bTreeLookupObjects(root.inodes, ObjectKindInodeNode, InodeKey(parent), objectFor)
	if err != nil || !ok {
		return nil, err
	}
	parentObject, exists := objectFor(parentID)
	if !exists || parentObject.ID.Kind != ObjectKindInode {
		return nil, errors.New("inode tree references missing or invalid parent inode")
	}
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
		object, ok := objectFor(id)
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
			value, ok := objectFor(node.children[index])
			if !ok || value.ID.Kind != ObjectKindMetadata {
				return fmt.Errorf("directory tree references missing or invalid object %s", node.children[index].key())
			}
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
	return filesystemRecordsObjects(rootID, func(id ObjectID) (Object, bool) {
		object, ok := s.state.Objects[id.key()]
		return object, ok
	})
}

func filesystemRecordsObjects(rootID ObjectID, objectFor func(ObjectID) (Object, bool)) ([]InodeRecord, []DirectoryEntryRecord, error) {
	rootObject, ok := objectFor(rootID)
	if !ok {
		return nil, nil, fmt.Errorf("filesystem root %s is missing", rootID.key())
	}
	root, err := decodeFilesystemRoot(rootObject)
	if err != nil {
		return nil, nil, err
	}
	var inodeIDs []ObjectID
	if !root.inodes.IsZero() {
		inodeIDs, err = bTreeValuesObjects(root.inodes, ObjectKindInodeNode, objectFor)
		if err != nil {
			return nil, nil, err
		}
	}
	inodes := make([]InodeRecord, 0, len(inodeIDs))
	var entries []DirectoryEntryRecord
	for _, id := range inodeIDs {
		object, ok := objectFor(id)
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
		entryIDs, err := bTreeValuesObjects(inode.Directory, ObjectKindDirectoryNode, objectFor)
		if err != nil {
			return nil, nil, err
		}
		for _, entryID := range entryIDs {
			entryObject, ok := objectFor(entryID)
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
	return bTreeValuesObjects(root, kind, func(id ObjectID) (Object, bool) {
		object, ok := state.Objects[id.key()]
		return object, ok
	})
}

func bTreeValuesObjects(root ObjectID, kind ObjectKind, objectFor func(ObjectID) (Object, bool)) ([]ObjectID, error) {
	var values []ObjectID
	var visit func(ObjectID, int) error
	visit = func(id ObjectID, depth int) error {
		if depth >= bTreeMaxDepth {
			return errors.New("TTC B-tree exceeds maximum structural depth")
		}
		object, ok := objectFor(id)
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
