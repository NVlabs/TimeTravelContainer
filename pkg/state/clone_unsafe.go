// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package state

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"
	"unsafe"
)

// CloneStats is what a Clone did.
type CloneStats struct {
	// Objects and Structs are the numbers of objects and structs that the
	// clone copied.
	Objects int
	Structs int
	// Record is how long the clone took to record the source's graph, and
	// Copy how long to copy it, its callbacks included.
	Record time.Duration
	Copy   time.Duration
}

// Clone makes the object graph of dstPtr a copy of that of srcPtr, as Save
// of srcPtr with saveCtx and then Load into dstPtr with loadCtx would, with
// neither's encoding: a live fork's child is a copy of its parent, in the
// same process, which needs no serialized form.
//
// Like Save, Clone calls the source's StateSave methods, once for each
// struct, takes what they save when they save it, and resolves the objects
// of the graph by their address ranges, so that pointers into objects keep
// pointing into the copies. Like Load, it calls the copies' StateLoad
// methods, which take what the source's StateSave methods saved, and runs
// the callbacks that they register in the order that their dependencies
// require. Values are copied as the encoder and the decoder would: fields
// that are zero are left as the destination has them, and strings, which
// are immutable, are shared.
//
// The source must not change during the call but for what its StateSave
// methods change.
func Clone(saveCtx, loadCtx context.Context, srcPtr, dstPtr any, opts SaveOptions) (CloneStats, error) {
	r, err := Record(saveCtx, srcPtr, opts)
	if err != nil {
		return CloneStats{}, err
	}
	return r.copy(loadCtx, dstPtr, true /* adopt */)
}

// Recording is what Record recorded of an object graph, which Copy copies,
// once or more, concurrently. It holds what the source's StateSave methods
// saved as it was then: the source may change afterward.
type Recording struct {
	cs     *cloneState
	record time.Duration
}

// Record records the object graph of srcPtr, with saveCtx, as Save would
// encode it, for Copy.
func Record(saveCtx context.Context, srcPtr any, opts SaveOptions) (*Recording, error) {
	src := reflect.ValueOf(srcPtr).Elem()
	cs := &cloneState{
		saveCtx:                   saveCtx,
		zeroValues:                make(map[reflect.Type]*objectEncodeState),
		structs:                   make(map[uintptr]*structRecord),
		types:                     make(map[uintptr]*cloneTypeInfo),
		ifaceCopies:               make(map[uintptr]reflect.Value),
		rootAddr:                  src.Addr().Pointer(),
		rootType:                  src.Type(),
		rejectMultipleRootObjects: opts.RejectMultipleRootObjects,
	}
	start := time.Now()
	if err := safely(func() { cs.record(src.Addr()) }); err != nil {
		return nil, err
	}
	// Recording's own state goes with the recording: copies need none of
	// it.
	cs.recorders, cs.recordSlab, cs.slotSlab, cs.oesSlab, cs.cloneSlab = nil, nil, nil, nil, nil
	cs.recorded = true
	return &Recording{cs: cs, record: time.Since(start)}, nil
}

// Copy makes the object graph of dstPtr a copy of the recording, with
// loadCtx, as Load of what Save encoded would.
func (r *Recording) Copy(loadCtx context.Context, dstPtr any) (CloneStats, error) {
	return r.copy(loadCtx, dstPtr, false /* adopt */)
}

// copy makes dstPtr's object graph a copy of the recording. If adopt is
// set, no other copy is made of the recording, and the copy may take the
// recording's snapshots of objects that refer to nothing as its own.
func (r *Recording) copy(loadCtx context.Context, dstPtr any, adopt bool) (stats CloneStats, err error) {
	cs := r.cs
	dst := reflect.ValueOf(dstPtr).Elem()
	if dst.Type() != cs.rootType {
		return stats, fmt.Errorf("copying a %v into a %v", cs.rootType, dst.Type())
	}
	run := &copyRun{cs: cs, ctx: loadCtx, adopt: adopt, objects: make([]copyObject, len(cs.objects))}
	start := time.Now()
	err = safely(func() {
		stats.Objects = run.copyGraph(dst)
	})
	stats.Record = r.record
	stats.Copy = time.Since(start)
	stats.Structs = len(cs.structs)
	return stats, err
}

