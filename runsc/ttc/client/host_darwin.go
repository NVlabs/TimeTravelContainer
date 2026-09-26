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

//go:build darwin

package client

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
)

// hostHelpText returns help text as it applies to this host: native macOS
// runs only the Hypervisor.framework platform.
func hostHelpText(text string) string {
	return strings.ReplaceAll(text,
		"--platform systrap|kvm          Execution platform (default: systrap)",
		"--platform hvf                  Execution platform (default: hvf)")
}

// explainDaemonExecFailure explains err, the failure to run the ttcd at path.
// macOS kills an executable whose code signature it rejects as soon as it
// starts, which "signal: killed" alone does not say.
func explainDaemonExecFailure(path string, err error) error {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return err
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		return err
	}
	detail := "although codesign accepts it on disk"
	if output, verifyErr := exec.Command("/usr/bin/codesign", "--verify", "--strict", path).CombinedOutput(); verifyErr != nil {
		detail = "codesign reports: " + strings.Join(strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(output)), "\n", "; ")), " ")
	}
	return fmt.Errorf("macOS killed %s as it started, as it does an executable whose code signature it rejects (%s). Reinstall ttc and ttcd with tools/ttc/install_macos.sh, or sign ttcd: codesign --force --sign - --entitlements runsc/hvf.entitlements %s", path, detail, path)
}
