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

package transport

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestHostConnectedEndpointShutdownAfterPeerDisconnect(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("Socketpair: %v", err)
	}
	defer unix.Close(fds[0])

	ep, serr := NewHostConnectedEndpoint(fds[0], "test")
	if serr != nil {
		t.Fatalf("NewHostConnectedEndpoint: %v", serr)
	}
	if err := unix.Close(fds[1]); err != nil {
		t.Fatalf("closing peer: %v", err)
	}

	// Darwin returns ENOTCONN here after the peer disconnects. Closing a
	// port-forwarded connection must not turn that harmless condition into a
	// Sentry-wide panic.
	ep.CloseSend()
	ep.CloseRecv()
}
