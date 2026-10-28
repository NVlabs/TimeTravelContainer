// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package boot

import (
	"slices"
	"testing"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/sentry/inet"
	"gvisor.dev/gvisor/pkg/sentry/socket/netlink/nlmsg"
	"gvisor.dev/gvisor/pkg/sentry/socket/netstack"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/runsc/boot/sentryapi"
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
	if loopback.Name != sentryapi.DefaultLoopbackLink.Name {
		t.Fatalf("loopback name = %q, want %q", loopback.Name, sentryapi.DefaultLoopbackLink.Name)
	}
	if loopback.Flags&linux.IFF_LOOPBACK == 0 || loopback.Flags&(linux.IFF_UP|linux.IFF_RUNNING) != 0 {
		t.Fatalf("new namespace loopback flags = %#x, want down loopback", loopback.Flags)
	}
	if got := stack.InterfaceAddrs()[linux.LOOPBACK_IFINDEX]; len(got) != 0 {
		t.Fatalf("new namespace loopback addresses = %+v, want none", got)
	}
	if got := stack.RouteTable(); len(got) != 0 {
		t.Fatalf("new namespace routes = %+v, want none", got)
	}
}

func newNestedTestStack(t *testing.T) *netstack.Stack {
	t.Helper()
	creator := sandboxNetstackCreator{
		clock: tcpip.NewStdClock(),
		uid:   &nestedNetworkTestIDProvider{},
	}
	s, err := creator.CreateStack()
	if err != nil {
		t.Fatalf("CreateStack: %v", err)
	}
	t.Cleanup(s.Destroy)
	return s.(*netstack.Stack)
}

// setLoopbackFlags sends the RTM_NEWLINK request that "ip link set lo up|down"
// and Bubblewrap use.
func setLoopbackFlags(t *testing.T, s *netstack.Stack, flags, change uint32) {
	t.Helper()
	msg := nlmsg.NewMessage(linux.NetlinkMessageHeader{Type: linux.RTM_NEWLINK, Flags: linux.NLM_F_REQUEST | linux.NLM_F_ACK})
	msg.Put(&linux.InterfaceInfoMessage{Family: linux.AF_UNSPEC, Index: linux.LOOPBACK_IFINDEX, Flags: flags, Change: change})
	if err := s.SetInterface(context.Background(), msg); err != nil {
		t.Fatalf("SetInterface(flags=%#x, change=%#x): %v", flags, change, err)
	}
}

func loopbackAddrs(s *netstack.Stack) []string {
	var addrs []string
	for _, addr := range s.InterfaceAddrs()[linux.LOOPBACK_IFINDEX] {
		addrs = append(addrs, tcpip.AddressWithPrefix{Address: tcpip.AddrFromSlice(addr.Addr), PrefixLen: int(addr.PrefixLen)}.String())
	}
	slices.Sort(addrs)
	return addrs
}

func loopbackFlags(s *netstack.Stack) uint32 {
	return s.Interfaces()[linux.LOOPBACK_IFINDEX].Flags
}

func routesTo127(s *netstack.Stack) bool {
	route, err := s.Stack.FindRoute(0, tcpip.Address{}, tcpip.AddrFrom4([4]byte{127, 0, 0, 1}), ipv4.ProtocolNumber, false)
	if err != nil {
		return false
	}
	route.Release()
	return true
}

func TestNestedLoopbackBringUpAssignsLinuxAddresses(t *testing.T) {
	s := newNestedTestStack(t)
	if routesTo127(s) {
		t.Fatal("127.0.0.1 is routable before the loopback device is up")
	}
	setLoopbackFlags(t, s, linux.IFF_UP, linux.IFF_UP)
	if flags := loopbackFlags(s); flags&(linux.IFF_UP|linux.IFF_RUNNING|linux.IFF_LOOPBACK) != linux.IFF_UP|linux.IFF_RUNNING|linux.IFF_LOOPBACK {
		t.Fatalf("loopback flags after bring-up = %#x", flags)
	}
	if got, want := loopbackAddrs(s), []string{"127.0.0.1/8", "::1/128"}; !slices.Equal(got, want) {
		t.Fatalf("loopback addresses after bring-up = %q, want %q", got, want)
	}
	if !routesTo127(s) {
		t.Fatal("127.0.0.1 is not routable after bring-up")
	}
	// Bringing an up device up again changes nothing.
	setLoopbackFlags(t, s, linux.IFF_UP, linux.IFF_UP)
	if got, want := loopbackAddrs(s), []string{"127.0.0.1/8", "::1/128"}; !slices.Equal(got, want) {
		t.Fatalf("loopback addresses after repeated bring-up = %q, want %q", got, want)
	}
}

func TestNestedLoopbackBubblewrapSequence(t *testing.T) {
	s := newNestedTestStack(t)
	// Bubblewrap adds 127.0.0.1/8 with NLM_F_EXCL while the device is down,
	// then brings it up. The kernel then adds only ::1.
	if err := s.AddInterfaceAddr(linux.LOOPBACK_IFINDEX, inet.InterfaceAddr{Family: linux.AF_INET, PrefixLen: 8, Addr: []byte{127, 0, 0, 1}}); err != nil {
		t.Fatalf("adding 127.0.0.1/8 to the down loopback device: %v", err)
	}
	setLoopbackFlags(t, s, linux.IFF_UP, linux.IFF_UP)
	if got, want := loopbackAddrs(s), []string{"127.0.0.1/8", "::1/128"}; !slices.Equal(got, want) {
		t.Fatalf("loopback addresses = %q, want %q", got, want)
	}
}

func TestNestedLoopbackZeroChangeReplacesFlags(t *testing.T) {
	s := newNestedTestStack(t)
	setLoopbackFlags(t, s, linux.IFF_UP, 0)
	if flags := loopbackFlags(s); flags&linux.IFF_UP == 0 {
		t.Fatalf("loopback flags = %#x, want up", flags)
	}
}

func TestNestedLoopbackDownRemovesIPv6Addresses(t *testing.T) {
	s := newNestedTestStack(t)
	setLoopbackFlags(t, s, linux.IFF_UP, linux.IFF_UP)
	setLoopbackFlags(t, s, 0, linux.IFF_UP)
	if flags := loopbackFlags(s); flags&(linux.IFF_UP|linux.IFF_RUNNING) != 0 {
		t.Fatalf("loopback flags after take-down = %#x, want down", flags)
	}
	if got, want := loopbackAddrs(s), []string{"127.0.0.1/8"}; !slices.Equal(got, want) {
		t.Fatalf("loopback addresses after take-down = %q, want %q", got, want)
	}
	if routesTo127(s) {
		t.Fatal("127.0.0.1 is routable while the loopback device is down")
	}
	setLoopbackFlags(t, s, linux.IFF_UP, linux.IFF_UP)
	if got, want := loopbackAddrs(s), []string{"127.0.0.1/8", "::1/128"}; !slices.Equal(got, want) {
		t.Fatalf("loopback addresses after second bring-up = %q, want %q", got, want)
	}
}
