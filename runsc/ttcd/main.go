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

// Binary ttcd is the rootless TimeTravelContainer per-user control service.
package main

import (
	"os"
	"path/filepath"
	"strings"

	"gvisor.dev/gvisor/runsc/cli/maincli"
	"gvisor.dev/gvisor/runsc/ttc/cmd"
	"gvisor.dev/gvisor/runsc/version"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "_cp-helper" {
		os.Exit(cmd.RunCopyHelper(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	if len(os.Args) >= 2 && os.Args[1] == "_auto-remove-client" {
		os.Exit(cmd.RunAutoRemoveClient(os.Args[2:], os.Stdout, os.Stderr))
	}
	if len(os.Args) >= 2 && os.Args[1] == "_lease-guardian" {
		os.Exit(cmd.RunLeaseGuardian(os.Args[2:], os.Stderr))
	}
	runscMode := os.Getenv("TTC_EMBEDDED_RUNTIME") == "1" || embeddedRuntimeArgv0(os.Args[0])
	if len(os.Args) >= 2 && os.Args[1] == "runsc" {
		runscMode = true
		os.Args = append([]string{"ttcd-runsc"}, os.Args[2:]...)
	}
	if runscMode {
		// runsc re-executes /proc/self/exe for sandbox helpers. The marker makes
		// those children return to the embedded runtime instead of starting the
		// TTC control daemon.
		_ = os.Setenv("TTC_EMBEDDED_RUNTIME", "1")
		if filepath.Base(os.Args[0]) != "ttcd-runsc" {
			os.Args[0] = "ttcd-runsc"
		}
		maincli.Main()
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		// Keep this useful for both users and the historical //runsc:version_test
		// alias. The embedded runtime reports the same linked version.
		os.Stdout.WriteString("ttcd version " + version.Version() + "\n")
		return
	}
	os.Exit(cmd.RunDaemon(os.Args[1:], os.Stdout, os.Stderr))
}

func embeddedRuntimeArgv0(argv0 string) bool {
	base := filepath.Base(argv0)
	return base == "runsc" ||
		strings.HasPrefix(base, "runsc-") ||
		strings.HasPrefix(base, "ttc-sentry[")
}
