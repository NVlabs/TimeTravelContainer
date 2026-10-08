// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcsnapshot

import (
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"slices"
	"sort"
)

// A graph's checkpoint and journal records are binary: integers are varints,
// strings and byte strings carry their length, and each record of a journal,
// and a checkpoint whole, carries a CRC-32C of its contents. A checkpoint
// holds the graph's snapshots, objects, roots, pins, frozen snapshots and
// handoffs, its objects in no order: sorting them, and finding each again,
// took more than half the time it took to encode 300,000. As JSON, a
// checkpoint of a filesystem of 100,000 files was 130 MB, and decoding it was
// most of the second a process took to open the graph; it is 42 MB, and
// decodes in a quarter of the time.

const checkpointMagic = "TTCSGC6\n"

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// A byte string or list that may be nil is written with its length plus one,
// and a nil one as zero: a state decodes to what was encoded, nil or empty.

// Flags of an encoded object.
const (
	objectFlagReclaiming = 1 << iota
	objectFlagDeferred
	objectFlagSource
	objectFlagData
	objectFlagChildren
	objectFlags = objectFlagChildren<<1 - 1
)

// Operations of an encoded journal record.
const (
	opPutSnapshot = 1 + iota
	opDeleteSnapshot
	opPutObject
	opDeleteObject
	opPutRoot
	opDeleteRoot
	opPutPin
	opDeletePin
	opSetNextSnapshot
	opFreezeSnapshot
	opUnfreezeSnapshot
	opBeginHandoff
	opAppendHandoff
	opApplyFilesystem
	// opApplyFilesystemBalanced is opApplyFilesystem of a mutation whose
	// B-trees are balanced: see filesystemJournalMutation.Balanced.
	opApplyFilesystemBalanced
)

// Operations of an encoded filesystem mutation, which may have several.
const (
	fsFlagPutInode = 1 << iota
	fsFlagDeleteInode
	fsFlagPutEntry
	fsFlagDeleteEntry
	fsFlags = fsFlagDeleteEntry<<1 - 1
)

var snapshotStates = [...]SnapshotState{SnapshotWorking, SnapshotSealedLocal, SnapshotSealedVolatile, SnapshotShared, SnapshotReclaiming, SnapshotDead}

type graphEncoder struct {
	out []byte
}

func (e *graphEncoder) uvarint(value uint64) {
	e.out = binary.AppendUvarint(e.out, value)
}

func (e *graphEncoder) varint(value int64) {
	e.out = binary.AppendVarint(e.out, value)
}

func (e *graphEncoder) flag(value bool) {
	if value {
		e.out = append(e.out, 1)
	} else {
		e.out = append(e.out, 0)
	}
}

func (e *graphEncoder) string(value string) {
	e.uvarint(uint64(len(value)))
	e.out = append(e.out, value...)
}

func (e *graphEncoder) bytes(value []byte) {
	e.uvarint(uint64(len(value)))
	e.out = append(e.out, value...)
}

func (e *graphEncoder) nullableBytes(value []byte) {
	if value == nil {
		e.uvarint(0)
		return
	}
	e.uvarint(uint64(len(value)) + 1)
	e.out = append(e.out, value...)
}

func (e *graphEncoder) objectID(id ObjectID) {
	e.uvarint(uint64(id.Snapshot))
	e.out = append(e.out, byte(id.Kind))
	e.uvarint(id.Ordinal)
}

func (e *graphEncoder) snapshot(snapshot Snapshot) error {
	state := slices.Index(snapshotStates[:], snapshot.State)
	if state < 0 {
		return fmt.Errorf("snapshot %d has invalid state %q", snapshot.ID, snapshot.State)
	}
	e.uvarint(uint64(snapshot.ID))
	e.out = append(e.out, byte(state))
	e.uvarint(snapshot.NextOrdinal)
	return nil
}

