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

import "strings"

// hostHelpText returns help text as it applies to this host: native macOS
// runs only the Hypervisor.framework platform.
func hostHelpText(text string) string {
	return strings.ReplaceAll(text,
		"--platform systrap|kvm          Execution platform (default: systrap)",
		"--platform hvf                  Execution platform (default: hvf)")
}
