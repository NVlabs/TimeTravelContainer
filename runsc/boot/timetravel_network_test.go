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
	"context"
	"io"
	"net"
	"testing"
	"time"

	gcontext "gvisor.dev/gvisor/pkg/context"
	sentrynetstack "gvisor.dev/gvisor/pkg/sentry/socket/netstack"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/link/veth"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	pf "gvisor.dev/gvisor/runsc/boot/portforward"
)

func newTimeTravelNetworkTestStack(t *testing.T, nat bool) *sentrynetstack.Stack {
	return newTimeTravelNetworkTestStackWithConfig(t, TimeTravelNetworkConfig{NAT: nat})
}

func newTimeTravelNetworkTestStackWithConfig(t *testing.T, cfg TimeTravelNetworkConfig) *sentrynetstack.Stack {
	t.Helper()
	s := sentrynetstack.NewStack(stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
		HandleLocal:        true,
	}), 1)
	if err := configureTimeTravelNetwork(s, cfg, true); err != nil {
		t.Fatalf("configureTimeTravelNetwork(%+v): %v", cfg, err)
	}
	t.Cleanup(s.Destroy)
	return s
}

func TestTimeTravelNetworkCustomAddressAndMAC(t *testing.T) {
	guestIP := net.IPv4(192, 0, 2, 44)
	guestMAC := net.HardwareAddr{0x02, 0xaa, 0xbb, 0xcc, 0xdd, 0xee}
	s := newTimeTravelNetworkTestStackWithConfig(t, TimeTravelNetworkConfig{GuestIP: guestIP, GuestMAC: guestMAC})
	info := s.Stack.NICInfo()[timeTravelNICID]
	if got := net.HardwareAddr(info.LinkAddress).String(); got != guestMAC.String() {
		t.Errorf("link address = %s, want %s", got, guestMAC)
	}
	wantAddress := tcpip.AddrFromSlice(guestIP.To4())
	found := false
	for _, address := range info.ProtocolAddresses {
		if address.Protocol == ipv4.ProtocolNumber && address.AddressWithPrefix.Address == wantAddress && address.AddressWithPrefix.PrefixLen == 24 {
			found = true
		}
	}
	if !found {
		t.Errorf("protocol addresses = %+v, want %s/24", info.ProtocolAddresses, wantAddress)
	}
}

func dialTimeTravelGateway(t *testing.T, s *sentrynetstack.Stack, port uint16, timeout time.Duration) (*gonet.TCPConn, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return gonet.DialContextTCP(ctx, s.Stack, tcpip.FullAddress{
		Addr: timeTravelGatewayAddress,
		Port: port,
	}, ipv4.ProtocolNumber)
}

func TestTimeTravelNetworkWithoutNAT(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer listener.Close()
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	s := newTimeTravelNetworkTestStack(t, false)
	if conn, err := dialTimeTravelGateway(t, s, port, 250*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("connection through NAT-disabled TTC network unexpectedly succeeded")
	}
}

func TestTimeTravelNetworkTCPNAT(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer listener.Close()
	serverErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		_, err = io.Copy(conn, conn)
		serverErr <- err
	}()

	s := newTimeTravelNetworkTestStack(t, true)
	conn, err := dialTimeTravelGateway(t, s, uint16(listener.Addr().(*net.TCPAddr).Port), 2*time.Second)
	if err != nil {
		t.Logf("guest IPv4 sent=%d received=%d", s.Stack.Stats().IP.PacketsSent.Value(), s.Stack.Stats().IP.PacketsReceived.Value())
		t.Fatalf("dial through TTC NAT: %v", err)
	}
	payload := []byte("ttc-nat-echo")
	if _, err := conn.Write(payload); err != nil {
		conn.Close()
		t.Fatalf("write through TTC NAT: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		conn.Close()
		t.Fatalf("read through TTC NAT: %v", err)
	}
	conn.Close()
	if string(got) != string(payload) {
		t.Fatalf("NAT echo = %q, want %q", got, payload)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("host echo server: %v", err)
	}
}

