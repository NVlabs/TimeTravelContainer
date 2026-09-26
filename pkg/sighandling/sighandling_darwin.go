// Copyright 2021 The gVisor Authors.
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
// +build darwin

package sighandling

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
)

func forwardedSignals() []forwardedSignal {
	// Signal numbers after SIGABRT diverge substantially between Darwin and
	// Linux. Register by Darwin semantic identity and deliver Linux ABI values.
	// Host SIGURG is deliberately absent because the Go runtime uses it for
	// asynchronous preemption; SIGPIPE and SIGCHLD are host implementation
	// details rather than external container signals.
	return []forwardedSignal{
		{host: os.Signal(unix.SIGHUP), guest: linux.SIGHUP},
		{host: os.Signal(unix.SIGINT), guest: linux.SIGINT},
		{host: os.Signal(unix.SIGQUIT), guest: linux.SIGQUIT},
		{host: os.Signal(unix.SIGILL), guest: linux.SIGILL},
		{host: os.Signal(unix.SIGTRAP), guest: linux.SIGTRAP},
		{host: os.Signal(unix.SIGABRT), guest: linux.SIGABRT},
		{host: os.Signal(unix.SIGBUS), guest: linux.SIGBUS},
		{host: os.Signal(unix.SIGFPE), guest: linux.SIGFPE},
		{host: os.Signal(unix.SIGKILL), guest: linux.SIGKILL},
		{host: os.Signal(unix.SIGSEGV), guest: linux.SIGSEGV},
		{host: os.Signal(unix.SIGSYS), guest: linux.SIGSYS},
		{host: os.Signal(unix.SIGALRM), guest: linux.SIGALRM},
		{host: os.Signal(unix.SIGTERM), guest: linux.SIGTERM},
		{host: os.Signal(unix.SIGSTOP), guest: linux.SIGSTOP},
		{host: os.Signal(unix.SIGTSTP), guest: linux.SIGTSTP},
		{host: os.Signal(unix.SIGCONT), guest: linux.SIGCONT},
		{host: os.Signal(unix.SIGTTIN), guest: linux.SIGTTIN},
		{host: os.Signal(unix.SIGTTOU), guest: linux.SIGTTOU},
		{host: os.Signal(unix.SIGIO), guest: linux.SIGIO},
		{host: os.Signal(unix.SIGXCPU), guest: linux.SIGXCPU},
		{host: os.Signal(unix.SIGXFSZ), guest: linux.SIGXFSZ},
		{host: os.Signal(unix.SIGVTALRM), guest: linux.SIGVTALRM},
		{host: os.Signal(unix.SIGPROF), guest: linux.SIGPROF},
		{host: os.Signal(unix.SIGWINCH), guest: linux.SIGWINCH},
		{host: os.Signal(unix.SIGUSR1), guest: linux.SIGUSR1},
		{host: os.Signal(unix.SIGUSR2), guest: linux.SIGUSR2},
	}
}

// IgnoreChildStop is a no-op on Darwin.
//
// On Linux it sets SA_NOCLDSTOP so that the Sentry's stub and ptrace
// subprocesses do not deliver SIGCHLD whenever they stop; a Darwin Sentry has
// no such children. Re-installing the Go runtime's handler here would also be
// unsafe: the raw sigaction syscall does not report the libc signal
// trampoline, so the handler cannot be re-registered without it, and a
// disposition with a bogus trampoline crashes the process on the next
// SIGCHLD.
func IgnoreChildStop() error {
	return nil
}

// ReplaceSignalHandler is unsupported on Darwin, for the same reason: without
// libc's sigaction the replacement handler cannot be given a valid signal
// trampoline. Its callers are Linux-only platforms.
func ReplaceSignalHandler(sig unix.Signal, _ uintptr, _ *uintptr) error {
	return fmt.Errorf("replacing the handler for signal %d is unsupported on Darwin", sig)
}
