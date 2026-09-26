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

func TestConvertXstateLayoutRelocatesAVX512Components(t *testing.T) {
	tests := []struct {
		name                       string
		feature                    uint64
		sourceOffset, targetOffset uint32
		size                       uint32
		fill                       byte
	}{
		{name: "avx", feature: cpuid.XSAVEFeatureAVX, sourceOffset: 576, targetOffset: 640, size: 256, fill: 0x22},
		{name: "avx512-opmask", feature: cpuid.XSAVEFeatureAVX512op, sourceOffset: 896, targetOffset: 960, size: 64, fill: 0x55},
		{name: "avx512-zmm0-15", feature: cpuid.XSAVEFeatureAVX512zmm0, sourceOffset: 1024, targetOffset: 1152, size: 512, fill: 0x66},
		{name: "avx512-zmm16-31", feature: cpuid.XSAVEFeatureAVX512zmm16, sourceOffset: 1664, targetOffset: 1792, size: 1024, fill: 0x77},
	}
	source := XstateLayout{UseXsave: true, Size: 3072}
	target := XstateLayout{UseXsave: true, Size: 3072}
	src := make([]byte, source.Size)
	dst := make([]byte, target.Size)
	savedBV := uint64(fxsaveBV)
	for _, test := range tests {
		bit := uint(0)
		for test.feature != uint64(1)<<bit {
			bit++
		}
		source.Components[bit] = XstateComponent{Offset: test.sourceOffset, Size: test.size}
		target.Components[bit] = XstateComponent{Offset: test.targetOffset, Size: test.size}
		copy(src[test.sourceOffset:test.sourceOffset+test.size], bytes.Repeat([]byte{test.fill}, int(test.size)))
		savedBV |= test.feature
	}

	convertXstateLayout(dst, src, source, target, savedBV)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := dst[test.targetOffset : test.targetOffset+test.size]
			if !bytes.Equal(got, bytes.Repeat([]byte{test.fill}, int(test.size))) {
				t.Fatalf("component was not relocated from offset %d to %d", test.sourceOffset, test.targetOffset)
			}
		})
	}
}