// cloneState is the state of a Clone: what it recorded.
type cloneState struct {
	saveCtx context.Context

	// lastID is the last object ID allocated. The root is object 1.
	lastID objectID

	// values, zeroValues, and deferred are as encodeState's: the objects of
	// the source's graph, by their address ranges, and those that remain
	// to be recorded.
	values     addrSet
	zeroValues map[reflect.Type]*objectEncodeState
	deferred   deferredList

	// objects are the objects of the source's graph, by ID. An object that
	// a containing object replaced is nil.
	objects []*objectEncodeState

	// structs are what the StateSave methods of the source's structs saved,
	// by the structs' addresses, of structs of different types each.
	structs map[uintptr]*structRecord

	// types caches what the clone needs to know of types, by rtypeOf. Only
	// the recording adds to it, before any copy reads it; copies, which may
	// run at once, read it without a lock, and add the types it lacks to
	// copyTypes, under copyTypesMu. Read under an RWMutex, it took an
	// eighth of a small branch's fork, in the lock's atomics.
	types       map[uintptr]*cloneTypeInfo
	recorded    bool
	copyTypesMu sync.Mutex
	copyTypes   map[uintptr]*cloneTypeInfo

	// ifaceCopies are addressable copies of the structs and arrays that
	// interfaces hold, by the addresses of the interfaces' recorded
	// values.
	ifaceCopies map[uintptr]reflect.Value

	// recorders are reused by the depth of structs' StateSave methods'
	// calls.
	recorders []*cloneRecorder

	// recordSlab and slotSlab allocate structs' records in chunks.
	recordSlab []structRecord
	slotSlab   []reflect.Value

	// oesSlab and cloneSlab allocate objects' states in chunks.
	oesSlab   []objectEncodeState
	cloneSlab []cloneObject

	rootAddr                  uintptr
	rootType                  reflect.Type
	rejectMultipleRootObjects bool
}

// structRecord is what a struct's StateSave method saved, by slot: values
// as they were when it saved them, or structs that hold structs, whose own
// records are what they saved.
type structRecord struct {
	slots []reflect.Value
	// typ is the struct's type's rtypeOf. A struct and its first field,
	// a struct of another type, share an address: next is the record of
	// another struct at the same address.
	typ  uintptr
	next *structRecord
	// empty is whether the struct is an anonymous empty struct, which
	// saves nothing.
	empty bool
}

// rtypeOf identifies a type as reflect.Type does, without comparing
// interfaces: the type's runtime type.
func rtypeOf(t reflect.Type) uintptr {
	return (*[2]uintptr)(unsafe.Pointer(&t))[1]
}

// lookupStruct returns the record of the struct at addr of the type of
// rtypeOf tid, if the clone recorded it.
func (cs *cloneState) lookupStruct(addr, tid uintptr) *structRecord {
	for r := cs.structs[addr]; r != nil; r = r.next {
		if r.typ == tid {
			return r
		}
	}
	return nil
}

// cloneTypeInfo is what the clone knows of a type.
type cloneTypeInfo struct {
	// structs is whether values of the type hold structs, whose StateSave
	// methods record them.
	structs bool
	// refs is whether values of the type refer to objects: pointers,
	// slices, maps, and interfaces.
	refs bool
	// registered is whether a struct type has state type information, and
	// fields its number of fields.
	registered bool
	fields     int
	// primitiveZero is isPrimitiveZero of the type, and plain whether its
	// values are primitives that hold no pointers, which the clone copies
	// as bytes.
	primitiveZero bool
	plain         bool
	size          uintptr
	// slots caches what the clone knows of the types of the values that a
	// struct type's StateSave method saves, by slot.
	slots []slotType
}

// slotType is what the clone knows of the type of a slot's value, of
// rtypeOf typ.
type slotType struct {
	typ uintptr
	ti  *cloneTypeInfo
}

// typeInfo returns what the clone knows of typ.
func (cs *cloneState) typeInfo(typ reflect.Type) *cloneTypeInfo {
	tid := rtypeOf(typ)
	if ti, ok := cs.types[tid]; ok {
		return ti
	}
	if !cs.recorded {
		ti := cs.newTypeInfo(typ)
		cs.types[tid] = ti
		return ti
	}
	cs.copyTypesMu.Lock()
	ti, ok := cs.copyTypes[tid]
	cs.copyTypesMu.Unlock()
	if ok {
		return ti
	}
	ti = cs.newTypeInfo(typ)
	cs.copyTypesMu.Lock()
	defer cs.copyTypesMu.Unlock()
	if other, ok := cs.copyTypes[tid]; ok {
		return other
	}
	if cs.copyTypes == nil {
		cs.copyTypes = make(map[uintptr]*cloneTypeInfo)
	}
	cs.copyTypes[tid] = ti
	return ti
}

// newTypeInfo returns what the clone needs to know of typ.
func (cs *cloneState) newTypeInfo(typ reflect.Type) *cloneTypeInfo {
	ti := &cloneTypeInfo{size: typ.Size()}
	switch k := typ.Kind(); k {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128:
		ti.plain = true
	}
	switch k := typ.Kind(); k {
	case reflect.Func, reflect.Chan, reflect.UnsafePointer:
		// isPrimitiveZero fails on these, as the encoder does.
	default:
		ti.primitiveZero = isPrimitiveZero(typ)
	}
	switch typ.Kind() {
	case reflect.Ptr, reflect.Slice, reflect.Map, reflect.Interface:
		ti.refs = true
	case reflect.Struct:
		ti.structs = true
		fields := cloneStructFieldsOf(typ)
		ti.registered = fields.registered
		ti.fields = fields.n
		ti.slots = make([]slotType, fields.n)
	case reflect.Array:
		elem := cs.typeInfo(typ.Elem())
		ti.structs, ti.refs = elem.structs, elem.refs
	}
	return ti
}

