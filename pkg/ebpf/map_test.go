// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ebpf

import (
	"bytes"
	"encoding/binary"
	"testing"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
)

func u32(v uint32) []byte {
	return binary.LittleEndian.AppendUint32(nil, v)
}

func newTestRegistry() *Registry {
	var r Registry
	r.Init()
	return &r
}

func TestNewMapChecks(t *testing.T) {
	r := newTestRegistry()
	for _, tc := range []struct {
		name string
		opts MapOptions
		want error
	}{
		{"unsupported type", MapOptions{Type: linux.BPF_MAP_TYPE_RINGBUF, KeySize: 4, ValueSize: 4, MaxEntries: 4}, linuxerr.EINVAL},
		{"no entries", MapOptions{Type: linux.BPF_MAP_TYPE_HASH, KeySize: 4, ValueSize: 4}, linuxerr.EINVAL},
		{"array key size", MapOptions{Type: linux.BPF_MAP_TYPE_ARRAY, KeySize: 8, ValueSize: 4, MaxEntries: 4}, linuxerr.EINVAL},
		{"array no prealloc", MapOptions{Type: linux.BPF_MAP_TYPE_ARRAY, KeySize: 4, ValueSize: 4, MaxEntries: 4, Flags: linux.BPF_F_NO_PREALLOC}, linuxerr.EINVAL},
		{"read and write only", MapOptions{Type: linux.BPF_MAP_TYPE_HASH, KeySize: 4, ValueSize: 4, MaxEntries: 4, Flags: linux.BPF_F_RDONLY | linux.BPF_F_WRONLY}, linuxerr.EINVAL},
		{"large key", MapOptions{Type: linux.BPF_MAP_TYPE_HASH, KeySize: 1024, ValueSize: 4, MaxEntries: 4}, linuxerr.E2BIG},
		{"common LRU flag on hash", MapOptions{Type: linux.BPF_MAP_TYPE_HASH, KeySize: 4, ValueSize: 4, MaxEntries: 4, Flags: linux.BPF_F_NO_COMMON_LRU}, linuxerr.EINVAL},
		{"over budget", MapOptions{Type: linux.BPF_MAP_TYPE_ARRAY, KeySize: 4, ValueSize: 1 << 20, MaxEntries: 1 << 12}, linuxerr.ENOMEM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := r.NewMap(tc.opts); err != tc.want {
				t.Errorf("NewMap(%+v): got %v, want %v", tc.opts, err, tc.want)
			}
		})
	}
	if got := r.Memory(); got != 0 {
		t.Errorf("failed maps left %d bytes charged", got)
	}
}

