// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package seatbelt confines a macOS process with the kernel's sandbox,
// Seatbelt, to a profile of its own, as the App Sandbox confines an app to
// Apple's.
package seatbelt

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
)

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

// RendererProfile returns the profile that ttc-gfx-renderer, TTC's GPU
// renderer, confines itself to before it reads anything from its Sentry. It
// decodes a guest's command streams with gfxstream, so it gets only what
// rendering takes:
//
//   - reading Apple's system files; the package prefixes where Homebrew
//     installs the Vulkan loader and MoltenVK, /opt/homebrew and /usr/local;
//     and readable, the files of the renderer, gfxstream, and the loader,
//     and the metadata of the directories above them, which resolving
//     their paths reads;
//   - the GPU's IOKit user clients, and IOSurface's;
//   - POSIX shared memory of the names that the renderer and gfxstream give
//     it, "/ttcgfx." and "/gfxs.", random and unlinked at once;
//   - if scratch is not empty, reading and writing the directory scratch,
//     where the renderer takes and restores snapshots.
//
// It denies the rest, such as writing files, the network, starting
// processes, and the services of the system that Apple's system.sb does
// not admit: Metal needs none of them.
//
// A path with a control character, which the profile cannot spell, is an
// error.
func RendererProfile(readable []string, scratch string) (string, error) {
	var b strings.Builder
	b.WriteString(`(version 1)
(deny default)
(import "system.sb")
(allow file-read*
    (subpath "/System")
    (subpath "/usr/lib")
    (subpath "/usr/share")
    (subpath "/opt/homebrew")
    (subpath "/usr/local")`)
	ancestors := make(map[string]bool)
	if strings.IndexFunc(scratch, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("path %q has a control character", scratch)
	}
	for _, path := range readable {
		if strings.IndexFunc(path, unicode.IsControl) >= 0 {
			return "", fmt.Errorf("path %q has a control character", path)
		}
		fmt.Fprintf(&b, "\n    (literal %s)", quote(path))
		for dir := filepath.Dir(path); dir != "/" && dir != "."; dir = filepath.Dir(dir) {
			ancestors[dir] = true
		}
	}
	b.WriteString(")\n(allow file-read-metadata")
	for _, dir := range slices.Sorted(maps.Keys(ancestors)) {
		fmt.Fprintf(&b, "\n    (literal %s)", quote(dir))
	}
	b.WriteString(`)
(allow iokit-open
    (iokit-connection "IOAccelerator")
    (iokit-user-client-class "IOSurfaceRootUserClient"))
(allow ipc-posix-shm-read* ipc-posix-shm-write-create ipc-posix-shm-write-data ipc-posix-shm-write-unlink
    (ipc-posix-name-prefix "/ttcgfx.")
    (ipc-posix-name-prefix "/gfxs."))
`)
	if scratch != "" {
		fmt.Fprintf(&b, "(allow file-read* file-write* (subpath %s))\n", quote(scratch))
	}
	return b.String(), nil
}

// quote returns s as a string of the Sandbox Profile Language.
func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