func (e *graphEncoder) object(object *Object) error {
	var flags byte
	switch object.State {
	case ObjectLive:
	case ObjectReclaiming:
		flags |= objectFlagReclaiming
	default:
		return fmt.Errorf("object %s has invalid state %q", object.ID, object.State)
	}
	if object.Deferred {
		flags |= objectFlagDeferred
	}
	if object.SourceCatalog != "" || !object.SourceObject.IsZero() {
		flags |= objectFlagSource
	}
	if object.Data != nil {
		flags |= objectFlagData
	}
	if object.Children != nil {
		flags |= objectFlagChildren
	}
	e.objectID(object.ID)
	e.out = append(e.out, flags)
	e.uvarint(object.Refs)
	e.uvarint(object.BulkLength)
	if object.Data != nil {
		e.bytes(object.Data)
	}
	if object.Children != nil {
		e.uvarint(uint64(len(object.Children)))
		for _, edge := range object.Children {
			e.uvarint(uint64(edge.Slot))
			e.objectID(edge.Child)
		}
	}
	if flags&objectFlagSource != 0 {
		e.string(object.SourceCatalog)
		e.objectID(object.SourceObject)
	}
	return nil
}

func (e *graphEncoder) references(references map[string]ObjectID) {
	if references == nil {
		e.uvarint(0)
		return
	}
	e.uvarint(uint64(len(references)) + 1)
	names := make([]string, 0, len(references))
	for name := range references {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		e.string(name)
		e.objectID(references[name])
	}
}

func (e *graphEncoder) referenceMutation(mutation *SnapshotReferenceMutation) {
	e.uvarint(mutation.Sequence)
	e.string(mutation.Operation)
	e.string(mutation.Name)
	e.objectID(mutation.Object)
	e.flag(mutation.Retain)
}

func (e *graphEncoder) handoff(handoff *SnapshotLedgerHandoff) {
	e.uvarint(uint64(handoff.Snapshot))
	e.uvarint(handoff.NextSequence)
	e.flag(handoff.Frozen)
	e.references(handoff.Baseline)
	if handoff.Mutations == nil {
		e.uvarint(0)
		return
	}
	e.uvarint(uint64(len(handoff.Mutations)) + 1)
	for index := range handoff.Mutations {
		e.referenceMutation(&handoff.Mutations[index])
	}
}

func (e *graphEncoder) inode(inode *InodeRecord) {
	e.uvarint(inode.Number)
	e.string(inode.Type)
	e.uvarint(uint64(inode.Mode))
	e.uvarint(uint64(inode.UID))
	e.uvarint(uint64(inode.GID))
	e.uvarint(uint64(inode.Nlink))
	e.varint(inode.Size)
	e.objectID(inode.File)
	e.objectID(inode.Directory)
	e.string(inode.Target)
	e.uvarint(uint64(inode.DeviceMajor))
	e.uvarint(uint64(inode.DeviceMinor))
	e.varint(inode.AtimeNsec)
	e.varint(inode.BtimeNsec)
	e.varint(inode.CtimeNsec)
	e.varint(inode.MtimeNsec)
	if inode.Xattrs == nil {
		e.uvarint(0)
		return
	}
	e.uvarint(uint64(len(inode.Xattrs)) + 1)
	names := make([]string, 0, len(inode.Xattrs))
	for name := range inode.Xattrs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		e.string(name)
		e.nullableBytes(inode.Xattrs[name])
	}
}

func (e *graphEncoder) entry(entry *DirectoryEntryRecord) {
	e.uvarint(entry.Parent)
	e.string(entry.Name)
	e.uvarint(entry.Inode)
}

func (e *graphEncoder) filesystemMutation(mutation *filesystemJournalMutation) {
	e.uvarint(uint64(mutation.Origin))
	e.string(mutation.RootName)
	e.uvarint(uint64(len(mutation.Operations)))
	for index := range mutation.Operations {
		operation := &mutation.Operations[index]
		var flags byte
		if operation.PutInode != nil {
			flags |= fsFlagPutInode
		}
		if operation.DeleteInode != 0 {
			flags |= fsFlagDeleteInode
		}
		if operation.PutEntry != nil {
			flags |= fsFlagPutEntry
		}
		if operation.DeleteEntry != nil {
			flags |= fsFlagDeleteEntry
		}
		e.out = append(e.out, flags)
		if operation.PutInode != nil {
			e.inode(operation.PutInode)
		}
		if operation.DeleteInode != 0 {
			e.uvarint(operation.DeleteInode)
		}
		if operation.PutEntry != nil {
			e.entry(operation.PutEntry)
		}
		if operation.DeleteEntry != nil {
			e.entry(operation.DeleteEntry)
		}
	}
	if mutation.ReleasePins == nil {
		e.uvarint(0)
	} else {
		e.uvarint(uint64(len(mutation.ReleasePins)) + 1)
		for _, pin := range mutation.ReleasePins {
			e.string(pin)
		}
	}
	e.objectID(mutation.Result)
}