// cloneStructFields holds, by rtypeOf, the cloneStructFieldsOf each struct
// type that a clone met. Each clone learns its types anew, and looking up
// each type's state type information was half of that, a twentieth of a
// small branch's fork.
var cloneStructFields sync.Map

// cloneStructFieldsOf returns whether typ, a struct type, has state type
// information, and its number of fields.
func cloneStructFieldsOf(typ reflect.Type) cloneStructInfo {
	tid := rtypeOf(typ)
	if info, ok := cloneStructFields.Load(tid); ok {
		return info.(cloneStructInfo)
	}
	typeInfo, ok := lookupTypeInfo(typ)
	info := cloneStructInfo{registered: ok, n: len(typeInfo.Fields)}
	cloneStructFields.Store(tid, info)
	return info
}

// cloneStructInfo is what cloneStructFieldsOf returns.
type cloneStructInfo struct {
	registered bool
	n          int
}

// cloneObject is what the clone recorded of an object of the source's
// graph that its structs' records do not hold.
type cloneObject struct {
	// snapshot is a copy of an object that holds no structs, as it was when
	// the clone recorded it.
	snapshot reflect.Value
	// keys and values are a map's entries, addressable copies.
	keys, values []reflect.Value
}

// copyRun is a copy of a recording.
type copyRun struct {
	cs  *cloneState
	ctx context.Context
	// adopt is whether the copy may take the recording's snapshots of
	// objects that refer to nothing as its own.
	adopt bool
	// objects are the copies of the recording's objects, by ID.
	objects []copyObject
	// loaders are reused by the depth of structs' StateLoad methods' calls.
	loaders []*cloneLoader
}

// copyObject is the copy of an object of the recording.
type copyObject struct {
	// clone is the copy: an addressable value of the object's type, or,
	// for a map, the new map.
	clone reflect.Value
	// base is the address of the copy, unless it is a map.
	base unsafe.Pointer

	// blockedBy and callbacks are as objectDecodeState's: the number of
	// objects that the copy waits for, and what runs once it is complete,
	// in order.
	blockedBy int
	callbacks []cloneCallback
	done      bool
}

// object returns the copy of object oes.
func (r *copyRun) object(oes *objectEncodeState) *copyObject {
	return &r.objects[oes.id-1]
}

// cloneCallback runs fn, or unblocks waiter.
type cloneCallback struct {
	fn     func()
	waiter *objectEncodeState
}

// slabSize is the number of objects' states that the clone allocates at
// once.
const slabSize = 256

func (cs *cloneState) newObjectEncodeState(obj reflect.Value, how encodeStrategy) *objectEncodeState {
	if len(cs.oesSlab) == 0 {
		cs.oesSlab = make([]objectEncodeState, slabSize)
	}
	oes := &cs.oesSlab[0]
	cs.oesSlab = cs.oesSlab[1:]
	oes.obj, oes.how = obj, how
	return oes
}

func (cs *cloneState) newCloneObject() *cloneObject {
	if len(cs.cloneSlab) == 0 {
		cs.cloneSlab = make([]cloneObject, slabSize)
	}
	cl := &cs.cloneSlab[0]
	cs.cloneSlab = cs.cloneSlab[1:]
	return cl
}

// addObject registers oes, which takes ID id, to be recorded.
func (cs *cloneState) addObject(oes *objectEncodeState, id objectID) {
	oes.id = id
	for int(id) > len(cs.objects) {
		cs.objects = append(cs.objects, nil)
	}
	cs.objects[id-1] = oes
	cs.deferred.PushBack(oes)
}

func (cs *cloneState) nextID() objectID {
	cs.lastID++
	return cs.lastID
}

