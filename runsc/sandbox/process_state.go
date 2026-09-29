// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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
