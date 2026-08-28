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

package boot

import (
	"testing"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/tcpip"
)

type nestedNetworkTestIDProvider struct {
	next uint64
}

func (p *nestedNetworkTestIDProvider) UniqueID() uint64 {
	p.next++
	return p.next
}

func TestNestedNetworkStackStartsWithUnconfiguredLoopback(t *testing.T) {
	creator := sandboxNetstackCreator{
		clock: tcpip.NewStdClock(),
		uid:   &nestedNetworkTestIDProvider{},
	}
	stack, err := creator.CreateStack()
	if err != nil {
		t.Fatalf("CreateStack: %v", err)
	}
	defer stack.Destroy()

	ifaces := stack.Interfaces()
	loopback, ok := ifaces[linux.LOOPBACK_IFINDEX]
	if !ok {
		t.Fatalf("interfaces = %+v, want loopback index %d", ifaces, linux.LOOPBACK_IFINDEX)
	}
	if loopback.Name != DefaultLoopbackLink.Name {
		t.Fatalf("loopback name = %q, want %q", loopback.Name, DefaultLoopbackLink.Name)
	}
	if got := stack.InterfaceAddrs()[linux.LOOPBACK_IFINDEX]; len(got) != 0 {
		t.Fatalf("new namespace loopback addresses = %+v, want none", got)
	}
	if got := stack.RouteTable(); len(got) != 0 {
		t.Fatalf("new namespace routes = %+v, want none", got)
	}
}
