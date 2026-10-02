// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Binary ttcd is the rootless TimeTravelContainer per-user control service.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gvisor.dev/gvisor/runsc/cli/maincli"
	"gvisor.dev/gvisor/runsc/processutil"
	"gvisor.dev/gvisor/runsc/ttc/cmd"
	"gvisor.dev/gvisor/runsc/ttc/copyguest"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer"
	"gvisor.dev/gvisor/runsc/ttc/hostbin"
	"gvisor.dev/gvisor/runsc/version"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == cmd.GFXRendererArg {
		// ttcd is its GPU renderer on macOS (see cmd.GFXRendererArg).
		os.Exit(gfxrenderer.Main(os.Args[2:], []string{cmd.GFXRendererArg}))
	}
	cmd.SetGuestCopyHelper(copyguest.Binary())
	cmd.SetCarriedHelpers(hostbin.GFXRenderer(), hostbin.CUDACheckpoint())
	if len(os.Args) >= 2 && os.Args[1] == "_cp-helper" {
		os.Exit(cmd.RunCopyHelper(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	if len(os.Args) >= 2 && os.Args[1] == "_auto-remove-client" {
		os.Exit(cmd.RunAutoRemoveClient(os.Args[2:], os.Stdout, os.Stderr))
	}
	if len(os.Args) >= 2 && os.Args[1] == "_lease-guardian" {
		os.Exit(cmd.RunLeaseGuardian(os.Args[2:], os.Stderr))
	}
	if len(os.Args) >= 2 && os.Args[1] == "_shared-fs-relay" {
		os.Exit(cmd.RunSharedFilesystemRelay(os.Args[2:], os.Stderr))
	}
	if len(os.Args) >= 2 && os.Args[1] == "_shared-memory-relay" {
		os.Exit(cmd.RunSharedMemoryRelay(os.Args[2:], os.Stderr))
	}
	runscMode := os.Getenv("TTC_EMBEDDED_RUNTIME") == "1" || embeddedRuntimeArgv0(os.Args[0])
	if len(os.Args) >= 2 && os.Args[1] == "runsc" {
		runscMode = true
		os.Args = append([]string{"ttcd-runsc"}, os.Args[2:]...)
	}
	if runscMode {
		// runsc re-executes its own host path for sandbox helpers. The marker makes
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
	reapInheritedChildren()
	code := cmd.RunDaemon(os.Args[1:], os.Stdout, os.Stderr)
	if code == cmd.DaemonRestartExitCode {
		// Keep the process, and so a batch job running it, alive: a new image
		// starts with nothing held over from this one.
		executable, err := os.Executable()
		if err == nil {
			err = syscall.Exec(executable, os.Args, os.Environ())
		}
		os.Stderr.WriteString("ttcd: restarting: " + err.Error() + "\n")
	}
	os.Exit(code)
}

// reapInheritedChildren waits, in the background, for the children this
// process has as it starts. Only an earlier image of it, restarting by
// execution, can have left them: console drains, port forwarders, its lease
// guardian. Nothing in this image knows them, and each would otherwise stay a
// zombie once it exits.
func reapInheritedChildren() {
	children := inheritedChildren()
	if len(children) == 0 {
		return
	}
	go func() {
		for len(children) != 0 {
			time.Sleep(5 * time.Second)
			waiting := children[:0]
			for _, pid := range children {
				if reaped, err := syscall.Wait4(pid, nil, syscall.WNOHANG, nil); reaped == 0 && err == nil {
					waiting = append(waiting, pid)
				}
			}
			children = waiting
		}
	}()
}

func inheritedChildren() []int {
	pids, err := processutil.PIDs()
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var children []int
	for _, pid := range pids {
		if parent, err := processutil.ParentPID(pid); err == nil && parent == self {
			children = append(children, pid)
		}
	}
	return children
}

func embeddedRuntimeArgv0(argv0 string) bool {
	base := filepath.Base(argv0)
	return base == "runsc" ||
		strings.HasPrefix(base, "runsc-") ||
		strings.HasPrefix(base, "ttc-sentry[")
}
