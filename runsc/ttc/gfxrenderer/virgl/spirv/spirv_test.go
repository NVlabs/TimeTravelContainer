// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package spirv

import "testing"

// instructions splits a module's words into its instructions' words, after its
// header.
func instructions(t *testing.T, words []uint32) [][]uint32 {
	t.Helper()
	if len(words) < 5 || words[0] != Magic || words[1] != Version {
		t.Fatalf("header %x", words[:5])
	}
	var insts [][]uint32
	for i := 5; i < len(words); {
		n := int(words[i] >> 16)
		if n == 0 || i+n > len(words) {
			t.Fatalf("instruction of %d words at %d of %d", n, i, len(words))
		}
		insts = append(insts, words[i:i+n])
		i += n
	}
	return insts
}

func TestTypesAndConstantsOnce(t *testing.T) {
	m := NewModule()
	f32 := m.TypeFloat(32)
	if m.TypeFloat(32) != f32 {
		t.Errorf("float made twice")
	}
	v4 := m.TypeVector(f32, 4)
	if m.TypeVector(m.TypeFloat(32), 4) != v4 {
		t.Errorf("vec4 made twice")
	}
	one := m.ConstantFloat(1)
	if m.ConstantFloat(1) != one || m.ConstantFloat(2) == one {
		t.Errorf("constants")
	}
	if m.ConstantUint(1) == m.ConstantInt(1) {
		t.Errorf("a uint and an int of 1 are one constant")
	}
	// Of decorations of their own.
	arr := m.TypeArrayNew(f32, m.ConstantUint(4))
	if m.TypeArrayNew(f32, m.ConstantUint(4)) == arr {
		t.Errorf("TypeArrayNew returned a type twice")
	}
	st := m.TypeStruct(f32)
	if m.TypeStruct(f32) == st {
		t.Errorf("TypeStruct returned a type twice")
	}
	if m.Capability(CapabilityShader); len(m.capabilities) != 1 {
		t.Errorf("capabilities %v", m.capabilities)
	}
}

func TestFunctionVariablesFirst(t *testing.T) {
	m := NewModule()
	void := m.TypeVoid()
	f32 := m.TypeFloat(32)
	ptr := m.TypePointer(StorageClassFunction, f32)
	f := m.Function(void, m.TypeFunction(void))
	f.Label()
	f.Op(OpFAdd, f32, uint32(m.ConstantFloat(1)), uint32(m.ConstantFloat(2)))
	v := f.Variable(ptr)
	f.Do(OpStore, uint32(v), uint32(m.ConstantFloat(3)))
	f.Do(OpReturn)
	f.End()
	m.EntryPoint(ExecutionModelGLCompute, f.ID(), "main")
	m.ExecutionMode(f.ID(), ExecutionModeLocalSize, 1, 1, 1)
	var ops []Op
	for _, inst := range instructions(t, m.Assemble()) {
		ops = append(ops, Op(inst[0]&0xffff))
	}
	// The variable follows the first label, before the instructions that
	// the block had.
	for i, op := range ops {
		if op == OpLabel {
			if i+2 >= len(ops) || ops[i+1] != OpVariable || ops[i+2] != OpFAdd {
				t.Errorf("after the label: %v", ops[i:])
			}
			return
		}
	}
	t.Errorf("no label: %v", ops)
}

func TestString(t *testing.T) {
	for s, n := range map[string]int{"": 1, "abc": 1, "main": 2, "GLSL.std.450": 4} {
		if w := String(s); len(w) != n {
			t.Errorf("String(%q) is %d words, want %d", s, len(w), n)
		}
	}
}
