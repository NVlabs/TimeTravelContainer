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
	"unsafe"

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

// IgnoreChildStop sets the SA_NOCLDSTOP flag, causing child processes to not
// generate SIGCHLD when they stop.
func IgnoreChildStop() error {
	var old sigactionDarwin
	if err := sigactionRaw(unix.SIGCHLD, nil, &old); err != nil {
		return err
	}
	old.flags |= darwinSANoChildStop
	return sigactionRaw(unix.SIGCHLD, &old, nil)
}

// ReplaceSignalHandler replaces the existing signal handler for the provided
// signal with the function pointer at `handler`. This bypasses the Go runtime
// signal handlers, and should only be used for low-level signal handlers where
// use of signal.Notify is not appropriate.
//
// It stores the value of the previously set handler in previous.
func ReplaceSignalHandler(sig unix.Signal, handler uintptr, previous *uintptr) error {
	var old sigactionDarwin
	if err := sigactionRaw(sig, nil, &old); err != nil {
		return err
	}
	if old.handler == 0 {
		return fmt.Errorf("previous handler for signal %x isn't set", sig)
	}
	*previous = old.handler
	old.handler = handler
	old.flags |= darwinSASiginfo
	return sigactionRaw(sig, &old, nil)
}

const (
	darwinSANoChildStop = 0x8
	darwinSASiginfo     = 0x40
)

// sigactionDarwin matches the 64-bit Darwin sigaction ABI used by both
// supported macOS architectures. Keep this host ABI separate from Linux's
// guest-visible SigAction layout.
type sigactionDarwin struct {
	handler uintptr
	mask    uint32
	flags   int32
}

func sigactionRaw(sig unix.Signal, act, old *sigactionDarwin) error {
	_, _, errno := unix.RawSyscall(
		unix.SYS_SIGACTION,
		uintptr(sig),
		uintptr(unsafe.Pointer(act)),
		uintptr(unsafe.Pointer(old)),
	)
	if errno != 0 {
		return errno
	}
	return nil
}
