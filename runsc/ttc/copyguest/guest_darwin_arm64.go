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

package copyguest

import _ "embed"

// linuxARM64 is a pure-Go Linux executable. The host passes its open file
// descriptor to the Sentry, so copy works even for distroless images.
//
//go:embed cmd/copy_helper_linux_arm64
var linuxARM64 []byte

// Binary returns the copy helper matching the native macOS guest ISA.
func Binary() []byte { return linuxARM64 }