func TestTimeTravelNetworkUDPNATAndHostAlias(t *testing.T) {
	hostAddress := net.IPv4(127, 0, 0, 53)
	hostConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: hostAddress})
	if err != nil {
		t.Fatalf("net.ListenUDP: %v", err)
	}
	defer hostConn.Close()
	serverErr := make(chan error, 1)
	go func() {
		buffer := make([]byte, 64)
		n, peer, err := hostConn.ReadFromUDP(buffer)
		if err == nil {
			_, err = hostConn.WriteToUDP(buffer[:n], peer)
		}
		serverErr <- err
	}()

	s := newTimeTravelNetworkTestStack(t, true)
	alias := tcpip.AddrFrom4([4]byte{10, 0, 2, 53})
	conn, err := gonet.DialUDP(s.Stack, nil, &tcpip.FullAddress{Addr: alias, Port: uint16(hostConn.LocalAddr().(*net.UDPAddr).Port)}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatalf("gonet.DialUDP: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	payload := []byte("ttc-udp-echo")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("UDP write through TTC NAT: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("UDP read through TTC NAT: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("UDP NAT echo = %q, want %q", got, payload)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("host UDP echo server: %v", err)
	}
}

func TestTimeTravelNetworkLiveForkHooks(t *testing.T) {
	source := newTimeTravelNetworkTestStack(t, true)
	if err := source.ValidateLiveFork(); err != nil {
		t.Fatalf("source ValidateLiveFork: %v", err)
	}
	child := sentrynetstack.NewStack(stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
		HandleLocal:        true,
	}), 2)
	t.Cleanup(child.Destroy)
	// Model guest-owned configuration already decoded by state.Load. Runtime
	// link reconstruction must not flatten or discard it.
	guestVeth, _ := veth.NewPair(1500, veth.DefaultBacklogSize)
	const guestNIC = tcpip.NICID(7)
	if err := child.Stack.CreateNICWithOptions(guestNIC, ethernet.New(guestVeth), stack.NICOptions{Name: "guest-veth"}); err != nil {
		t.Fatalf("creating decoded guest veth: %v", err)
	}
	if err := source.CloneLiveForkConfig(child); err != nil {
		t.Fatalf("CloneLiveForkConfig: %v", err)
	}
	if err := child.ValidateLiveFork(); err != nil {
		t.Fatalf("child ValidateLiveFork: %v", err)
	}
	if err := child.ResetLiveForkChild(); err != nil {
		t.Fatalf("child ResetLiveForkChild: %v", err)
	}
	if source.Stack == child.Stack {
		t.Fatal("live-fork child shares the source netstack")
	}
	if nic, ok := child.Interfaces()[int32(guestNIC)]; !ok || nic.Name != "guest-veth" {
		t.Fatalf("live-fork runtime link reconstruction discarded guest NIC: %+v", child.Interfaces())
	}
}

func TestTimeTravelNetworkResetsPublishedTCPPair(t *testing.T) {
	s := newTimeTravelNetworkTestStack(t, false)
	listener, err := gonet.ListenTCP(s.Stack, tcpip.FullAddress{Addr: header.IPv4Loopback, Port: 0}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatalf("gonet.ListenTCP: %v", err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- conn
	}()
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	proxyConn, err := pf.NewNetstackConn(s.Stack, port)
	if err != nil {
		t.Fatalf("NewNetstackConn: %v", err)
	}
	ctx := gcontext.Background()
	defer proxyConn.Close(ctx)
	var appConn net.Conn
	select {
	case appConn = <-accepted:
	case err := <-acceptErr:
		t.Fatalf("accept: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out accepting published connection")
	}
	defer appConn.Close()
	if err := resetTimeTravelChildConnections(s, timeTravelGuestAddress); err != nil {
		t.Fatalf("resetTimeTravelChildConnections: %v", err)
	}
	if _, err := proxyConn.Write(ctx, []byte("must-reset"), make(chan struct{})); err == nil {
		t.Fatal("marked published proxy endpoint remained writable after child reset")
	}
	if err := appConn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	if _, err := appConn.Read(make([]byte, 1)); err == nil {
		t.Fatal("container peer of published endpoint remained connected after child reset")
	}
}
