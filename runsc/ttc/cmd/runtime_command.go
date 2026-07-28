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

package cmd

import (
	"os/exec"
	"path/filepath"
)

// runscCommand invokes TTC's OCI runtime. Production installations embed that
// entry point in ttcd; an explicitly configured legacy/development runsc path
// remains directly executable for tests and upgrades.
func runscCommand(path string, args ...string) *exec.Cmd {
	if filepath.Base(path) == "ttcd" {
		args = append([]string{"runsc"}, args...)
	}
	return exec.Command(path, args...)
}
