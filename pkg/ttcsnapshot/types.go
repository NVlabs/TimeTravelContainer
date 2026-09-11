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

// Package ttcsnapshot implements the common logical snapshot graph used by
// standalone and distributed TTC storage backends.
package ttcsnapshot

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const (
	formatVersion      = 4
	maxObjectDataBytes = 4 << 20
	maxObjectEdges     = 256
)

// SnapshotID is a monotonically allocated snapshot origin. Zero is invalid.
type SnapshotID uint64

// ObjectKind identifies the interpretation of one immutable graph object.
type ObjectKind uint8

const (
	ObjectKindInvalid ObjectKind = iota
	ObjectKindPage
	ObjectKindFile
	ObjectKindMemoryNode
	ObjectKindInode
	ObjectKindInodeNode
	ObjectKindDirectoryNode
	ObjectKindMetadata
	ObjectKindMemoryChunk
)

func (k ObjectKind) valid() bool {
	return k >= ObjectKindPage && k <= ObjectKindMemoryChunk
}

// ObjectID is stable across physical relocation and compaction. Ordinals are
// allocated independently inside one snapshot origin.
type ObjectID struct {
	Snapshot SnapshotID
	Ordinal  uint64
	Kind     ObjectKind
}

// String returns the canonical stable object ID.
func (id ObjectID) String() string {
	return id.key()
}

// ParseObjectID parses a canonical stable object ID.
func ParseObjectID(value string) (ObjectID, error) {
	return parseObjectKey(value)
}

// Typed IDs prevent callers from confusing bulk objects at API boundaries.
type PageID ObjectID
type MemoryChunkID ObjectID
type FileID ObjectID
type InodeID ObjectID
type MetadataID ObjectID

// AsPageID checks and converts id to a page ID.
func AsPageID(id ObjectID) (PageID, error) {
	if id.Kind != ObjectKindPage {
		return PageID{}, fmt.Errorf("object %s is not a page", id.key())
	}
	return PageID(id), nil
}

// AsMemoryChunkID checks and converts id to a cold memory chunk ID.
func AsMemoryChunkID(id ObjectID) (MemoryChunkID, error) {
	if id.Kind != ObjectKindMemoryChunk {
		return MemoryChunkID{}, fmt.Errorf("object %s is not a memory chunk", id.key())
	}
	return MemoryChunkID(id), nil
}

// AsFileID checks and converts id to a file ID.
func AsFileID(id ObjectID) (FileID, error) {
	if id.Kind != ObjectKindFile {
		return FileID{}, fmt.Errorf("object %s is not a file", id.key())
	}
	return FileID(id), nil
}

// AsInodeID checks and converts id to an inode ID.
func AsInodeID(id ObjectID) (InodeID, error) {
	if id.Kind != ObjectKindInode {
		return InodeID{}, fmt.Errorf("object %s is not an inode", id.key())
	}
	return InodeID(id), nil
}

// AsMetadataID checks and converts id to a metadata ID.
func AsMetadataID(id ObjectID) (MetadataID, error) {
	if id.Kind != ObjectKindMetadata {
		return MetadataID{}, fmt.Errorf("object %s is not metadata", id.key())
	}
	return MetadataID(id), nil
}

// IsZero reports whether id is the absent reference.
func (id ObjectID) IsZero() bool {
	return id == (ObjectID{})
}

func (id ObjectID) validate() error {
	if id.Snapshot == 0 || id.Ordinal == 0 || !id.Kind.valid() {
		return fmt.Errorf("invalid TTC object ID %+v", id)
	}
	return nil
}

func (id ObjectID) key() string {
	var raw [17]byte
	binary.BigEndian.PutUint64(raw[0:8], uint64(id.Snapshot))
	raw[8] = byte(id.Kind)
	binary.BigEndian.PutUint64(raw[9:17], id.Ordinal)
	var encoded [36]byte
	hex.Encode(encoded[0:16], raw[0:8])
	encoded[16] = '-'
	hex.Encode(encoded[17:19], raw[8:9])
	encoded[19] = '-'
	hex.Encode(encoded[20:36], raw[9:17])
	return string(encoded[:])
}

