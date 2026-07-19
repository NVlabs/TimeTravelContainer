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

//go:build amd64

package usertrap

import "testing"

func TestIsPatchableSyscall(t *testing.T) {
	const sysno = uintptr(202)
	valid := []byte{0xb8, 0xca, 0x00, 0x00, 0x00, 0x0f, 0x05}
	tests := []struct {
		name string
		code []byte
		no   uintptr
		want bool
	}{
		{name: "exact", code: valid, no: sysno, want: true},
		{name: "short", code: valid[:6], no: sysno},
		{name: "unrelated mov opcode", code: []byte{0xb9, 0xca, 0, 0, 0, 0x0f, 0x05}, no: sysno},
		{name: "different immediate", code: []byte{0xb8, 0xcb, 0, 0, 0, 0x0f, 0x05}, no: sysno},
		{name: "not syscall", code: []byte{0xb8, 0xca, 0, 0, 0, 0x0f, 0x04}, no: sysno},
		{name: "high syscall number", code: valid, no: uintptr(1) << 32},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isPatchableSyscall(test.code, test.no); got != test.want {
				t.Fatalf("isPatchableSyscall(%#v, %d) = %t, want %t", test.code, test.no, got, test.want)
			}
		})
	}
}
