// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package protocol

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// A daemon keeps every file of its own in its root, which identifies it: the
// TTC_ROOT of a standalone daemon, or the node-local TTC_LOCAL_ROOT of a
// daemon of a cluster. A client finds the daemon's socket in the same root.

const (
	// SocketName is the name of a daemon's API socket in its root.
	SocketName = "ttcd.sock"
	// LogName is the name of a daemon's log in its root.
	LogName = "ttcd.log"
)

// DaemonRoot returns the root of the daemon that the environment getenv reads
// selects: TTC_LOCAL_ROOT if it is set, as on a node of a cluster, and
// otherwise TTC_ROOT.
func DaemonRoot(getenv func(string) string) (string, error) {
	for _, name := range []string{"TTC_LOCAL_ROOT", "TTC_ROOT"} {
		root := strings.TrimSpace(getenv(name))
		if root == "" {
			continue
		}
		if !filepath.IsAbs(root) {
			return "", fmt.Errorf("%s %q is not an absolute path", name, root)
		}
		return filepath.Clean(root), nil
	}
	return "", errors.New("TTC_ROOT is not set; set it to the root of the ttcd to use, and TTC_LOCAL_ROOT too on a node of a cluster")
}

// DaemonSocket returns the path of the API socket of the daemon whose root is
// root.
func DaemonSocket(root string) string {
	return filepath.Join(root, SocketName)
}

// DaemonLog returns the path of the log of the daemon whose root is root.
func DaemonLog(root string) string {
	return filepath.Join(root, LogName)
}
