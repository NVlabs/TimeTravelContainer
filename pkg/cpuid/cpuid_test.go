// Copyright 2020 The gVisor Authors.
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

package cpuid

import (
	"bytes"
	"context"
	"encoding/binary"
	"math/rand"
	"os"
	"runtime"
	"testing"

	"gvisor.dev/gvisor/pkg/state"
)

func TestFeatureFromString(t *testing.T) {
	// Check that known features do match.
	for feature := range allFeatures {
		f, ok := FeatureFromString(feature.String())
		if f != feature || !ok {
			t.Errorf("got %v, %v want %v, true", f, ok, feature)
		}
	}

	// Check that "bad" doesn't match.
	f, ok := FeatureFromString("bad")
	if ok {
		t.Errorf("got %v, %v want false", f, ok)
	}
}

func TestReadHwCap(t *testing.T) {
	// Make an auxv with fake entries
	const (
		auxvEntries = 16
		uint64Size  = 8
	)
	auxv := [auxvEntries * uint64Size * 2]byte{}

	hwCap1Idx := rand.Intn(auxvEntries)
	hwCap2Idx := rand.Intn(auxvEntries)
	if hwCap1Idx == hwCap2Idx {
		hwCap2Idx = (hwCap2Idx + 1 + rand.Intn(auxvEntries-2)) % auxvEntries
	}
	// Set the entries we are interested in to not 0.
	hwCap1Val := 1 + uint64(rand.Int63())
	hwCap2Val := 1 + uint64(rand.Int63())

	binary.LittleEndian.PutUint64(auxv[hwCap1Idx*uint64Size*2:], _AT_HWCAP)
	binary.LittleEndian.PutUint64(auxv[hwCap1Idx*uint64Size*2+8:], hwCap1Val)
	binary.LittleEndian.PutUint64(auxv[hwCap2Idx*uint64Size*2:], _AT_HWCAP2)
	binary.LittleEndian.PutUint64(auxv[hwCap2Idx*uint64Size*2+8:], hwCap2Val)

	file, err := os.CreateTemp(t.TempDir(), "fake-self-auxv")
	if err != nil {
		t.Errorf("failed to create file: %v", err)
	}
	_, err = file.Write(auxv[:])
	if err != nil {
		t.Errorf("failed to write to file: %v", err)
	}
	err = file.Close()
	if err != nil {
		t.Errorf("failed to close file: %v", err)
	}

	c, err := readHWCap(file.Name())
	if err != nil {
		t.Errorf("readHwCap got err %v, want nil", err)
	}
	if c.hwCap1 != hwCap1Val {
		t.Errorf("c.hwCap1 got %d, want %d", c.hwCap1, hwCap1Val)
	}
	if c.hwCap2 != hwCap2Val {
		t.Errorf("c.hwCap2 got %d, want %d", c.hwCap2, hwCap2Val)
	}
}

func TestReadingSelfProcAuxv(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("/proc/self/auxv is Linux-specific")
	}
	_, err := readHWCap("/proc/self/auxv")
	if err != nil {
		t.Errorf("got %v, expected nil", err)
	}
}

func TestFeatureSetStateRoundTripIncludesHWCap(t *testing.T) {
	source := &FeatureSet{
		hwCap:      hwCap{hwCap1: 0x123456789abcdef0, hwCap2: 0xfedcba9876543210},
		cpuFreqMHz: 2400,
		cpuImplHex: 0x61,
		cpuArchDec: 8,
		cpuVarHex:  2,
		cpuPartHex: 0x23,
		cpuRevDec:  1,
	}
	var image bytes.Buffer
	if _, err := state.Save(context.Background(), &image, source); err != nil {
		t.Fatalf("saving FeatureSet: %v", err)
	}
	restored := &FeatureSet{}
	if _, err := state.Load(context.Background(), bytes.NewReader(image.Bytes()), restored); err != nil {
		t.Fatalf("loading FeatureSet: %v", err)
	}
	if *restored != *source {
		t.Fatalf("restored FeatureSet = %+v, want %+v", *restored, *source)
	}
}

func TestMain(m *testing.M) {
	Initialize()
	os.Exit(m.Run())
}