// resolve registers the object that obj, a pointer or a map, refers to, as
// encodeState.resolve does.
func (cs *cloneState) resolve(obj reflect.Value) {
	addr := obj.Pointer()
	if obj.Kind() == reflect.Map {
		seg, gap := cs.values.Find(addr)
		if seg.Ok() {
			if seg.Value().obj.Type() != obj.Type() {
				Failf("overlapping map objects at 0x%x: [new object] %#v [existing object type] %s", addr, obj, seg.Value().obj)
			}
			return
		}
		oes := cs.newObjectEncodeState(obj, encodeMapAsValue)
		cs.values.InsertWithoutMergingUnchecked(gap, addrRange{addr, addr + 1}, oes)
		cs.addObject(oes, cs.nextID())
		return
	}
	if obj.Kind() != reflect.Ptr {
		Failf("attempt to record non-map and non-pointer object %#v", obj)
	}
	if cs.rejectMultipleRootObjects && obj.Type().Elem() == cs.rootType && addr != cs.rootAddr {
		Failf("object graph crossed root boundary: root is %s at %#x, reached distinct %s at %#x", cs.rootType, cs.rootAddr, obj.Type().Elem(), addr)
	}
	obj = obj.Elem()
	typ := obj.Type()
	size := typ.Size()
	if size == 0 {
		if addr == dummyAddr {
			// A true zero-sized object; see encodeState.resolve.
			if _, ok := cs.zeroValues[typ]; !ok {
				oes := cs.newObjectEncodeState(obj, encodeDefault)
				cs.zeroValues[typ] = oes
				cs.addObject(oes, cs.nextID())
			}
			return
		}
		size = 1
	}
	end := addr + size
	r := addrRange{addr, end}
	seg := cs.values.LowerBoundSegment(addr)
	if seg.Ok() && seg.Start() < end {
		existing := seg.Value()
		if seg.Range() == r && typ == existing.obj.Type() {
			// Registered already.
			return
		}
		if seg.Range().IsSupersetOf(r) && (seg.Range() != r || isSameSizeParent(existing.obj, typ)) {
			// Within a registered object.
			return
		}
		// The object contains registered objects, which it replaces,
		// taking the ID of the first.
		oes := cs.newObjectEncodeState(obj, encodeDefault)
		id := existing.id
		var gap addrGapIterator
		for {
			if raceEnabled && !r.IsSupersetOf(seg.Range()) {
				Failf("containing object %#v does not contain existing object %#v", obj, existing.obj)
			}
			cs.objects[existing.id-1] = nil
			cs.deferred.Remove(existing)
			gap = cs.values.Remove(seg)
			seg = gap.NextSegment()
			if !seg.Ok() || seg.Start() >= end {
				break
			}
			existing = seg.Value()
		}
		cs.values.InsertWithoutMergingUnchecked(gap, r, oes)
		cs.addObject(oes, id)
		return
	}
	oes := cs.newObjectEncodeState(obj, encodeDefault)
	var gap addrGapIterator
	if seg.Ok() {
		gap = seg.PrevGap()
	} else {
		gap = cs.values.LastGap()
	}
	cs.values.InsertWithoutMergingUnchecked(gap, r, oes)
	cs.addObject(oes, cs.nextID())
}

// record records the graph of root, a pointer to the source's root, which
// becomes object 1, as encodeState.Save encodes it: each object once it
// takes it from the queue of those that it registered.
func (cs *cloneState) record(root reflect.Value) {
	cs.resolve(root)
	for oes := cs.deferred.Front(); oes != nil; oes = cs.deferred.Front() {
		cs.deferred.Remove(oes)
		cs.recordObject(oes)
	}
}

// recordObject records an object.
func (cs *cloneState) recordObject(oes *objectEncodeState) {
	cl := cs.newCloneObject()
	oes.clone = cl
	obj := oes.obj
	if oes.how == encodeMapAsValue {
		iter := obj.MapRange()
		for iter.Next() {
			k := addressable(iter.Key())
			v := addressable(iter.Value())
			cl.keys = append(cl.keys, k)
			cl.values = append(cl.values, v)
			cs.visit(k)
			cs.visit(v)
		}
		return
	}
	ti := cs.typeInfo(obj.Type())
	if ti.structs {
		// Its structs' StateSave methods record it.
		cs.visit(obj)
		return
	}
	cl.snapshot = reflect.New(obj.Type()).Elem()
	cl.snapshot.Set(obj)
	if ti.refs {
		cs.visit(cl.snapshot)
	}
}

// addressable returns obj, or an addressable copy of it.
func addressable(obj reflect.Value) reflect.Value {
	if obj.CanAddr() {
		return obj
	}
	c := reflect.New(obj.Type()).Elem()
	c.Set(obj)
	return c
}