func (e *graphEncoder) op(op *journalOp) error {
	switch {
	case op.PutSnapshot != nil:
		e.out = append(e.out, opPutSnapshot)
		return e.snapshot(*op.PutSnapshot)
	case op.DeleteSnapshot != 0:
		e.out = append(e.out, opDeleteSnapshot)
		e.uvarint(uint64(op.DeleteSnapshot))
	case op.PutObject != nil:
		e.out = append(e.out, opPutObject)
		return e.object(op.PutObject)
	case !op.DeleteObject.IsZero():
		e.out = append(e.out, opDeleteObject)
		e.objectID(op.DeleteObject)
	case op.PutRoot != nil:
		e.out = append(e.out, opPutRoot)
		e.string(op.RootName)
		e.objectID(*op.PutRoot)
	case op.DeleteRoot:
		e.out = append(e.out, opDeleteRoot)
		e.string(op.RootName)
	case op.PutPin != nil:
		e.out = append(e.out, opPutPin)
		e.string(op.PinName)
		e.objectID(*op.PutPin)
	case op.DeletePin:
		e.out = append(e.out, opDeletePin)
		e.string(op.PinName)
	case op.SetNextSnapshot != nil:
		e.out = append(e.out, opSetNextSnapshot)
		e.uvarint(uint64(*op.SetNextSnapshot))
	case op.FreezeSnapshot != 0:
		e.out = append(e.out, opFreezeSnapshot)
		e.uvarint(uint64(op.FreezeSnapshot))
	case op.UnfreezeSnapshot != 0:
		e.out = append(e.out, opUnfreezeSnapshot)
		e.uvarint(uint64(op.UnfreezeSnapshot))
	case op.BeginHandoff != nil:
		e.out = append(e.out, opBeginHandoff)
		e.handoff(op.BeginHandoff)
	case op.AppendHandoff != nil:
		e.out = append(e.out, opAppendHandoff)
		e.referenceMutation(op.AppendHandoff)
	case op.ApplyFilesystem != nil:
		if op.ApplyFilesystem.Balanced {
			e.out = append(e.out, opApplyFilesystemBalanced)
		} else {
			e.out = append(e.out, opApplyFilesystem)
		}
		e.filesystemMutation(op.ApplyFilesystem)
	default:
		return errors.New("empty TTC snapshot graph journal operation")
	}
	return nil
}

// encodeJournalRecord encodes record as a journal holds it, without its frame.
func encodeJournalRecord(record *journalRecord) ([]byte, error) {
	size := 16 + 32*len(record.References)
	for index := range record.Ops {
		size += 32
		if object := record.Ops[index].PutObject; object != nil {
			size += len(object.Data) + 16*len(object.Children)
		}
		if mutation := record.Ops[index].ApplyFilesystem; mutation != nil {
			size += 128 * len(mutation.Operations)
		}
	}
	e := graphEncoder{out: make([]byte, 0, size)}
	e.uvarint(record.Sequence)
	e.uvarint(uint64(len(record.Ops)))
	for index := range record.Ops {
		if err := e.op(&record.Ops[index]); err != nil {
			return nil, err
		}
	}
	e.uvarint(uint64(len(record.References)))
	for _, reference := range record.References {
		e.objectID(reference.Object)
		e.uvarint(reference.Refs)
		e.flag(reference.State == ObjectReclaiming)
	}
	return e.out, nil
}

// frameJournalRecord returns data framed as a journal holds a record: its
// length and CRC-32C, then data.
func frameJournalRecord(data []byte) []byte {
	frame := make([]byte, journalFrameBytes+len(data))
	binary.BigEndian.PutUint32(frame[0:4], uint32(len(data)))
	binary.BigEndian.PutUint32(frame[4:8], crc32.Checksum(data, castagnoli))
	copy(frame[journalFrameBytes:], data)
	return frame
}

