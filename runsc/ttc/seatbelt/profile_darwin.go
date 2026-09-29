// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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