func parseObjectKey(key string) (ObjectID, error) {
	if len(key) != 36 || key[16] != '-' || key[19] != '-' {
		return ObjectID{}, fmt.Errorf("invalid TTC object key %q", key)
	}
	snapshot, err := strconv.ParseUint(key[0:16], 16, 64)
	if err != nil {
		return ObjectID{}, fmt.Errorf("invalid TTC object key %q: %w", key, err)
	}
	kind, err := strconv.ParseUint(key[17:19], 16, 8)
	if err != nil {
		return ObjectID{}, fmt.Errorf("invalid TTC object key %q: %w", key, err)
	}
	ordinal, err := strconv.ParseUint(key[20:36], 16, 64)
	if err != nil {
		return ObjectID{}, fmt.Errorf("invalid TTC object key %q: %w", key, err)
	}
	id := ObjectID{Snapshot: SnapshotID(snapshot), Kind: ObjectKind(kind), Ordinal: ordinal}
	if err := id.validate(); err != nil {
		return ObjectID{}, err
	}
	if id.key() != key {
		return ObjectID{}, fmt.Errorf("non-canonical TTC object key %q", key)
	}
	return id, nil
}

// SnapshotState is the authority state of one local snapshot origin.
type SnapshotState string

const (
	SnapshotWorking        SnapshotState = "working"
	SnapshotSealedLocal    SnapshotState = "sealed-local"
	SnapshotSealedVolatile SnapshotState = "sealed-volatile"
	SnapshotShared         SnapshotState = "shared"
	SnapshotReclaiming     SnapshotState = "reclaiming"
	SnapshotDead           SnapshotState = "dead"
)

func (s SnapshotState) valid() bool {
	switch s {
	case SnapshotWorking, SnapshotSealedLocal, SnapshotSealedVolatile, SnapshotShared, SnapshotReclaiming, SnapshotDead:
		return true
	default:
		return false
	}
}

// Snapshot records the next object ordinal and the origin authority state.
type Snapshot struct {
	ID          SnapshotID
	State       SnapshotState
	NextOrdinal uint64
}

// ObjectState prevents a zero-count object from being resurrected.
type ObjectState string

const (
	ObjectLive       ObjectState = "live"
	ObjectReclaiming ObjectState = "reclaiming"
)

// Edge is one counted parent-to-child graph edge. Slots are unique within a
// parent and make activation/deactivation idempotency keys stable.
type Edge struct {
	Slot  uint16
	Child ObjectID
}

// Object is graph metadata. Bulk page and file bytes live in a backend selected
// by the object's typed ID and are not stored here.
type Object struct {
	ID         ObjectID
	State      ObjectState
	Refs       uint64
	BulkLength uint64
	Data       []byte
	Children   []Edge
}

func validateObject(object Object) error {
	if err := object.ID.validate(); err != nil {
		return err
	}
	if object.State != ObjectLive && object.State != ObjectReclaiming {
		return fmt.Errorf("object %s has invalid state %q", object.ID.key(), object.State)
	}
	if object.State == ObjectReclaiming && object.Refs != 0 {
		return fmt.Errorf("reclaiming object %s has %d references", object.ID.key(), object.Refs)
	}
	if len(object.Data) > maxObjectDataBytes {
		return fmt.Errorf("object %s has %d metadata bytes, limit is %d", object.ID.key(), len(object.Data), maxObjectDataBytes)
	}
	if object.BulkLength != 0 && object.ID.Kind != ObjectKindPage && object.ID.Kind != ObjectKindFile && object.ID.Kind != ObjectKindMemoryChunk {
		return fmt.Errorf("metadata object %s declares bulk data", object.ID.key())
	}
	if object.ID.Kind == ObjectKindMemoryChunk && object.BulkLength > MemoryChunkBytes {
		return fmt.Errorf("memory chunk %s has %d bytes, limit is %d", object.ID.key(), object.BulkLength, MemoryChunkBytes)
	}
	if len(object.Children) > maxObjectEdges {
		return fmt.Errorf("object %s has %d edges, limit is %d", object.ID.key(), len(object.Children), maxObjectEdges)
	}
	seen := make(map[uint16]struct{}, len(object.Children))
	for _, edge := range object.Children {
		if err := edge.Child.validate(); err != nil {
			return fmt.Errorf("object %s child: %w", object.ID.key(), err)
		}
		if _, ok := seen[edge.Slot]; ok {
			return fmt.Errorf("object %s repeats child slot %d", object.ID.key(), edge.Slot)
		}
		seen[edge.Slot] = struct{}{}
	}
	sort.Slice(object.Children, func(i, j int) bool {
		return object.Children[i].Slot < object.Children[j].Slot
	})
	return nil
}

func validateRootName(name string) error {
	if name == "" || len(name) > 512 || strings.TrimSpace(name) != name || strings.ContainsRune(name, 0) {
		return errors.New("invalid TTC snapshot root name")
	}
	return nil
}