// visit records what obj, an addressable value, refers to and holds.
func (cs *cloneState) visit(obj reflect.Value) {
	switch obj.Kind() {
	case reflect.Ptr:
		if !obj.IsNil() {
			cs.resolve(obj)
		}
	case reflect.Slice:
		if !obj.IsNil() && obj.Cap() != 0 {
			cs.resolve(arrayFromSlice(obj))
		}
	case reflect.Map:
		if !obj.IsNil() {
			cs.resolve(obj)
		}
	case reflect.Interface:
		if obj.IsNil() {
			return
		}
		e := obj.Elem()
		switch e.Kind() {
		case reflect.Struct, reflect.Array:
			// What the interface holds, which only a new interface may
			// change.
			c := addressable(e)
			cs.ifaceCopies[obj.UnsafeAddr()] = c
			cs.visit(c)
		default:
			cs.visit(e)
		}
	case reflect.Struct:
		cs.visitStruct(obj)
	case reflect.Array:
		ti := cs.typeInfo(obj.Type())
		if !ti.structs && !ti.refs {
			return
		}
		for i := 0; i < obj.Len(); i++ {
			cs.visit(obj.Index(i))
		}
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128, reflect.String:
	default:
		Failf("unknown object %#v", obj.Interface())
	}
}

// visitStruct records what a struct's StateSave method saves, once.
func (cs *cloneState) visitStruct(obj reflect.Value) {
	addr, typ := obj.UnsafeAddr(), obj.Type()
	tid := rtypeOf(typ)
	if cs.lookupStruct(addr, tid) != nil {
		return
	}
	ti := cs.typeInfo(typ)
	sl, ok := obj.Addr().Interface().(SaverLoader)
	bo, binary := obj.Addr().Interface().(binaryObject)
	if (!ok && !binary) || !ti.registered {
		if obj.NumField() == 0 {
			// An anonymous empty struct, as the encoder allows.
			rec := cs.newStructRecord(0)
			rec.typ, rec.empty = tid, true
			rec.next = cs.structs[addr]
			cs.structs[addr] = rec
			return
		}
		Failf("struct %T does not implement SaverLoader", obj.Interface())
	}
	rec := cs.newStructRecord(ti.fields)
	rec.typ = tid
	rec.next = cs.structs[addr]
	cs.structs[addr] = rec
	cr := cs.pushRecorder()
	cr.rec = rec
	cr.ti = ti
	cr.live = obj
	cr.base = obj.UnsafeAddr()
	cr.size = obj.Type().Size()
	cr.snap = nil
	if ok {
		sl.StateSave(Sink{internal: objectEncoder{cr: cr}})
	} else {
		// A foreign struct, which the encoder saves as its binary
		// representation, as the copy loads it.
		data, err := bo.AppendBinary(nil)
		if err != nil {
			Failf("encoding %v: %w", typ, err)
		}
		Sink{internal: objectEncoder{cr: cr}}.SaveValue(0, string(data))
	}
	cs.popRecorder(cr)
}

// newStructRecord returns a record of a struct of n fields.
func (cs *cloneState) newStructRecord(n int) *structRecord {
	if len(cs.recordSlab) == 0 {
		cs.recordSlab = make([]structRecord, slabSize)
	}
	rec := &cs.recordSlab[0]
	cs.recordSlab = cs.recordSlab[1:]
	if n > len(cs.slotSlab) {
		cs.slotSlab = make([]reflect.Value, max(n, 16*slabSize))
	}
	rec.slots = cs.slotSlab[:n:n]
	cs.slotSlab = cs.slotSlab[n:]
	return rec
}

// pushRecorder returns a recorder for a struct's StateSave method, which
// popRecorder releases.
func (cs *cloneState) pushRecorder() *cloneRecorder {
	for _, r := range cs.recorders {
		if r.rec == nil {
			return r
		}
	}
	r := &cloneRecorder{cs: cs}
	cs.recorders = append(cs.recorders, r)
	return r
}

func (cs *cloneState) popRecorder(r *cloneRecorder) {
	r.rec = nil
	r.ti = nil
	r.live = reflect.Value{}
	r.snap = nil
}

// cloneRecorder records what a struct's StateSave method saves, for Sink.
type cloneRecorder struct {
	cs  *cloneState
	rec *structRecord
	// ti is what the clone knows of the struct's type.
	ti *cloneTypeInfo
	// live is the struct, at base, of size bytes.
	live       reflect.Value
	base, size uintptr
	// snap is a copy of the struct, taken when its StateSave method saved
	// its first value: what the method saves of the struct is as it is
	// then, as the encoder would encode it.
	snap unsafe.Pointer
}

// save records the value of a slot, as it is now.
func (cr *cloneRecorder) save(slot int, obj reflect.Value) {
	cs := cr.cs
	for slot >= len(cr.rec.slots) {
		cr.rec.slots = append(cr.rec.slots, reflect.Value{})
	}
	typ := obj.Type()
	tid := rtypeOf(typ)
	var ti *cloneTypeInfo
	if slots := cr.ti.slots; slot < len(slots) && slots[slot].typ == tid {
		ti = slots[slot].ti
	} else {
		ti = cs.typeInfo(typ)
		if slot < len(slots) {
			slots[slot] = slotType{tid, ti}
		}
	}
	if ti.structs {
		// The structs' StateSave methods record them, now.
		v := addressable(obj)
		cr.rec.slots[slot] = v
		cs.visit(v)
		return
	}
	var v reflect.Value
	if obj.CanAddr() {
		if addr := obj.UnsafeAddr(); addr >= cr.base && addr+obj.Type().Size() <= cr.base+cr.size {
			if cr.snap == nil {
				snap := reflect.New(cr.live.Type())
				snap.Elem().Set(cr.live)
				cr.snap = snap.UnsafePointer()
			}
			v = reflect.NewAt(obj.Type(), unsafe.Add(cr.snap, addr-cr.base)).Elem()
		}
	}
	if !v.IsValid() {
		v = reflect.New(obj.Type()).Elem()
		v.Set(obj)
	}
	cr.rec.slots[slot] = v
	if ti.refs {
		cs.visit(v)
	}
}

