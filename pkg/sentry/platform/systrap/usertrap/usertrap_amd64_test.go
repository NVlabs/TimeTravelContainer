// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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
