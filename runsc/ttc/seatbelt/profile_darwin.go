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

// Package seatbelt confines a macOS process with the kernel's sandbox,
// Seatbelt, to a profile of its own, as the App Sandbox confines an app to
// Apple's.
package seatbelt

// DaemonProfile is the profile ttcd confines itself, and so every process it
// starts, to: its Sentries, gofers, and helpers. It is to macOS what the user
// namespace of a rootless sandbox is to Linux, a bound on what an attacker who
// gains code execution in them can reach of the host.
//
// It admits what TTC uses: all files, and the network, as containers and
// image registries need; the processes it starts and signals, and their
// state; and the services every process uses, which Apple's system.sb admits,
// among them trustd, which verifies registries' certificates. It denies the
// rest: devices, USB ones among them, which IOKit opens, and the other
// services of the system, such as those that manage disks, power, and the
// user's session and apps. Hypervisor.framework needs neither.
const DaemonProfile = `(version 1)
(deny default)
(import "system.sb")
(allow mach-bootstrap)
(allow syscall*)
(allow file*)
(allow network*)
(allow process-fork)
(allow process-exec)
(allow process-info*)
(allow signal)
`
