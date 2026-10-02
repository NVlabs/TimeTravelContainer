// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ebpf

import (
	"encoding/binary"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/sync"
)

// Limits of maps' sizes, as Linux's.
const (
	// maxKeySize is MAX_BPF_STACK: programs can't use larger keys.
	maxKeySize = 512

	// maxElemSize bounds a hash map's keys and values together, as
	// KMALLOC_MAX_SIZE does.
	maxElemSize = 4 << 20

	// maxPerCPUValueSize is PCPU_MIN_UNIT_SIZE, which bounds per-CPU maps'
	// values.
	maxPerCPUValueSize = 32 << 10

	// hashEntryOverhead is the memory charged for each element of a hash
	// map, besides its key and value.
	hashEntryOverhead = 64
)

// Flags that maps of each kind may have, as Linux's HTAB_CREATE_FLAG_MASK
// and ARRAY_CREATE_FLAG_MASK. The sentry lacks NUMA nodes, mmap of maps,
// and maps of maps.
const (
	accessFlags     = linux.BPF_F_RDONLY | linux.BPF_F_WRONLY | linux.BPF_F_RDONLY_PROG | linux.BPF_F_WRONLY_PROG
	hashCreateFlags = linux.BPF_F_NO_PREALLOC | linux.BPF_F_NO_COMMON_LRU | linux.BPF_F_ZERO_SEED | accessFlags
	arrayFlags      = linux.BPF_F_PRESERVE_ELEMS | accessFlags
)

// MapOptions describes a map to create.
type MapOptions struct {
	Type       linux.BPFMapType
	KeySize    uint32
	ValueSize  uint32
	MaxEntries uint32
	Flags      uint32
	Name       string

	// NumCPUs is the number of possible CPUs, whose values per-CPU maps
	// have.
	NumCPUs uint32
}

// Map is an eBPF map. Programs never run, so only bpf(2) reads and writes
// it.
//
// All fields of Map, but refs and those that mu protects, are immutable.
//
// +stateify savable
type Map struct {
	// refs is the number of references on the map: from file descriptions,
	// pins, and programs.
	refs atomicbitops.Int64

	// registry is the registry that the map's memory is charged to.
	registry *Registry

	// cost is the memory charged for the map.
	cost int64

	id         BPFID
	mapType    linux.BPFMapType
	keySize    uint32
	valueSize  uint32
	maxEntries uint32
	flags      uint32
	name       string

	// userValueSize is the size of values as bpf(2) reads and writes them:
	// the values of all CPUs, 8-byte aligned, for per-CPU maps.
	userValueSize uint32

	mu sync.Mutex `state:"nosave"`

	// frozen is whether bpf(2) may no longer write the map.
	//
	// +checklocks:mu
	frozen bool

	// array holds an array's values, of userValueSize each.
	//
	// +checklocks:mu
	array []byte

	// entries holds a hash map's elements, by key. They are also in a list,
	// from head to tail, in the order in which bpf(2) iterates them: that
	// of their insertion, or, for LRU maps, of their last use.
	//
	// +checklocks:mu
	entries map[string]*hashEntry
	// +checklocks:mu
	head *hashEntry
	// +checklocks:mu
	tail *hashEntry
}

// hashEntry is an element of a hash map.
//
// +stateify savable
type hashEntry struct {
	key        string
	value      []byte
	prev, next *hashEntry
}

// isArray returns whether maps of type t are arrays.
func isArray(t linux.BPFMapType) bool {
	return t == linux.BPF_MAP_TYPE_ARRAY || t == linux.BPF_MAP_TYPE_PERCPU_ARRAY
}

// isPerCPU returns whether maps of type t have a value per CPU.
func isPerCPU(t linux.BPFMapType) bool {
	return t == linux.BPF_MAP_TYPE_PERCPU_ARRAY || t == linux.BPF_MAP_TYPE_PERCPU_HASH || t == linux.BPF_MAP_TYPE_LRU_PERCPU_HASH
}

// isLRU returns whether maps of type t evict their least recently used
// elements when full.
func isLRU(t linux.BPFMapType) bool {
	return t == linux.BPF_MAP_TYPE_LRU_HASH || t == linux.BPF_MAP_TYPE_LRU_PERCPU_HASH
}

// SupportedMapType returns whether the sentry has maps of type t.
func SupportedMapType(t linux.BPFMapType) bool {
	switch t {
	case linux.BPF_MAP_TYPE_HASH, linux.BPF_MAP_TYPE_ARRAY,
		linux.BPF_MAP_TYPE_PERCPU_HASH, linux.BPF_MAP_TYPE_PERCPU_ARRAY,
		linux.BPF_MAP_TYPE_LRU_HASH, linux.BPF_MAP_TYPE_LRU_PERCPU_HASH:
		return true
	default:
		return false
	}
}