// journalFrameBytes is the size of a journal record's frame: its length and
// its CRC-32C.
const journalFrameBytes = 8

// encodeGraphCheckpoint encodes state as a checkpoint holds it.
func encodeGraphCheckpoint(state *graphState) ([]byte, error) {
	size, dataBytes, edges := len(checkpointMagic)+64, 0, 0
	for _, object := range state.Objects {
		dataBytes += len(object.Data)
		edges += len(object.Children)
		size += 32 + len(object.Data) + 16*len(object.Children) + len(object.SourceCatalog)
	}
	e := graphEncoder{out: make([]byte, 0, size)}
	e.out = append(e.out, checkpointMagic...)
	// What the objects hold in all, for the decoder to allocate at once.
	e.uvarint(uint64(len(state.Objects)))
	e.uvarint(uint64(dataBytes))
	e.uvarint(uint64(edges))
	e.uvarint(state.Sequence)
	e.uvarint(uint64(state.NextSnapshot))

	snapshots := make([]Snapshot, 0, len(state.Snapshots))
	for _, snapshot := range state.Snapshots {
		snapshots = append(snapshots, snapshot)
	}
	slices.SortFunc(snapshots, func(a, b Snapshot) int { return cmp.Compare(a.ID, b.ID) })
	e.uvarint(uint64(len(snapshots)))
	for _, snapshot := range snapshots {
		if err := e.snapshot(snapshot); err != nil {
			return nil, err
		}
	}
	for _, object := range state.Objects {
		if err := e.object(&object); err != nil {
			return nil, err
		}
	}
	for _, table := range []map[string]ObjectID{state.Roots, state.Pins} {
		names := make([]string, 0, len(table))
		for name := range table {
			names = append(names, name)
		}
		sort.Strings(names)
		e.uvarint(uint64(len(names)))
		for _, name := range names {
			e.string(name)
			e.objectID(table[name])
		}
	}
	frozen := make([]SnapshotID, 0, len(state.Frozen))
	for id, value := range state.Frozen {
		if !value {
			return nil, fmt.Errorf("snapshot reference barrier %d is false", id)
		}
		frozen = append(frozen, id)
	}
	slices.Sort(frozen)
	e.uvarint(uint64(len(frozen)))
	for _, id := range frozen {
		e.uvarint(uint64(id))
	}
	handoffs := make([]SnapshotLedgerHandoff, 0, len(state.Handoffs))
	for _, handoff := range state.Handoffs {
		handoffs = append(handoffs, handoff)
	}
	slices.SortFunc(handoffs, func(a, b SnapshotLedgerHandoff) int { return cmp.Compare(a.Snapshot, b.Snapshot) })
	e.uvarint(uint64(len(handoffs)))
	for index := range handoffs {
		e.handoff(&handoffs[index])
	}
	return binary.BigEndian.AppendUint32(e.out, crc32.Checksum(e.out, castagnoli)), nil
}

type graphDecoder struct {
	data []byte
	pos  int
	err  error
	// data and edges, if not nil, are where the objects decoded take their
	// bytes and edges from, in turn.
	dataArena []byte
	edgeArena []Edge
}

func (d *graphDecoder) fail(format string, args ...any) {
	if d.err == nil {
		d.err = fmt.Errorf("at byte %d: %s", d.pos, fmt.Sprintf(format, args...))
	}
}

func (d *graphDecoder) uvarint() uint64 {
	if d.err != nil {
		return 0
	}
	value, n := binary.Uvarint(d.data[d.pos:])
	if n <= 0 {
		d.fail("truncated or overflowing integer")
		return 0
	}
	d.pos += n
	return value
}

func (d *graphDecoder) varint() int64 {
	if d.err != nil {
		return 0
	}
	value, n := binary.Varint(d.data[d.pos:])
	if n <= 0 {
		d.fail("truncated or overflowing integer")
		return 0
	}
	d.pos += n
	return value
}

// integer decodes an unsigned integer of at most bits bits.
func (d *graphDecoder) integer(bits uint) uint64 {
	value := d.uvarint()
	if bits < 64 && value>>bits != 0 {
		d.fail("integer %d exceeds %d bits", value, bits)
		return 0
	}
	return value
}

