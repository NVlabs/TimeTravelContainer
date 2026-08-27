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

package monitorabi

import "testing"

func TestCommandRoundTrip(t *testing.T) {
	want := Command{
		Sequence:       7,
		Opcode:         OpcodeApply,
		AddressSpaceID: 3,
		Operations: []Operation{
			{Type: OperationMap, Access: AccessRead | AccessWrite, VA: 0x4000, GPA: 0x8000, Length: 0x2000},
			{Type: OperationProtect, Access: AccessRead, VA: 0x4000, Length: 0x1000},
			{Type: OperationUnmap, VA: 0x5000, Length: 0x1000},
		},
	}
	b, err := MarshalCommand(want)
	if err != nil {
		t.Fatalf("MarshalCommand: %v", err)
	}
	got, err := UnmarshalCommand(b)
	if err != nil {
		t.Fatalf("UnmarshalCommand: %v", err)
	}
	if got.Sequence != want.Sequence || got.Opcode != want.Opcode || got.AddressSpaceID != want.AddressSpaceID || len(got.Operations) != len(want.Operations) {
		t.Fatalf("round trip got %+v, want %+v", got, want)
	}
	for i := range want.Operations {
		if got.Operations[i] != want.Operations[i] {
			t.Errorf("operation %d got %+v, want %+v", i, got.Operations[i], want.Operations[i])
		}
	}
}

func TestPoolExpansionRoundTrip(t *testing.T) {
	want := Command{
		Sequence:   9,
		Opcode:     OpcodeExpandPageTablePool,
		PoolGPA:    0x1200000,
		PoolLength: 0x1000000,
	}
	b, err := MarshalCommand(want)
	if err != nil {
		t.Fatalf("MarshalCommand: %v", err)
	}
	got, err := UnmarshalCommand(b)
	if err != nil {
		t.Fatalf("UnmarshalCommand: %v", err)
	}
	if got.Sequence != want.Sequence || got.Opcode != want.Opcode || got.PoolGPA != want.PoolGPA || got.PoolLength != want.PoolLength {
		t.Fatalf("round trip got %+v, want %+v", got, want)
	}
}

func TestMalformedCommands(t *testing.T) {
	tests := []Command{
		{Sequence: 0, Opcode: OpcodeCreateAddressSpace, AddressSpaceID: 1},
		{Sequence: 1, Opcode: OpcodeApply, AddressSpaceID: 1},
		{Sequence: 1, Opcode: OpcodeApply, AddressSpaceID: 1, Operations: []Operation{{Type: OperationMap, Access: AccessRead, VA: 1, GPA: 0x4000, Length: 0x1000}}},
		{Sequence: 1, Opcode: OpcodeApply, AddressSpaceID: 1, Operations: []Operation{{Type: OperationUnmap, Access: AccessRead, VA: 0x1000, Length: 0x1000}}},
		{Sequence: 1, Opcode: OpcodeExpandPageTablePool, PoolGPA: 0x1000},
		{Sequence: 1, Opcode: OpcodeExpandPageTablePool, PoolGPA: 1, PoolLength: 0x4000},
	}
	for _, test := range tests {
		if _, err := MarshalCommand(test); err == nil {
			t.Errorf("MarshalCommand(%+v) unexpectedly succeeded", test)
		}
	}

	valid, err := MarshalCommand(Command{Sequence: 1, Opcode: OpcodeActivate, AddressSpaceID: 1})
	if err != nil {
		t.Fatal(err)
	}
	valid[0] ^= 1
	if _, err := UnmarshalCommand(valid); err == nil {
		t.Error("UnmarshalCommand accepted bad magic")
	}
	valid, err = MarshalCommand(Command{Sequence: 1, Opcode: OpcodeActivate, AddressSpaceID: 1})
	if err != nil {
		t.Fatal(err)
	}
	valid[56] = 1
	if _, err := UnmarshalCommand(valid); err == nil {
		t.Error("UnmarshalCommand accepted nonzero reserved bytes")
	}

	response := MarshalResponse(Response{Sequence: 1, Status: StatusOK})
	response[10] = 1
	if _, err := UnmarshalResponse(response); err == nil {
		t.Error("UnmarshalResponse accepted nonzero reserved bytes")
	}
}
