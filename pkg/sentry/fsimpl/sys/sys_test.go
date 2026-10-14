// Copyright 2024 The gVisor Authors.
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

package sys

import (
	"testing"
)

func TestFullCPUMask(t *testing.T) {
	for _, test := range []struct {
		cores uint
		want  string
	}{
		{1, "1"},
		{2, "3"},
		{3, "7"},
		{4, "f"},
		{5, "1f"},
		{32, "ffffffff"},
		{33, "1,ffffffff"},
		{36, "f,ffffffff"},
		{37, "1f,ffffffff"},
		{64, "ffffffff,ffffffff"},
		{65, "1,ffffffff,ffffffff"},
	} {
		if got := fullCPUMask(test.cores); got != test.want {
			t.Errorf("fullCPUMask(%d): got %s, want %s", test.cores, got, test.want)
		}
	}
}

func TestOneCPUMask(t *testing.T) {
	for _, test := range []struct {
		i     uint
		cores uint
		want  string
	}{
		{0, 1, "1"},
		{0, 4, "1"},
		{1, 4, "2"},
		{2, 4, "4"},
		{3, 4, "8"},
		{0, 5, "01"},
		{4, 5, "10"},
		{0, 32, "00000001"},
		{26, 32, "04000000"},
		{0, 33, "0,00000001"},
		{31, 33, "0,80000000"},
		{32, 33, "1,00000000"},
		{0, 64, "00000000,00000001"},
		{31, 64, "00000000,80000000"},
		{32, 64, "00000001,00000000"},
		{63, 64, "80000000,00000000"},
		{0, 65, "0,00000000,00000001"},
		{31, 65, "0,00000000,80000000"},
		{32, 65, "0,00000001,00000000"},
		{63, 65, "0,80000000,00000000"},
		{64, 65, "1,00000000,00000000"},
	} {
		if got := oneCPUMask(test.i, test.cores); got != test.want {
			t.Errorf("oneCPUMask(%d, %d): got %s, want %s", test.i, test.cores, got, test.want)
		}
	}
}

func TestCPUMaskString(t *testing.T) {
	for _, cores := range []uint{1, 2, 4, 5, 32, 33, 36, 64, 65, 96, 256} {
		all := make([]int, cores)
		for i := range all {
			all[i] = i
		}
		if got, want := cpuMaskString(all, cores), fullCPUMask(cores); got != want {
			t.Errorf("cpuMaskString(all of %d): got %s, want %s", cores, got, want)
		}
		for _, i := range []uint{0, min(1, cores-1), cores / 2, cores - 1} {
			if got, want := cpuMaskString([]int{int(i)}, cores), oneCPUMask(i, cores); got != want {
				t.Errorf("cpuMaskString([%d], %d): got %s, want %s", i, cores, got, want)
			}
		}
	}
	if got, want := cpuMaskString([]int{0, 1, 2, 3, 48, 49, 95}, 96), "80000000,00030000,0000000f"; got != want {
		t.Errorf("cpuMaskString(0-3,48-49,95 of 96): got %s, want %s", got, want)
	}
}