func (d *graphDecoder) byte() byte {
	if d.err != nil {
		return 0
	}
	if d.pos >= len(d.data) {
		d.fail("truncated")
		return 0
	}
	value := d.data[d.pos]
	d.pos++
	return value
}

func (d *graphDecoder) flag() bool {
	switch d.byte() {
	case 0:
		return false
	case 1:
		return true
	}
	d.fail("invalid flag")
	return false
}

// count decodes the length of a list whose elements each take at least one
// byte, which the rest of the data must hold.
func (d *graphDecoder) count() int {
	value := d.uvarint()
	if value > uint64(len(d.data)-d.pos) {
		d.fail("count %d exceeds the data", value)
		return 0
	}
	return int(value)
}

// nullableCount decodes the length of a list that may be nil, which it
// reports as false.
func (d *graphDecoder) nullableCount() (int, bool) {
	value := d.uvarint()
	if value == 0 {
		return 0, false
	}
	if value-1 > uint64(len(d.data)-d.pos) {
		d.fail("count %d exceeds the data", value-1)
		return 0, false
	}
	return int(value - 1), true
}

// take returns the next n bytes, which alias the data.
func (d *graphDecoder) take(n uint64) []byte {
	if d.err != nil {
		return nil
	}
	if n > uint64(len(d.data)-d.pos) {
		d.fail("length %d exceeds the data", n)
		return nil
	}
	value := d.data[d.pos : d.pos+int(n)]
	d.pos += int(n)
	return value
}

func (d *graphDecoder) string() string {
	return string(d.take(d.uvarint()))
}

// bytes decodes a byte string into memory of its own, or of the data arena.
func (d *graphDecoder) bytes() []byte {
	n := d.uvarint()
	source := d.take(n)
	if d.err != nil {
		return nil
	}
	if n == 0 {
		return []byte{}
	}
	if d.dataArena == nil {
		return slices.Clone(source)
	}
	if n > uint64(len(d.dataArena)) {
		d.fail("objects hold more bytes than the checkpoint declares")
		return nil
	}
	value := d.dataArena[:n:n]
	d.dataArena = d.dataArena[n:]
	copy(value, source)
	return value
}

func (d *graphDecoder) nullableBytes() []byte {
	n := d.uvarint()
	if n == 0 {
		return nil
	}
	source := d.take(n - 1)
	if d.err != nil {
		return nil
	}
	return append([]byte{}, source...)
}

func (d *graphDecoder) objectID() ObjectID {
	snapshot := d.uvarint()
	kind := d.byte()
	ordinal := d.uvarint()
	return ObjectID{Snapshot: SnapshotID(snapshot), Kind: ObjectKind(kind), Ordinal: ordinal}
}

func (d *graphDecoder) snapshot() Snapshot {
	id := d.uvarint()
	state := d.byte()
	next := d.uvarint()
	if int(state) >= len(snapshotStates) {
		d.fail("invalid snapshot state %d", state)
		return Snapshot{}
	}
	return Snapshot{ID: SnapshotID(id), State: snapshotStates[state], NextOrdinal: next}
}

func (d *graphDecoder) object() Object {
	var object Object
	object.ID = d.objectID()
	flags := d.byte()
	if flags&^objectFlags != 0 {
		d.fail("invalid object flags %#x", flags)
		return Object{}
	}
	object.State = ObjectLive
	if flags&objectFlagReclaiming != 0 {
		object.State = ObjectReclaiming
	}
	object.Deferred = flags&objectFlagDeferred != 0
	object.Refs = d.uvarint()
	object.BulkLength = d.uvarint()
	if flags&objectFlagData != 0 {
		object.Data = d.bytes()
	}
	if flags&objectFlagChildren != 0 {
		// An edge takes at least three bytes.
		count := d.count()
		if d.edgeArena == nil {
			object.Children = make([]Edge, count)
		} else if count > len(d.edgeArena) {
			d.fail("objects hold more edges than the checkpoint declares")
			return Object{}
		} else {
			object.Children = d.edgeArena[:count:count]
			d.edgeArena = d.edgeArena[count:]
		}
		for index := range object.Children {
			object.Children[index] = Edge{Slot: uint16(d.integer(16)), Child: d.objectID()}
		}
	}
	if flags&objectFlagSource != 0 {
		object.SourceCatalog = d.string()
		object.SourceObject = d.objectID()
	}
	return object
}

