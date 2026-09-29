// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package console

import (
	"path/filepath"
	"testing"
)

func TestWaitStatusRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "init.wait-status")
	if _, found, err := ReadWaitStatus(path); err != nil || found {
		t.Fatalf("missing ReadWaitStatus=(found=%t, err=%v)", found, err)
	}
	if err := WriteWaitStatus(path, 9); err != nil {
		t.Fatal(err)
	}
	status, found, err := ReadWaitStatus(path)
	if err != nil || !found || status != 9 {
		t.Fatalf("ReadWaitStatus=(%d,%t,%v), want (9,true,nil)", status, found, err)
	}
}
