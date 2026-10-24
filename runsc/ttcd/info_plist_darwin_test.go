// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import (
	"debug/macho"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInfoPlist checks that ttcd embeds its Info.plist, whose bundle
// identifier gives its GPU renderer caches of its own.
func TestInfoPlist(t *testing.T) {
	exe := filepath.Join(os.Getenv("TEST_SRCDIR"), os.Getenv("TEST_WORKSPACE"), "runsc/ttcd/ttcd_/ttcd")
	f, err := macho.Open(exe)
	if err != nil {
		t.Fatalf("opening ttcd: %v", err)
	}
	defer f.Close()
	s := f.Section("__info_plist")
	if s == nil || s.Seg != "__TEXT" {
		t.Fatal("ttcd has no __TEXT,__info_plist section")
	}
	data, err := s.Data()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "<string>com.nvidia.ttcd</string>") {
		t.Errorf("ttcd's Info.plist has no bundle identifier com.nvidia.ttcd:\n%s", data)
	}
}