func (d *graphDecoder) references() map[string]ObjectID {
	count, ok := d.nullableCount()
	if !ok {
		return nil
	}
	references := make(map[string]ObjectID, count)
	for index := 0; index < count && d.err == nil; index++ {
		name := d.string()
		references[name] = d.objectID()
	}
	return references
}

func (d *graphDecoder) referenceMutation() SnapshotReferenceMutation {
	var mutation SnapshotReferenceMutation
	mutation.Sequence = d.uvarint()
	mutation.Operation = d.string()
	mutation.Name = d.string()
	mutation.Object = d.objectID()
	mutation.Retain = d.flag()
	return mutation
}

func (d *graphDecoder) handoff() SnapshotLedgerHandoff {
	var handoff SnapshotLedgerHandoff
	handoff.Snapshot = SnapshotID(d.uvarint())
	handoff.NextSequence = d.uvarint()
	handoff.Frozen = d.flag()
	handoff.Baseline = d.references()
	if count, ok := d.nullableCount(); ok {
		handoff.Mutations = make([]SnapshotReferenceMutation, 0, count)
		for index := 0; index < count && d.err == nil; index++ {
			handoff.Mutations = append(handoff.Mutations, d.referenceMutation())
		}
	}
	return handoff
}

func (d *graphDecoder) inode() *InodeRecord {
	inode := &InodeRecord{}
	d.inodeRecord(inode)
	return inode
}

// inodeTypes are the types of inodes, which a decoded record shares, rather
// than a string of its own.
var inodeTypes = [...]string{"regular", "directory", "symlink", "fifo", "char", "block", "socket"}

func (d *graphDecoder) inodeType() string {
	value := d.take(d.uvarint())
	for _, known := range inodeTypes {
		if string(value) == known {
			return known
		}
	}
	return string(value)
}

func (d *graphDecoder) inodeRecord(inode *InodeRecord) {
	inode.Number = d.uvarint()
	inode.Type = d.inodeType()
	inode.Mode = uint32(d.integer(32))
	inode.UID = uint32(d.integer(32))
	inode.GID = uint32(d.integer(32))
	inode.Nlink = uint32(d.integer(32))
	inode.Size = d.varint()
	inode.File = d.objectID()
	inode.Directory = d.objectID()
	inode.Target = d.string()
	inode.DeviceMajor = uint32(d.integer(32))
	inode.DeviceMinor = uint32(d.integer(32))
	inode.AtimeNsec = d.varint()
	inode.BtimeNsec = d.varint()
	inode.CtimeNsec = d.varint()
	inode.MtimeNsec = d.varint()
	if count, ok := d.nullableCount(); ok {
		inode.Xattrs = make(map[string][]byte, count)
		for index := 0; index < count && d.err == nil; index++ {
			name := d.string()
			inode.Xattrs[name] = d.nullableBytes()
		}
	}
}

func (d *graphDecoder) entry() *DirectoryEntryRecord {
	parent := d.uvarint()
	name := d.string()
	inode := d.uvarint()
	return &DirectoryEntryRecord{Parent: parent, Name: name, Inode: inode}
}

func (d *graphDecoder) filesystemMutation() *filesystemJournalMutation {
	mutation := &filesystemJournalMutation{}
	mutation.Origin = SnapshotID(d.uvarint())
	mutation.RootName = d.string()
	count := d.count()
	mutation.Operations = make([]FilesystemOperation, count)
	for index := 0; index < count && d.err == nil; index++ {
		operation := &mutation.Operations[index]
		flags := d.byte()
		if flags&^fsFlags != 0 {
			d.fail("invalid filesystem operation flags %#x", flags)
			return nil
		}
		if flags&fsFlagPutInode != 0 {
			operation.PutInode = d.inode()
		}
		if flags&fsFlagDeleteInode != 0 {
			if operation.DeleteInode = d.uvarint(); operation.DeleteInode == 0 {
				d.fail("filesystem operation deletes inode 0")
			}
		}
		if flags&fsFlagPutEntry != 0 {
			operation.PutEntry = d.entry()
		}
		if flags&fsFlagDeleteEntry != 0 {
			operation.DeleteEntry = d.entry()
		}
	}
	if count, ok := d.nullableCount(); ok {
		mutation.ReleasePins = make([]string, 0, count)
		for index := 0; index < count && d.err == nil; index++ {
			mutation.ReleasePins = append(mutation.ReleasePins, d.string())
		}
	}
	mutation.Result = d.objectID()
	return mutation
}

