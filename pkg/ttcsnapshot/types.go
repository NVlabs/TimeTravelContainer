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
	"strings"
)

const (
	formatVersion      = 5
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
	// ObjectKindMemoryTable describes the pages of 1 MiB of a memory tree; see
	// memory.go.
	ObjectKindMemoryTable
)

func (k ObjectKind) valid() bool {
	return k >= ObjectKindPage && k <= ObjectKindMemoryTable
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

// Compare orders id and other as their String forms order: by snapshot, then
// kind, then ordinal.
func (id ObjectID) Compare(other ObjectID) int {
	return compareObjectIDs(id, other)
}

// ParseObjectID parses a canonical stable object ID.
func ParseObjectID(value string) (ObjectID, error) {
	return parseObjectKey(value)
}

// MemoryChunkID identifies one immutable cold-memory chunk.
type MemoryChunkID ObjectID

// FileID identifies one immutable whole-file data object.
type FileID ObjectID

// InodeID identifies one immutable inode record.
type InodeID ObjectID

// MetadataID identifies one immutable graph metadata object.
type MetadataID ObjectID

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
	// Lower-case hex digits of these widths are exactly the canonical form.
	snapshot, snapshotOK := parseLowerHex(key[0:16])
	kind, kindOK := parseLowerHex(key[17:19])
	ordinal, ordinalOK := parseLowerHex(key[20:36])
	if !snapshotOK || !kindOK || !ordinalOK {
		return ObjectID{}, fmt.Errorf("non-canonical TTC object key %q", key)
	}
	id := ObjectID{Snapshot: SnapshotID(snapshot), Kind: ObjectKind(kind), Ordinal: ordinal}
	if err := id.validate(); err != nil {
		return ObjectID{}, err
	}
	return id, nil
}

// parseLowerHex parses digits, lower-case hex ones only.
func parseLowerHex(digits string) (uint64, bool) {
	var value uint64
	for i := 0; i < len(digits); i++ {
		switch c := digits[i]; {
		case '0' <= c && c <= '9':
			value = value<<4 | uint64(c-'0')
		case 'a' <= c && c <= 'f':
			value = value<<4 | uint64(c-'a'+10)
		default:
			return 0, false
		}
	}
	return value, true
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
	// SourceCatalog and SourceObject preserve the immutable foreign identity
	// from which this node was materialized. Deferred objects contain only
	// these fields until a tree walk reaches them; expansion installs the
	// bounded set of direct child stubs in one graph transaction.
	SourceCatalog string   `json:",omitempty"`
	SourceObject  ObjectID `json:",omitempty"`
	Deferred      bool     `json:",omitempty"`
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
	if object.Deferred {
		if object.SourceCatalog == "" || object.SourceObject.IsZero() || object.SourceObject.Kind != object.ID.Kind {
			return fmt.Errorf("deferred object %s has invalid foreign identity", object.ID.key())
		}
		if object.BulkLength != 0 || len(object.Data) != 0 || len(object.Children) != 0 {
			return fmt.Errorf("deferred object %s contains materialized state", object.ID.key())
		}
	} else if (object.SourceCatalog == "") != object.SourceObject.IsZero() {
		return fmt.Errorf("object %s has incomplete foreign identity", object.ID.key())
	}
	if len(object.SourceCatalog) > 256 || strings.ContainsRune(object.SourceCatalog, 0) {
		return fmt.Errorf("object %s has invalid foreign catalog", object.ID.key())
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
	// Sort edges only when they are not sorted: objects a graph's cached state
	// shares are never written.
	bySlot := func(i, j int) bool { return object.Children[i].Slot < object.Children[j].Slot }
	if !sort.SliceIsSorted(object.Children, bySlot) {
		sort.Slice(object.Children, bySlot)
	}
	return nil
}

func validateRootName(name string) error {
	if name == "" || len(name) > 512 || strings.TrimSpace(name) != name || strings.ContainsRune(name, 0) {
		return errors.New("invalid TTC snapshot root name")
	}
	return nil
}