// round8 rounds n up to a multiple of 8.
func round8(n uint64) uint64 {
	return (n + 7) &^ 7
}

// NewMap returns a map, which holds a reference, as the caller's. It checks
// opts as Linux's htab_map_alloc_check and array_map_alloc_check do.
func (r *Registry) NewMap(opts MapOptions) (*Map, error) {
	if !SupportedMapType(opts.Type) {
		return nil, linuxerr.EINVAL
	}
	perCPU := isPerCPU(opts.Type)
	if opts.MaxEntries == 0 || opts.KeySize == 0 || opts.ValueSize == 0 {
		return nil, linuxerr.EINVAL
	}
	if opts.Flags&linux.BPF_F_RDONLY != 0 && opts.Flags&linux.BPF_F_WRONLY != 0 {
		return nil, linuxerr.EINVAL
	}
	if opts.Flags&linux.BPF_F_RDONLY_PROG != 0 && opts.Flags&linux.BPF_F_WRONLY_PROG != 0 {
		return nil, linuxerr.EINVAL
	}
	if perCPU && round8(uint64(opts.ValueSize)) > maxPerCPUValueSize {
		return nil, linuxerr.E2BIG
	}
	userValueSize := uint64(opts.ValueSize)
	if perCPU {
		userValueSize = round8(userValueSize) * uint64(max(opts.NumCPUs, 1))
	}
	var cost uint64
	if isArray(opts.Type) {
		if opts.KeySize != 4 || opts.Flags&^arrayFlags != 0 {
			return nil, linuxerr.EINVAL
		}
		cost = uint64(opts.MaxEntries) * round8(userValueSize)
	} else {
		if opts.Flags&^hashCreateFlags != 0 {
			return nil, linuxerr.EINVAL
		}
		if opts.Flags&linux.BPF_F_NO_COMMON_LRU != 0 && !isLRU(opts.Type) {
			return nil, linuxerr.EINVAL
		}
		if opts.KeySize > maxKeySize {
			return nil, linuxerr.E2BIG
		}
		if uint64(opts.KeySize)+uint64(opts.ValueSize) >= maxElemSize {
			return nil, linuxerr.E2BIG
		}
		cost = uint64(opts.MaxEntries) * (round8(uint64(opts.KeySize)) + round8(userValueSize) + hashEntryOverhead)
	}
	cost += objectOverhead
	if cost > MemoryLimit {
		return nil, linuxerr.ENOMEM
	}
	if err := r.charge(int64(cost)); err != nil {
		return nil, err
	}
	var array []byte
	var entries map[string]*hashEntry
	if isArray(opts.Type) {
		array = make([]byte, uint64(opts.MaxEntries)*userValueSize)
	} else {
		entries = make(map[string]*hashEntry)
	}
	m := &Map{
		registry:      r,
		cost:          int64(cost),
		id:            r.newID(&r.nextMapID),
		mapType:       opts.Type,
		keySize:       opts.KeySize,
		valueSize:     opts.ValueSize,
		maxEntries:    opts.MaxEntries,
		flags:         opts.Flags,
		name:          opts.Name,
		userValueSize: uint32(userValueSize),
		array:         array,
		entries:       entries,
	}
	m.refs.Store(1)
	return m, nil
}

// IncRef takes a reference on m.
func (m *Map) IncRef() {
	m.refs.Add(1)
}

// DecRef drops a reference on m, and releases it if it was the last.
func (m *Map) DecRef() {
	if m.refs.Add(-1) != 0 {
		return
	}
	m.mu.Lock()
	m.array = nil
	m.entries = make(map[string]*hashEntry)
	m.head = nil
	m.tail = nil
	m.mu.Unlock()
	m.registry.uncharge(m.cost)
}

// ID returns the map's ID.
func (m *Map) ID() BPFID { return m.id }

// Type returns the map's type.
func (m *Map) Type() linux.BPFMapType { return m.mapType }

// KeySize returns the size of the map's keys.
func (m *Map) KeySize() uint32 { return m.keySize }

// ValueSize returns the size of the map's values.
func (m *Map) ValueSize() uint32 { return m.valueSize }

// UserValueSize returns the size of values as bpf(2) reads and writes them.
func (m *Map) UserValueSize() uint32 { return m.userValueSize }

// MaxEntries returns the most elements the map may have.
func (m *Map) MaxEntries() uint32 { return m.maxEntries }

// Flags returns the map's flags.
func (m *Map) Flags() uint32 { return m.flags }

// Name returns the map's name.
func (m *Map) Name() string { return m.name }

// Frozen returns whether bpf(2) may no longer write m.
func (m *Map) Frozen() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.frozen
}

