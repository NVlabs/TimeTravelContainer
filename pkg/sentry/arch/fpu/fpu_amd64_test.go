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

package fpu

import (
	"bytes"
	"testing"

	"gvisor.dev/gvisor/pkg/cpuid"
)

func TestConvertXstateLayoutRelocatesComponents(t *testing.T) {
	const componentSize = 256
	source := XstateLayout{UseXsave: true, Size: 1024}
	target := XstateLayout{UseXsave: true, Size: 1280}
	source.Components[2] = XstateComponent{Offset: 576, Size: componentSize}
	target.Components[2] = XstateComponent{Offset: 832, Size: componentSize}
	src := make([]byte, source.Size)
	dst := make([]byte, target.Size)
	for i := 0; i < 512; i++ {
		src[i] = byte(i)
	}
	want := bytes.Repeat([]byte{0xa5}, componentSize)
	copy(src[576:], want)
	convertXstateLayout(dst, src, source, target, fxsaveBV|cpuid.XSAVEFeatureAVX)
	if !bytes.Equal(dst[:512], src[:512]) {
		t.Fatal("legacy x87/SSE state changed")
	}
	if !bytes.Equal(dst[832:832+componentSize], want) {
		t.Fatal("AVX component was not relocated")
	}
	if bytes.Equal(dst[576:576+componentSize], want) {
		t.Fatal("AVX component remained at its source offset")
	}
}