func TestHashMap(t *testing.T) {
	r := newTestRegistry()
	m, err := r.NewMap(MapOptions{Type: linux.BPF_MAP_TYPE_HASH, KeySize: 4, ValueSize: 8, MaxEntries: 3, Name: "h"})
	if err != nil {
		t.Fatalf("NewMap: %v", err)
	}
	if m.ID() == 0 {
		t.Errorf("map has ID 0")
	}
	val := func(v byte) []byte { return bytes.Repeat([]byte{v}, 8) }
	if _, err := m.Lookup(u32(1)); err != linuxerr.ENOENT {
		t.Errorf("Lookup of a missing key: got %v, want ENOENT", err)
	}
	if err := m.Update(u32(1), val(1), linux.BPF_EXIST); err != linuxerr.ENOENT {
		t.Errorf("Update(BPF_EXIST) of a missing key: got %v, want ENOENT", err)
	}
	for k := uint32(1); k <= 3; k++ {
		if err := m.Update(u32(k), val(byte(k)), linux.BPF_NOEXIST); err != nil {
			t.Fatalf("Update(%d): %v", k, err)
		}
	}
	if err := m.Update(u32(4), val(4), linux.BPF_ANY); err != linuxerr.E2BIG {
		t.Errorf("Update of a full map: got %v, want E2BIG", err)
	}
	if err := m.Update(u32(2), val(2), linux.BPF_NOEXIST); err != linuxerr.EEXIST {
		t.Errorf("Update(BPF_NOEXIST) of a present key: got %v, want EEXIST", err)
	}
	if err := m.Update(u32(2), val(9), linux.BPF_EXIST); err != nil {
		t.Errorf("Update(BPF_EXIST): %v", err)
	}
	if got, err := m.Lookup(u32(2)); err != nil || !bytes.Equal(got, val(9)) {
		t.Errorf("Lookup(2): got %v, %v, want %v", got, err, val(9))
	}
	if err := m.Update(u32(2), val(9), 7); err != linuxerr.EINVAL {
		t.Errorf("Update with bad flags: got %v, want EINVAL", err)
	}

	// Iteration visits every key once, even as it deletes the current key.
	var seen []uint32
	key, err := m.NextKey(nil)
	for err == nil {
		next, nerr := m.NextKey(key)
		seen = append(seen, binary.LittleEndian.Uint32(key))
		if derr := m.Delete(key); derr != nil {
			t.Fatalf("Delete: %v", derr)
		}
		key, err = next, nerr
	}
	if err != linuxerr.ENOENT || len(seen) != 3 {
		t.Errorf("iteration: saw %v, ended with %v", seen, err)
	}
	if err := m.Delete(u32(1)); err != linuxerr.ENOENT {
		t.Errorf("Delete of a missing key: got %v, want ENOENT", err)
	}
	if _, err := m.NextKey(u32(7)); err != linuxerr.ENOENT {
		t.Errorf("NextKey of an empty map: got %v, want ENOENT", err)
	}

	m.DecRef()
	if got := r.Memory(); got != 0 {
		t.Errorf("released map left %d bytes charged", got)
	}
}

func TestLRUHashMapEvicts(t *testing.T) {
	r := newTestRegistry()
	m, err := r.NewMap(MapOptions{Type: linux.BPF_MAP_TYPE_LRU_HASH, KeySize: 4, ValueSize: 4, MaxEntries: 2})
	if err != nil {
		t.Fatalf("NewMap: %v", err)
	}
	defer m.DecRef()
	for _, k := range []uint32{1, 2} {
		if err := m.Update(u32(k), u32(k), linux.BPF_ANY); err != nil {
			t.Fatalf("Update(%d): %v", k, err)
		}
	}
	// Using 1 makes 2 the least recently used.
	if err := m.Update(u32(1), u32(10), linux.BPF_ANY); err != nil {
		t.Fatalf("Update(1): %v", err)
	}
	if err := m.Update(u32(3), u32(3), linux.BPF_ANY); err != nil {
		t.Fatalf("Update(3): %v", err)
	}
	if _, err := m.Lookup(u32(2)); err != linuxerr.ENOENT {
		t.Errorf("Lookup of the evicted key: got %v, want ENOENT", err)
	}
	for _, k := range []uint32{1, 3} {
		if _, err := m.Lookup(u32(k)); err != nil {
			t.Errorf("Lookup(%d): %v", k, err)
		}
	}
}

func TestArrayMap(t *testing.T) {
	r := newTestRegistry()
	m, err := r.NewMap(MapOptions{Type: linux.BPF_MAP_TYPE_ARRAY, KeySize: 4, ValueSize: 4, MaxEntries: 2})
	if err != nil {
		t.Fatalf("NewMap: %v", err)
	}
	defer m.DecRef()
	if got, err := m.Lookup(u32(1)); err != nil || !bytes.Equal(got, u32(0)) {
		t.Errorf("Lookup(1): got %v, %v, want zeroes", got, err)
	}
	if _, err := m.Lookup(u32(2)); err != linuxerr.ENOENT {
		t.Errorf("Lookup past the end: got %v, want ENOENT", err)
	}
	if err := m.Update(u32(1), u32(123), linux.BPF_ANY); err != nil {
		t.Errorf("Update(1): %v", err)
	}
	if got, _ := m.Lookup(u32(1)); !bytes.Equal(got, u32(123)) {
		t.Errorf("Lookup(1): got %v, want 123", got)
	}
	if err := m.Update(u32(0), u32(1), linux.BPF_NOEXIST); err != linuxerr.EEXIST {
		t.Errorf("Update(BPF_NOEXIST): got %v, want EEXIST", err)
	}
	if err := m.Update(u32(2), u32(1), linux.BPF_ANY); err != linuxerr.E2BIG {
		t.Errorf("Update past the end: got %v, want E2BIG", err)
	}
	if err := m.Delete(u32(0)); err != linuxerr.EINVAL {
		t.Errorf("Delete: got %v, want EINVAL", err)
	}
	for _, tc := range []struct {
		key  []byte
		want []byte
		err  error
	}{
		{nil, u32(0), nil},
		{u32(0), u32(1), nil},
		{u32(1), nil, linuxerr.ENOENT},
		{u32(5), u32(0), nil},
	} {
		got, err := m.NextKey(tc.key)
		if err != tc.err || !bytes.Equal(got, tc.want) {
			t.Errorf("NextKey(%v): got %v, %v, want %v, %v", tc.key, got, err, tc.want, tc.err)
		}
	}
}