func (d *graphDecoder) op() journalOp {
	var op journalOp
	switch code := d.byte(); code {
	case opPutSnapshot:
		snapshot := d.snapshot()
		op.PutSnapshot = &snapshot
	case opDeleteSnapshot:
		if op.DeleteSnapshot = SnapshotID(d.uvarint()); op.DeleteSnapshot == 0 {
			d.fail("operation deletes snapshot 0")
		}
	case opPutObject:
		object := d.object()
		op.PutObject = &object
	case opDeleteObject:
		if op.DeleteObject = d.objectID(); op.DeleteObject.IsZero() {
			d.fail("operation deletes no object")
		}
	case opPutRoot:
		op.RootName = d.string()
		root := d.objectID()
		op.PutRoot = &root
	case opDeleteRoot:
		op.RootName = d.string()
		op.DeleteRoot = true
	case opPutPin:
		op.PinName = d.string()
		pin := d.objectID()
		op.PutPin = &pin
	case opDeletePin:
		op.PinName = d.string()
		op.DeletePin = true
	case opSetNextSnapshot:
		next := SnapshotID(d.uvarint())
		op.SetNextSnapshot = &next
	case opFreezeSnapshot:
		if op.FreezeSnapshot = SnapshotID(d.uvarint()); op.FreezeSnapshot == 0 {
			d.fail("operation freezes snapshot 0")
		}
	case opUnfreezeSnapshot:
		if op.UnfreezeSnapshot = SnapshotID(d.uvarint()); op.UnfreezeSnapshot == 0 {
			d.fail("operation unfreezes snapshot 0")
		}
	case opBeginHandoff:
		handoff := d.handoff()
		op.BeginHandoff = &handoff
	case opAppendHandoff:
		mutation := d.referenceMutation()
		op.AppendHandoff = &mutation
	case opApplyFilesystem, opApplyFilesystemBalanced:
		op.ApplyFilesystem = d.filesystemMutation()
		if op.ApplyFilesystem != nil {
			op.ApplyFilesystem.Balanced = code == opApplyFilesystemBalanced
		}
	default:
		d.fail("invalid journal operation %d", code)
	}
	return op
}

// decodeJournalRecord decodes a journal record without its frame.
func decodeJournalRecord(data []byte) (journalRecord, error) {
	d := graphDecoder{data: data}
	var record journalRecord
	record.Sequence = d.uvarint()
	if count := d.count(); d.err == nil {
		record.Ops = make([]journalOp, 0, count)
		for index := 0; index < count && d.err == nil; index++ {
			record.Ops = append(record.Ops, d.op())
		}
	}
	if count := d.count(); d.err == nil && count != 0 {
		record.References = make([]journalReference, 0, count)
		for index := 0; index < count && d.err == nil; index++ {
			reference := journalReference{Object: d.objectID(), Refs: d.uvarint(), State: ObjectLive}
			if d.flag() {
				reference.State = ObjectReclaiming
			}
			record.References = append(record.References, reference)
		}
	}
	if d.err == nil && d.pos != len(data) {
		d.fail("trailing bytes")
	}
	if d.err != nil {
		return journalRecord{}, fmt.Errorf("decoding TTC snapshot graph journal record: %w", d.err)
	}
	return record, nil
}

