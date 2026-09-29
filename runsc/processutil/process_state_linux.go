// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package processutil

import (
	"fmt"
	"os"
	"strings"
)

func processZombieState(pid int) (zombie, known bool) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false, false
	}
	// comm is parenthesized and may itself contain spaces or ')'. The state is
	// the byte after the final ") ".
	end := strings.LastIndex(string(stat), ") ")
	if end < 0 || end+2 >= len(stat) {
		return false, false
	}
	switch stat[end+2] {
	case 'Z', 'X', 'x':
		return true, true
	default:
		return false, true
	}
}
