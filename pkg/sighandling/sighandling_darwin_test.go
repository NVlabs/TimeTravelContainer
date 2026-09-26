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

package sighandling

import (
	"testing"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
)

func TestDarwinForwardedSignalsUseLinuxABI(t *testing.T) {
	got := make(map[unix.Signal]linux.Signal)
	for _, registration := range forwardedSignals() {
		host := registration.host.(unix.Signal)
		got[host] = registration.guest
	}
	if _, ok := got[unix.SIGURG]; ok {
		t.Fatal("Darwin SIGURG must not be forwarded; the Go runtime uses it for preemption")
	}
	for host, want := range map[unix.Signal]linux.Signal{
		unix.SIGBUS:  linux.SIGBUS,
		unix.SIGSYS:  linux.SIGSYS,
		unix.SIGCONT: linux.SIGCONT,
		unix.SIGUSR1: linux.SIGUSR1,
		unix.SIGUSR2: linux.SIGUSR2,
	} {
		if guest, ok := got[host]; !ok || guest != want {
			t.Errorf("host signal %v maps to Linux signal %v (present=%t), want %v", host, guest, ok, want)
		}
	}
}