// copyGraph creates the copies of the recorded objects, dst the root's, and
// fills them. It returns the number of objects.
func (r *copyRun) copyGraph(dst reflect.Value) int {
	cs := r.cs
	objects := make([]*objectEncodeState, 0, len(cs.objects))
	for _, oes := range cs.objects {
		if oes == nil {
			continue
		}
		objects = append(objects, oes)
		rec, co := oes.clone, r.object(oes)
		switch {
		case oes.id == 1:
			co.clone = dst
			co.base = unsafe.Pointer(dst.UnsafeAddr())
		case oes.how == encodeMapAsValue:
			co.clone = reflect.MakeMapWithSize(oes.obj.Type(), len(rec.keys))
		case r.adopt && rec.snapshot.IsValid() && !cs.typeInfo(oes.obj.Type()).refs:
			// The snapshot, which refers to nothing, is the copy.
			co.clone = rec.snapshot
			co.base = unsafe.Pointer(co.clone.UnsafeAddr())
		default:
			co.clone = reflect.New(oes.obj.Type()).Elem()
			co.base = unsafe.Pointer(co.clone.UnsafeAddr())
		}
	}
	for _, oes := range objects {
		rec, co := oes.clone, r.object(oes)
		switch {
		case oes.how == encodeMapAsValue:
			typ := oes.obj.Type()
			for i := range rec.keys {
				k := reflect.New(typ.Key()).Elem()
				v := reflect.New(typ.Elem()).Elem()
				r.copyValue(oes, k, rec.keys[i], true, nil)
				r.copyValue(oes, v, rec.values[i], true, nil)
				co.clone.SetMapIndex(k, v)
			}
		case rec.snapshot.IsValid():
			if co.clone.UnsafeAddr() != rec.snapshot.UnsafeAddr() {
				r.copyValue(oes, co.clone, rec.snapshot, false, nil)
			}
		default:
			r.copyValue(oes, co.clone, oes.obj, false, nil)
		}
	}
	r.complete(objects)
	return len(objects)
}

// complete runs the copies' callbacks, each object's once those of the
// objects that it waits for ran, as decodeState.Load does.
func (r *copyRun) complete(objects []*objectEncodeState) {
	leaves := make([]*objectEncodeState, 0, len(objects))
	for _, oes := range objects {
		if r.object(oes).blockedBy == 0 {
			leaves = append(leaves, oes)
		}
	}
	for i := 0; i < len(leaves); i++ {
		co := r.object(leaves[i])
		for _, cb := range co.callbacks {
			if cb.waiter == nil {
				cb.fn()
				continue
			}
			w := r.object(cb.waiter)
			w.blockedBy--
			if w.blockedBy == 0 {
				leaves = append(leaves, cb.waiter)
			} else if w.blockedBy < 0 {
				Failf("object %d has negative blockedBy: %d", cb.waiter.id, w.blockedBy)
			}
		}
		co.callbacks = nil
		co.done = true
	}
	for _, oes := range objects {
		if !r.object(oes).done {
			Failf("incomplete graph: dependency cycle: object %d (%v) and others never complete", oes.id, oes.obj.Type())
		}
	}
}

// lookup returns the object that holds what ptr, a pointer, points to, and
// the address of its copy.
func (r *copyRun) lookup(ptr reflect.Value) (*objectEncodeState, unsafe.Pointer) {
	cs := r.cs
	addr := ptr.Pointer()
	typ := ptr.Type().Elem()
	if typ.Size() == 0 && addr == dummyAddr {
		oes := cs.zeroValues[typ]
		if oes == nil {
			Failf("no object for zero-sized %v", typ)
		}
		return oes, r.object(oes).base
	}
	seg := cs.values.FindSegment(addr)
	if !seg.Ok() {
		Failf("no object holds %v at %#x", typ, addr)
	}
	oes := seg.Value()
	return oes, unsafe.Add(r.object(oes).base, addr-seg.Start())
}

