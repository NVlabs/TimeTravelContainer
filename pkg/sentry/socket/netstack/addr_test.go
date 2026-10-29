// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package netstack

import (
	"testing"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/sentry/inet"
	"gvisor.dev/gvisor/pkg/tcpip/link/loopback"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

func TestIPv4ClassPrefixLen(t *testing.T) {
	for _, tc := range []struct {
		addr   [4]byte
		prefix uint8
		ok     bool
	}{
		{[4]byte{0, 0, 0, 0}, 0, true},
		{[4]byte{255, 255, 255, 255}, 0, true},
		{[4]byte{10, 0, 2, 15}, 8, true},
		{[4]byte{172, 16, 0, 1}, 16, true},
		{[4]byte{192, 168, 1, 1}, 24, true},
		{[4]byte{224, 0, 0, 1}, 0, false},
		{[4]byte{240, 0, 0, 1}, 32, true},
	} {
		prefix, ok := ipv4ClassPrefixLen(tc.addr[:])
		if prefix != tc.prefix || ok != tc.ok {
			t.Errorf("ipv4ClassPrefixLen(%v) = %d, %t; want %d, %t", tc.addr, prefix, ok, tc.prefix, tc.ok)
		}
	}
}

func TestIPv4MaskPrefixLen(t *testing.T) {
	for _, tc := range []struct {
		mask   [4]byte
		prefix uint8
		ok     bool
	}{
		{[4]byte{0, 0, 0, 0}, 0, true},
		{[4]byte{255, 0, 0, 0}, 8, true},
		{[4]byte{255, 255, 255, 0}, 24, true},
		{[4]byte{255, 255, 255, 255}, 32, true},
		{[4]byte{255, 0, 255, 0}, 0, false},
		{[4]byte{0, 255, 255, 255}, 0, false},
	} {
		prefix, ok := ipv4MaskPrefixLen(tc.mask[:])
		if prefix != tc.prefix || ok != tc.ok {
			t.Errorf("ipv4MaskPrefixLen(%v) = %d, %t; want %d, %t", tc.mask, prefix, ok, tc.prefix, tc.ok)
		}
	}
}

type addrEvent struct {
	idx   int32
	addr  string
	added bool
}

type addrSubscriber struct {
	events []addrEvent
}

func (*addrSubscriber) OnInterfaceChangeEvent(context.Context, int32, inet.Interface) {}
func (*addrSubscriber) OnInterfaceDeleteEvent(context.Context, int32, inet.Interface) {}
func (s *addrSubscriber) OnAddressEvent(_ context.Context, idx int32, _ inet.Interface, addr inet.InterfaceAddr, added bool) {
	s.events = append(s.events, addrEvent{idx, string(addr.Addr), added})
}
func (*addrSubscriber) OnRouteEvent(context.Context, inet.Route, bool, uint16) {}

// TestInterfaceAddrEvents checks that the stack tells subscribers of the
// addresses that interfaces gain and lose, as netlink listeners learn them.
func TestInterfaceAddrEvents(t *testing.T) {
	ts := stack.New(stack.Options{NetworkProtocols: []stack.NetworkProtocolFactory{ipv4.NewProtocol}})
	defer ts.Close()
	if err := ts.CreateNIC(1, loopback.New()); err != nil {
		t.Fatalf("CreateNIC: %v", err)
	}
	s := NewStack(ts, 0)
	sub := &addrSubscriber{}
	s.AddInterfaceEventSubscriber(sub)

	addr := inet.InterfaceAddr{Family: linux.AF_INET, PrefixLen: 8, Addr: []byte{10, 0, 2, 15}}
	if err := s.AddInterfaceAddr(context.Background(), 1, addr); err != nil {
		t.Fatalf("AddInterfaceAddr: %v", err)
	}
	if err := s.RemoveInterfaceAddr(context.Background(), 1, addr); err != nil {
		t.Fatalf("RemoveInterfaceAddr: %v", err)
	}
	want := []addrEvent{
		{1, string(addr.Addr), true},
		{1, string(addr.Addr), false},
	}
	if len(sub.events) != len(want) {
		t.Fatalf("events = %v, want %v", sub.events, want)
	}
	for i := range want {
		if sub.events[i] != want[i] {
			t.Errorf("event %d = %v, want %v", i, sub.events[i], want[i])
		}
	}
}
