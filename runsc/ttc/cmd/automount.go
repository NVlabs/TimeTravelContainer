// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// triggerBindMountSources looks up, from ttcd's own mount namespace, the
// source of every bind mount of the OCI spec in bundleDir. A source under an
// autofs map, as /home is on many batch farms, is mounted only when something
// looks it up, and the map's daemon mounts it only for a lookup from the
// host's mount namespace: a rootless sandbox, in a user and mount namespace of
// its own, cannot have it mounted, and failed to start ("cannot read client
// sync file: waiting for sandbox to start: EOF") on a node where nothing had
// used the source for the map's timeout.
func triggerBindMountSources(bundleDir string) {
	data, err := os.ReadFile(filepath.Join(bundleDir, "config.json"))
	if err != nil {
		return
	}
	var spec specs.Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return
	}
	for _, mount := range spec.Mounts {
		if !filepath.IsAbs(mount.Source) {
			continue
		}
		if mount.Type != "bind" && !slices.Contains(mount.Options, "bind") && !slices.Contains(mount.Options, "rbind") {
			continue
		}
		_, _ = os.Stat(mount.Source)
	}
}

// bundleOf returns the bundle that runsc arguments args name, or "".
func bundleOf(args []string) string {
	for i, arg := range args {
		if arg == "--bundle" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