// wait makes waiter's copy wait for target's to complete, and runs fn once
// it does, as decodeState.wait does.
func (r *copyRun) wait(waiter, target *objectEncodeState, fn func()) {
	if target == waiter || target.id == 1 {
		if fn != nil {
			fn()
		}
		return
	}
	r.object(waiter).blockedBy++
	t := r.object(target)
	if fn != nil {
		t.callbacks = append(t.callbacks, cloneCallback{fn: fn})
	}
	t.callbacks = append(t.callbacks, cloneCallback{waiter: waiter})
}

// setConverted sets dst to v, of the same underlying type.
func setConverted(dst, v reflect.Value) {
	if rtypeOf(v.Type()) != rtypeOf(dst.Type()) {
		v = v.Convert(dst.Type())
	}
	dst.Set(v)
}

// copyValue makes dst, a settable value of object oes's copy, a copy of src,
// a recorded value. If wait is set, the copy waits for the objects that src
// refers to, and fn runs once they are complete, as decodeState.waitObject
// does.
func (r *copyRun) copyValue(oes *objectEncodeState, dst, src reflect.Value, wait bool, fn func()) {
	cs := r.cs
	switch src.Kind() {
	case reflect.Ptr:
		if src.IsNil() {
			break
		}
		target, p := r.lookup(src)
		setConverted(dst, reflect.NewAt(src.Type().Elem(), p))
		if wait {
			r.wait(oes, target, fn)
			return
		}
	case reflect.Map:
		if src.IsNil() {
			break
		}
		seg := cs.values.FindSegment(src.Pointer())
		if !seg.Ok() || seg.Value().how != encodeMapAsValue {
			Failf("no map object at %#x", src.Pointer())
		}
		target := seg.Value()
		setConverted(dst, r.object(target).clone)
		if wait {
			r.wait(oes, target, fn)
			return
		}
	case reflect.Slice:
		if src.IsNil() {
			break
		}
		if src.Cap() == 0 {
			dst.Set(reflect.MakeSlice(dst.Type(), 0, 0))
			break
		}
		arrayPtr := arrayFromSlice(src)
		target, p := r.lookup(arrayPtr)
		array := reflect.NewAt(arrayPtr.Type().Elem(), p).Elem()
		setConverted(dst, reflectValueRWSlice3(array, 0, src.Len(), src.Cap()))
		if wait {
			r.wait(oes, target, fn)
			return
		}
	case reflect.Interface:
		if src.IsNil() {
			break
		}
		e := src.Elem()
		if k := e.Kind(); k == reflect.Struct || k == reflect.Array {
			c, ok := cs.ifaceCopies[src.UnsafeAddr()]
			if !ok {
				Failf("no copy of the %v that an interface holds", e.Type())
			}
			e = c
		}
		v := reflect.New(e.Type()).Elem()
		r.copyValue(oes, v, e, wait, fn)
		setInterface(dst, src, v)
		return
	case reflect.Struct:
		rec := cs.lookupStruct(src.UnsafeAddr(), rtypeOf(src.Type()))
		if rec == nil {
			Failf("struct %v at %#x was not recorded", src.Type(), src.UnsafeAddr())
		}
		if !rec.empty {
			switch v := dst.Addr().Interface().(type) {
			case SaverLoader:
				cl := r.pushLoader()
				cl.oes, cl.rec = oes, rec
				v.StateLoad(r.ctx, Source{internal: objectDecoder{cl: cl}})
				r.popLoader(cl)
			case binaryObject:
				cl := r.pushLoader()
				cl.oes, cl.rec = oes, rec
				loadBinary(v, src.Type().String(), Source{internal: objectDecoder{cl: cl}})
				r.popLoader(cl)
			}
		}
	case reflect.Array:
		ti := cs.typeInfo(src.Type())
		if !ti.structs && !ti.refs {
			if src.Len() != 0 {
				reflect.Copy(reflectValueRWSlice3(dst, 0, dst.Len(), dst.Len()), reflectValueRWSlice3(src, 0, src.Len(), src.Len()))
			}
			break
		}
		for i := 0; i < src.Len(); i++ {
			// The decoder waits for what each element refers to.
			r.copyValue(oes, dst.Index(i), src.Index(i), true, nil)
		}
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128, reflect.String:
		cs.copyPrimitive(dst, src)
	default:
		Failf("unknown object %#v", src.Interface())
	}
	if wait && fn != nil {
		// Nothing to wait for.
		fn()
	}
}

