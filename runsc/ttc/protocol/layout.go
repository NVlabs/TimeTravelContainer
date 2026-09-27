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
