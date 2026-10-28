// Copyright 2018 The gVisor Authors.
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

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package state

import (
	"reflect"
	"unsafe"
)

// arrayFromSlice constructs a new pointer to the slice data.
//
// It would be similar to the following:
//
//	x := make([]Foo, l, c)
//	a := ([l]Foo*)(unsafe.Pointer(x[0]))
//
// The array includes the slice's unused capacity, which it does not clear:
// another slice of the same array may hold live elements there, and a save
// must not change what it saves (the source of a live fork runs on).
func arrayFromSlice(obj reflect.Value) reflect.Value {
	return reflect.NewAt(
		reflect.ArrayOf(obj.Cap(), obj.Type().Elem()),
		unsafe.Pointer(obj.Pointer()))
}

// byteType is the type of the elements of arrays of bytes.
var byteType = reflect.TypeOf(byte(0))

// byteArrayBytes returns the bytes of obj, a non-empty array of bytes whose
// elements are addressable, without copying them.
func byteArrayBytes(obj reflect.Value) []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(obj.Index(0).UnsafeAddr())), obj.Len())
}
