// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package state

import (
	"context"
	"fmt"
	"reflect"
	"unsafe"
)

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
func Clone(saveCtx, loadCtx context.Context, srcPtr, dstPtr any, opts SaveOptions) (err error) {
	src := reflect.ValueOf(srcPtr).Elem()
	dst := reflect.ValueOf(dstPtr).Elem()
	if src.Type() != dst.Type() {
		return fmt.Errorf("cloning a %v into a %v", src.Type(), dst.Type())
	}
	cs := cloneState{
		saveCtx:                   saveCtx,
		loadCtx:                   loadCtx,
		zeroValues:                make(map[reflect.Type]*objectEncodeState),
		structs:                   make(map[structKey]*structRecord),
		types:                     make(map[reflect.Type]*cloneTypeInfo),
		ifaceCopies:               make(map[uintptr]reflect.Value),
		rootAddr:                  src.Addr().Pointer(),
		rootType:                  src.Type(),
		rejectMultipleRootObjects: opts.RejectMultipleRootObjects,
	}
	return safely(func() {
		cs.record(src.Addr())
		cs.copyGraph(dst)
	})
}

// cloneState is the state of a Clone.
type cloneState struct {
	saveCtx context.Context
	loadCtx context.Context

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

	// structs are the source's structs, by their addresses and types, and
	// what their StateSave methods saved.
	structs map[structKey]*structRecord

	// types caches what the clone needs to know of types.
	types map[reflect.Type]*cloneTypeInfo

	// ifaceCopies are addressable copies of the structs and arrays that
	// interfaces hold, by the addresses of the interfaces' recorded
	// values.
	ifaceCopies map[uintptr]reflect.Value

	// recorders and loaders are reused by the depth of structs' StateSave
	// and StateLoad methods' calls.
	recorders []*cloneRecorder
	loaders   []*cloneLoader

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

// structKey identifies a struct: a struct and its first field share an
// address.
type structKey struct {
	addr uintptr
	typ  reflect.Type
}

// structRecord is what a struct's StateSave method saved, by slot: values
// as they were when it saved them, or structs that hold structs, whose own
// records are what they saved.
type structRecord struct {
	slots []reflect.Value
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
	// slots caches what the clone knows of the types of the values that a
	// struct type's StateSave method saves, by slot.
	slots []slotType
}

// slotType is what the clone knows of the type of a slot's value.
type slotType struct {
	typ reflect.Type
	ti  *cloneTypeInfo
}

// typeInfo returns what the clone knows of typ.
func (cs *cloneState) typeInfo(typ reflect.Type) *cloneTypeInfo {
	if ti, ok := cs.types[typ]; ok {
		return ti
	}
	ti := &cloneTypeInfo{}
	cs.types[typ] = ti
	switch typ.Kind() {
	case reflect.Ptr, reflect.Slice, reflect.Map, reflect.Interface:
		ti.refs = true
	case reflect.Struct:
		ti.structs = true
		_, fields, ok := lookupNameFields(typ)
		ti.registered = ok
		ti.fields = len(fields)
		ti.slots = make([]slotType, len(fields))
	case reflect.Array:
		elem := cs.typeInfo(typ.Elem())
		ti.structs, ti.refs = elem.structs, elem.refs
	}
	return ti
}

// cloneObject is the copy of an object of the source's graph.
type cloneObject struct {
	// snapshot is a copy of an object that holds no structs, as it was when
	// the clone recorded it.
	snapshot reflect.Value
	// keys and values are a map's entries, addressable copies.
	keys, values []reflect.Value

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
	key := structKey{obj.UnsafeAddr(), obj.Type()}
	if _, ok := cs.structs[key]; ok {
		return
	}
	ti := cs.typeInfo(obj.Type())
	sl, ok := obj.Addr().Interface().(SaverLoader)
	if !ok || !ti.registered {
		if obj.NumField() == 0 {
			// An anonymous empty struct, as the encoder allows.
			cs.structs[key] = nil
			return
		}
		Failf("struct %T does not implement SaverLoader", obj.Interface())
	}
	rec := cs.newStructRecord(ti.fields)
	cs.structs[key] = rec
	cr := cs.pushRecorder()
	cr.rec = rec
	cr.ti = ti
	cr.live = obj
	cr.base = obj.UnsafeAddr()
	cr.size = obj.Type().Size()
	cr.snap = nil
	sl.StateSave(Sink{internal: objectEncoder{cr: cr}})
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
	var ti *cloneTypeInfo
	if slots := cr.ti.slots; slot < len(slots) && slots[slot].typ == typ {
		ti = slots[slot].ti
	} else {
		ti = cs.typeInfo(typ)
		if slot < len(slots) {
			slots[slot] = slotType{typ, ti}
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
// fills them.
func (cs *cloneState) copyGraph(dst reflect.Value) {
	objects := make([]*objectEncodeState, 0, len(cs.objects))
	for _, oes := range cs.objects {
		if oes == nil {
			continue
		}
		objects = append(objects, oes)
		cl := oes.clone
		switch {
		case oes.id == 1:
			cl.clone = dst
			cl.base = unsafe.Pointer(dst.UnsafeAddr())
		case oes.how == encodeMapAsValue:
			cl.clone = reflect.MakeMap(oes.obj.Type())
		case cl.snapshot.IsValid() && !cs.typeInfo(oes.obj.Type()).refs:
			// The snapshot, which refers to nothing, is the copy.
			cl.clone = cl.snapshot
			cl.base = unsafe.Pointer(cl.clone.UnsafeAddr())
		default:
			cl.clone = reflect.New(oes.obj.Type()).Elem()
			cl.base = unsafe.Pointer(cl.clone.UnsafeAddr())
		}
	}
	for _, oes := range objects {
		cl := oes.clone
		switch {
		case oes.how == encodeMapAsValue:
			typ := oes.obj.Type()
			for i := range cl.keys {
				k := reflect.New(typ.Key()).Elem()
				v := reflect.New(typ.Elem()).Elem()
				cs.copyValue(oes, k, cl.keys[i], true, nil)
				cs.copyValue(oes, v, cl.values[i], true, nil)
				cl.clone.SetMapIndex(k, v)
			}
			cl.keys, cl.values = nil, nil
		case cl.snapshot.IsValid():
			if cl.clone.UnsafeAddr() != cl.snapshot.UnsafeAddr() {
				cs.copyValue(oes, cl.clone, cl.snapshot, false, nil)
			}
		default:
			cs.copyValue(oes, cl.clone, oes.obj, false, nil)
		}
	}
	cs.complete(objects)
}

// complete runs the copies' callbacks, each object's once those of the
// objects that it waits for ran, as decodeState.Load does.
func (cs *cloneState) complete(objects []*objectEncodeState) {
	leaves := make([]*objectEncodeState, 0, len(objects))
	for _, oes := range objects {
		if oes.clone.blockedBy == 0 {
			leaves = append(leaves, oes)
		}
	}
	for i := 0; i < len(leaves); i++ {
		cl := leaves[i].clone
		for _, cb := range cl.callbacks {
			if cb.waiter == nil {
				cb.fn()
				continue
			}
			w := cb.waiter.clone
			w.blockedBy--
			if w.blockedBy == 0 {
				leaves = append(leaves, cb.waiter)
			} else if w.blockedBy < 0 {
				Failf("object %d has negative blockedBy: %d", cb.waiter.id, w.blockedBy)
			}
		}
		cl.callbacks = nil
		cl.done = true
	}
	for _, oes := range objects {
		if !oes.clone.done {
			Failf("incomplete graph: dependency cycle: object %d (%v) and others never complete", oes.id, oes.obj.Type())
		}
	}
}

// lookup returns the object that holds what ptr, a pointer, points to, and
// the address of its copy.
func (cs *cloneState) lookup(ptr reflect.Value) (*objectEncodeState, unsafe.Pointer) {
	addr := ptr.Pointer()
	typ := ptr.Type().Elem()
	if typ.Size() == 0 && addr == dummyAddr {
		oes := cs.zeroValues[typ]
		if oes == nil {
			Failf("no object for zero-sized %v", typ)
		}
		return oes, oes.clone.base
	}
	seg := cs.values.FindSegment(addr)
	if !seg.Ok() {
		Failf("no object holds %v at %#x", typ, addr)
	}
	oes := seg.Value()
	return oes, unsafe.Add(oes.clone.base, addr-seg.Start())
}

// wait makes waiter's copy wait for target's to complete, and runs fn once
// it does, as decodeState.wait does.
func (cs *cloneState) wait(waiter, target *objectEncodeState, fn func()) {
	if target == waiter || target.id == 1 {
		if fn != nil {
			fn()
		}
		return
	}
	waiter.clone.blockedBy++
	if fn != nil {
		target.clone.callbacks = append(target.clone.callbacks, cloneCallback{fn: fn})
	}
	target.clone.callbacks = append(target.clone.callbacks, cloneCallback{waiter: waiter})
}

// setConverted sets dst to v, of the same underlying type.
func setConverted(dst, v reflect.Value) {
	if v.Type() != dst.Type() {
		v = v.Convert(dst.Type())
	}
	dst.Set(v)
}

// copyValue makes dst, a settable value of object oes's copy, a copy of src,
// a recorded value. If wait is set, the copy waits for the objects that src
// refers to, and fn runs once they are complete, as decodeState.waitObject
// does.
func (cs *cloneState) copyValue(oes *objectEncodeState, dst, src reflect.Value, wait bool, fn func()) {
	switch src.Kind() {
	case reflect.Ptr:
		if src.IsNil() {
			break
		}
		target, p := cs.lookup(src)
		setConverted(dst, reflect.NewAt(src.Type().Elem(), p))
		if wait {
			cs.wait(oes, target, fn)
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
		setConverted(dst, target.clone.clone)
		if wait {
			cs.wait(oes, target, fn)
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
		target, p := cs.lookup(arrayPtr)
		array := reflect.NewAt(arrayPtr.Type().Elem(), p).Elem()
		setConverted(dst, reflectValueRWSlice3(array, 0, src.Len(), src.Cap()))
		if wait {
			cs.wait(oes, target, fn)
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
		cs.copyValue(oes, v, e, wait, fn)
		dst.Set(v)
		return
	case reflect.Struct:
		rec, ok := cs.structs[structKey{src.UnsafeAddr(), src.Type()}]
		if !ok {
			Failf("struct %v at %#x was not recorded", src.Type(), src.UnsafeAddr())
		}
		if rec != nil {
			if sl, ok := dst.Addr().Interface().(SaverLoader); ok {
				cl := cs.pushLoader()
				cl.oes, cl.rec = oes, rec
				sl.StateLoad(cs.loadCtx, Source{internal: objectDecoder{cl: cl}})
				cs.popLoader(cl)
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
			cs.copyValue(oes, dst.Index(i), src.Index(i), true, nil)
		}
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128, reflect.String:
		copyPrimitive(dst, src)
	default:
		Failf("unknown object %#v", src.Interface())
	}
	if wait && fn != nil {
		// Nothing to wait for.
		fn()
	}
}

// pushLoader returns a loader for a struct's StateLoad method, which
// popLoader releases.
func (cs *cloneState) pushLoader() *cloneLoader {
	for _, l := range cs.loaders {
		if l.rec == nil {
			return l
		}
	}
	l := &cloneLoader{cs: cs}
	cs.loaders = append(cs.loaders, l)
	return l
}

func (cs *cloneState) popLoader(l *cloneLoader) {
	l.oes, l.rec = nil, nil
}

// cloneLoader gives a copy's StateLoad method what the source's StateSave
// method saved, for Source.
type cloneLoader struct {
	cs  *cloneState
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
	if !src.IsValid() || (isPrimitiveZero(src.Type()) && src.IsZero()) {
		// The encoder encodes nothing for a zero value, and the decoder
		// leaves the destination as it is.
		if wait && fn != nil {
			fn()
		}
		return
	}
	cl.cs.copyValue(cl.oes, dst, src, wait, fn)
}

// afterLoad queues fn to run once the copy of the object is complete.
func (cl *cloneLoader) afterLoad(fn func()) {
	cl.oes.clone.callbacks = append(cl.oes.clone.callbacks, cloneCallback{fn: fn})
}

// copyPrimitive sets dst to src, primitives, as the decoder sets a value of
// one type to what the encoder encoded of another, of the same kind.
func copyPrimitive(dst, src reflect.Value) {
	if src.Type() == dst.Type() {
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