// Freeze makes m read-only for bpf(2), as BPF_MAP_FREEZE does.
func (m *Map) Freeze() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.frozen {
		return linuxerr.EBUSY
	}
	m.frozen = true
	return nil
}

// arrayIndex returns the index that key, of an array, holds.
func arrayIndex(key []byte) uint32 {
	return binary.LittleEndian.Uint32(key)
}

// Lookup returns the value of key, of KeySize bytes.
func (m *Map) Lookup(key []byte) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if isArray(m.mapType) {
		i := arrayIndex(key)
		if i >= m.maxEntries || m.array == nil {
			return nil, linuxerr.ENOENT
		}
		off := uint64(i) * uint64(m.userValueSize)
		return append([]byte(nil), m.array[off:off+uint64(m.userValueSize)]...), nil
	}
	e, ok := m.entries[string(key)]
	if !ok {
		return nil, linuxerr.ENOENT
	}
	return append([]byte(nil), e.value...), nil
}

// Update sets the value of key, of KeySize bytes, to value, of
// UserValueSize bytes, as flags, BPF_ANY, BPF_NOEXIST, or BPF_EXIST, allow.
func (m *Map) Update(key, value []byte, flags uint64) error {
	if flags > linux.BPF_EXIST {
		return linuxerr.EINVAL
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if isArray(m.mapType) {
		i := arrayIndex(key)
		if i >= m.maxEntries || m.array == nil {
			// All elements exist: there is no room for a new one.
			return linuxerr.E2BIG
		}
		if flags == linux.BPF_NOEXIST {
			return linuxerr.EEXIST
		}
		off := uint64(i) * uint64(m.userValueSize)
		copy(m.array[off:off+uint64(m.userValueSize)], value)
		return nil
	}
	e, ok := m.entries[string(key)]
	switch {
	case ok && flags == linux.BPF_NOEXIST:
		return linuxerr.EEXIST
	case !ok && flags == linux.BPF_EXIST:
		return linuxerr.ENOENT
	case ok:
		e.value = append(e.value[:0], value...)
		if isLRU(m.mapType) {
			m.unlinkLocked(e)
			m.pushLocked(e)
		}
		return nil
	}
	if uint32(len(m.entries)) >= m.maxEntries {
		if !isLRU(m.mapType) {
			return linuxerr.E2BIG
		}
		old := m.head
		m.unlinkLocked(old)
		delete(m.entries, old.key)
	}
	e = &hashEntry{
		key:   string(key),
		value: append([]byte(nil), value...),
	}
	m.entries[e.key] = e
	m.pushLocked(e)
	return nil
}

// Delete deletes key, of KeySize bytes.
func (m *Map) Delete(key []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if isArray(m.mapType) {
		return linuxerr.EINVAL
	}
	e, ok := m.entries[string(key)]
	if !ok {
		return linuxerr.ENOENT
	}
	m.unlinkLocked(e)
	delete(m.entries, e.key)
	return nil
}

// LookupAndDelete deletes key, of KeySize bytes, and returns its value.
func (m *Map) LookupAndDelete(key []byte) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if isArray(m.mapType) {
		return nil, linuxerr.EOPNOTSUPP
	}
	e, ok := m.entries[string(key)]
	if !ok {
		return nil, linuxerr.ENOENT
	}
	m.unlinkLocked(e)
	delete(m.entries, e.key)
	return e.value, nil
}

// NextKey returns the key after key, or, if key is nil or missing, the first
// key, as BPF_MAP_GET_NEXT_KEY does. It fails with ENOENT past the last.
func (m *Map) NextKey(key []byte) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if isArray(m.mapType) {
		i := uint32(0)
		if key != nil {
			cur := arrayIndex(key)
			if cur < m.maxEntries {
				if cur == m.maxEntries-1 {
					return nil, linuxerr.ENOENT
				}
				i = cur + 1
			}
		}
		return binary.LittleEndian.AppendUint32(nil, i), nil
	}
	next := m.head
	if key != nil {
		if e, ok := m.entries[string(key)]; ok {
			next = e.next
		}
	}
	if next == nil {
		return nil, linuxerr.ENOENT
	}
	return []byte(next.key), nil
}

// pushLocked appends e to m's list.
//
// +checklocks:m.mu
func (m *Map) pushLocked(e *hashEntry) {
	e.prev = m.tail
	e.next = nil
	if m.tail != nil {
		m.tail.next = e
	} else {
		m.head = e
	}
	m.tail = e
}

// unlinkLocked removes e from m's list.
//
// +checklocks:m.mu
func (m *Map) unlinkLocked(e *hashEntry) {
	if e.prev != nil {
		e.prev.next = e.next
	} else {
		m.head = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	} else {
		m.tail = e.prev
	}
	e.prev = nil
	e.next = nil
}