// decodeGraphCheckpoint decodes a checkpoint.
func decodeGraphCheckpoint(data []byte) (graphState, error) {
	if len(data) < len(checkpointMagic)+4 || string(data[:len(checkpointMagic)]) != checkpointMagic {
		return graphState{}, errors.New("not a TTC snapshot graph checkpoint")
	}
	body := data[:len(data)-4]
	if binary.BigEndian.Uint32(data[len(body):]) != crc32.Checksum(body, castagnoli) {
		return graphState{}, errors.New("TTC snapshot graph checkpoint fails its checksum")
	}
	d := graphDecoder{data: body, pos: len(checkpointMagic)}
	objects := d.count()
	// Each byte and edge of the objects takes at least one byte.
	dataBytes := d.count()
	edges := d.count()
	state := graphState{Version: formatVersion}
	state.Sequence = d.uvarint()
	state.NextSnapshot = SnapshotID(d.uvarint())
	count := d.count()
	if d.err != nil {
		return graphState{}, fmt.Errorf("decoding TTC snapshot graph checkpoint: %w", d.err)
	}
	state.Snapshots = make(map[SnapshotID]Snapshot, count)
	for index := 0; index < count && d.err == nil; index++ {
		snapshot := d.snapshot()
		state.Snapshots[snapshot.ID] = snapshot
	}
	d.dataArena = make([]byte, dataBytes)
	d.edgeArena = make([]Edge, edges)
	state.Objects = make(map[ObjectID]Object, objects)
	for index := 0; index < objects && d.err == nil; index++ {
		object := d.object()
		state.Objects[object.ID] = object
	}
	if d.err == nil && len(state.Objects) != objects {
		d.fail("the checkpoint repeats objects")
	}
	state.Roots, state.Pins = d.names(), d.names()
	count = d.count()
	state.Frozen = make(map[SnapshotID]bool, count)
	for index := 0; index < count && d.err == nil; index++ {
		state.Frozen[SnapshotID(d.uvarint())] = true
	}
	count = d.count()
	state.Handoffs = make(map[SnapshotID]SnapshotLedgerHandoff, count)
	for index := 0; index < count && d.err == nil; index++ {
		handoff := d.handoff()
		state.Handoffs[handoff.Snapshot] = handoff
	}
	if d.err == nil && (d.pos != len(body) || len(d.dataArena) != 0 || len(d.edgeArena) != 0) {
		d.fail("the checkpoint holds other than it declares")
	}
	if d.err != nil {
		return graphState{}, fmt.Errorf("decoding TTC snapshot graph checkpoint: %w", d.err)
	}
	return state, nil
}

// names decodes a table of roots or pins.
func (d *graphDecoder) names() map[string]ObjectID {
	count := d.count()
	names := make(map[string]ObjectID, count)
	for index := 0; index < count && d.err == nil; index++ {
		name := d.string()
		names[name] = d.objectID()
	}
	return names
}

// The data of an inode's object, and of a directory entry's, is its record:
// recordMagic, then the record as a journal holds it. A record that begins
// with '{' is JSON, as an earlier release wrote it. As JSON, a regular
// file's inode record took some 220 bytes, and decoding one 3 us.
const recordMagic = 0x01

func appendInodeRecord(out []byte, inode *InodeRecord) []byte {
	e := graphEncoder{out: append(out, recordMagic)}
	e.inode(inode)
	return e.out
}

func appendDirectoryEntryRecord(out []byte, entry *DirectoryEntryRecord) []byte {
	e := graphEncoder{out: append(out, recordMagic)}
	e.entry(entry)
	return e.out
}

func decodeBinaryInodeRecord(data []byte) (InodeRecord, error) {
	d := graphDecoder{data: data, pos: 1}
	var inode InodeRecord
	d.inodeRecord(&inode)
	if d.err == nil && d.pos != len(data) {
		d.fail("trailing bytes")
	}
	if d.err != nil {
		return InodeRecord{}, fmt.Errorf("decoding TTC inode record: %w", d.err)
	}
	return inode, nil
}

func decodeBinaryDirectoryEntryRecord(data []byte) (DirectoryEntryRecord, error) {
	d := graphDecoder{data: data, pos: 1}
	entry := DirectoryEntryRecord{Parent: d.uvarint(), Name: d.string(), Inode: d.uvarint()}
	if d.err == nil && d.pos != len(data) {
		d.fail("trailing bytes")
	}
	if d.err != nil {
		return DirectoryEntryRecord{}, fmt.Errorf("decoding TTC directory entry record: %w", d.err)
	}
	return entry, nil
}