// setInterface sets dst, an interface, to v, a copy of what src, an
// interface, holds. reflect.Value.Set looks for each of the interface's
// methods in v's type, every time: an interface of the type of src, which
// holds a value of v's type, has the type word dst needs, and v, as any
// interface holds it, the data word.
func setInterface(dst, src, v reflect.Value) {
	if rtypeOf(dst.Type()) != rtypeOf(src.Type()) || !dst.CanAddr() || !src.CanAddr() || rtypeOf(src.Elem().Type()) != rtypeOf(v.Type()) {
		dst.Set(v)
		return
	}
	held := v.Interface()
	words := (*[2]unsafe.Pointer)(unsafe.Pointer(dst.UnsafeAddr()))
	words[0] = (*[2]unsafe.Pointer)(unsafe.Pointer(src.UnsafeAddr()))[0]
	words[1] = (*[2]unsafe.Pointer)(unsafe.Pointer(&held))[1]
}

// pushLoader returns a loader for a struct's StateLoad method, which
// popLoader releases.
func (r *copyRun) pushLoader() *cloneLoader {
	for _, l := range r.loaders {
		if l.rec == nil {
			return l
		}
	}
	l := &cloneLoader{r: r}
	r.loaders = append(r.loaders, l)
	return l
}

func (r *copyRun) popLoader(l *cloneLoader) {
	l.oes, l.rec = nil, nil
}

// cloneLoader gives a copy's StateLoad method what the source's StateSave
// method saved, for Source.
type cloneLoader struct {
	r   *copyRun
	oes *objectEncodeState
	rec *structRecord
}

// load copies the value of a slot into dst, as objectDecoder.load decodes
// it.
func (cl *cloneLoader) load(slot int, dst reflect.Value, wait bool, fn func()) {
	var src reflect.Value
	if slot < len(cl.rec.slots) {
		src = cl.rec.slots[slot]
	}
	if !src.IsValid() || cl.r.cs.isZeroPrimitive(src) {
		// The encoder encodes nothing for a zero value, and the decoder
		// leaves the destination as it is.
		if wait && fn != nil {
			fn()
		}
		return
	}
	cl.r.copyValue(cl.oes, dst, src, wait, fn)
}

// afterLoad queues fn to run once the copy of the object is complete.
func (cl *cloneLoader) afterLoad(fn func()) {
	co := cl.r.object(cl.oes)
	co.callbacks = append(co.callbacks, cloneCallback{fn: fn})
}

// isZeroPrimitive reports whether src, an addressable value, is a zero
// value that the encoder would encode as nothing, as isPrimitiveZero and
// reflect.Value.IsZero do.
func (cs *cloneState) isZeroPrimitive(src reflect.Value) bool {
	ti := cs.typeInfo(src.Type())
	if !ti.primitiveZero {
		return false
	}
	if ti.plain && src.CanAddr() {
		return isZeroBytes(unsafe.Pointer(src.UnsafeAddr()), ti.size)
	}
	return src.IsZero()
}

// isZeroBytes reports whether the size bytes at p are zero.
func isZeroBytes(p unsafe.Pointer, size uintptr) bool {
	switch size {
	case 1:
		return *(*uint8)(p) == 0
	case 2:
		return *(*uint16)(p) == 0
	case 4:
		return *(*uint32)(p) == 0
	case 8:
		return *(*uint64)(p) == 0
	}
	for _, b := range unsafe.Slice((*byte)(p), size) {
		if b != 0 {
			return false
		}
	}
	return true
}

// copyPrimitive sets dst to src, primitives, as the decoder sets a value of
// one type to what the encoder encoded of another, of the same kind.
// Primitives of one type that hold no pointers are bytes to copy.
func (cs *cloneState) copyPrimitive(dst, src reflect.Value) {
	if st, dt := src.Type(), dst.Type(); rtypeOf(st) == rtypeOf(dt) {
		if ti := cs.typeInfo(st); ti.plain && dst.CanAddr() && src.CanAddr() {
			copy(unsafe.Slice((*byte)(unsafe.Pointer(dst.UnsafeAddr())), ti.size), unsafe.Slice((*byte)(unsafe.Pointer(src.UnsafeAddr())), ti.size))
			return
		}
		dst.Set(src)
		return
	}
	switch src.Kind() {
	case reflect.Bool:
		dst.SetBool(src.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		dst.SetInt(src.Int())
		if dst.Int() != src.Int() {
			Failf("signed integer truncated from %v to %v", src.Int(), dst.Int())
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		dst.SetUint(src.Uint())
		if dst.Uint() != src.Uint() {
			Failf("unsigned integer truncated from %v to %v", src.Uint(), dst.Uint())
		}
	case reflect.Float32, reflect.Float64:
		dst.SetFloat(src.Float())
		if !isFloatEq(dst.Float(), src.Float()) {
			Failf("floating point number truncated from %v to %v", src.Float(), dst.Float())
		}
	case reflect.Complex64, reflect.Complex128:
		dst.SetComplex(src.Complex())
		if !isComplexEq(dst.Complex(), src.Complex()) {
			Failf("complex number truncated from %v to %v", src.Complex(), dst.Complex())
		}
	case reflect.String:
		dst.SetString(src.String())
	}
}