func TestPerCPUMapValues(t *testing.T) {
	r := newTestRegistry()
	m, err := r.NewMap(MapOptions{Type: linux.BPF_MAP_TYPE_PERCPU_HASH, KeySize: 4, ValueSize: 4, MaxEntries: 2, NumCPUs: 3})
	if err != nil {
		t.Fatalf("NewMap: %v", err)
	}
	defer m.DecRef()
	if got, want := m.UserValueSize(), uint32(24); got != want {
		t.Errorf("UserValueSize: got %d, want %d", got, want)
	}
}

func TestFreeze(t *testing.T) {
	r := newTestRegistry()
	m, err := r.NewMap(MapOptions{Type: linux.BPF_MAP_TYPE_HASH, KeySize: 4, ValueSize: 4, MaxEntries: 2})
	if err != nil {
		t.Fatalf("NewMap: %v", err)
	}
	defer m.DecRef()
	if err := m.Freeze(); err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if !m.Frozen() {
		t.Errorf("map isn't frozen")
	}
	if err := m.Freeze(); err != linuxerr.EBUSY {
		t.Errorf("second Freeze: got %v, want EBUSY", err)
	}
}

func TestProgramRefs(t *testing.T) {
	r := newTestRegistry()
	m, err := r.NewMap(MapOptions{Type: linux.BPF_MAP_TYPE_HASH, KeySize: 4, ValueSize: 4, MaxEntries: 2})
	if err != nil {
		t.Fatalf("NewMap: %v", err)
	}
	insns := []linux.EBPFInstruction{
		{Code: ldImm64, Registers: linux.BPF_PSEUDO_MAP_FD << 4, Immediate: 5},
		{},
		{Code: 0xb7}, // mov r0, 0
		{Code: 0x95}, // exit
	}
	fds, ok := MapFDs(insns)
	if !ok || len(fds) != 1 || fds[0] != 5 {
		t.Errorf("MapFDs: got %v, %v, want [5]", fds, ok)
	}
	if _, ok := MapFDs(insns[:1]); ok {
		t.Errorf("MapFDs accepted a truncated BPF_LD_IMM64")
	}
	p, err := r.NewProgram(ProgramOptions{Type: linux.BPF_PROG_TYPE_SOCKET_FILTER, Instructions: insns, Maps: []*Map{m}})
	if err != nil {
		t.Fatalf("NewProgram: %v", err)
	}
	// The tag doesn't depend on map file descriptors.
	insns2 := append([]linux.EBPFInstruction(nil), insns...)
	insns2[0].Immediate = 9
	if programTag(insns2) != p.Tag() {
		t.Errorf("tag depends on the map's file descriptor")
	}
	if ids := p.MapIDs(); len(ids) != 1 || ids[0] != m.ID() {
		t.Errorf("MapIDs: got %v, want [%d]", ids, m.ID())
	}
	m.DecRef()
	if err := m.Update(u32(1), u32(1), linux.BPF_ANY); err != nil {
		t.Errorf("map of a live program was released: %v", err)
	}
	p.DecRef()
	if got := r.Memory(); got != 0 {
		t.Errorf("released objects left %d bytes charged", got)
	}
}
