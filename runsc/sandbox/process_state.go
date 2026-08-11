// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sandbox

import (
	"fmt"
	"strings"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/runsc/processutil"
	"gvisor.dev/gvisor/runsc/specutils"
)

func sandboxProcessArgv0(spec *specs.Spec, id string) string {
	name := ""
	if spec != nil {
		name = specutils.ContainerName(spec)
	}
	if name == "" {
		name = id
		if len(name) > 12 {
			name = name[:12]
		}
	}
	name = strings.Map(func(r rune) rune {
		if r < 0x21 || r > 0x7e {
			return '_'
		}
		return r
	}, name)
	return fmt.Sprintf("ttc-sentry[%s@family]", name)
}

// IsProcessRunning reports whether pid still has executable process state.
// kill(pid, 0) alone is insufficient: it succeeds for an unreaped zombie,
// which previously made forced TTC removal wait for its full timeout.
func IsProcessRunning(pid int) bool {
	return processutil.IsRunning(pid)
}
