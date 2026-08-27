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
