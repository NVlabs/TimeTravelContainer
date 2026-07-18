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
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/socket/netfilter"
	sentrynetstack "gvisor.dev/gvisor/pkg/sentry/socket/netstack"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/link/loopback"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const (
	timeTravelNICID   = tcpip.NICID(2)
	timeTravelNICName = "ttos0"
	timeTravelMTU     = uint32(65535)
)

var (
	timeTravelGuestAddress   = tcpip.AddrFrom4([4]byte{10, 0, 2, 15})
	timeTravelGatewayAddress = tcpip.AddrFrom4([4]byte{10, 0, 2, 2})
)

// TimeTravelNetworkConfig configures TTOS's rootless, in-process virtual NIC.
// NAT controls whether the branch-local proxy may create outbound host sockets.
type TimeTravelNetworkConfig struct {
	NAT bool
}

// timeTravelLinkEndpoint is one end of TTOS's in-process virtual wire. Its
// peer and dispatcher are runtime resources: a live-fork child reconstructs a
// fresh pair in configureTimeTravelNetwork instead of serializing the proxy
// stack reachable through the peer dispatcher.
//
// +stateify savable
type timeTravelLinkEndpoint struct {
	peer       *timeTravelLinkEndpoint `state:"nosave"`
	mu         sync.RWMutex            `state:"nosave"`
	dispatcher stack.NetworkDispatcher `state:"nosave"`
	linkAddr   tcpip.LinkAddress
	mtu        uint32
}

func newTimeTravelLink(mtu uint32) (*timeTravelLinkEndpoint, *timeTravelLinkEndpoint) {
	guest := &timeTravelLinkEndpoint{mtu: mtu}
	gateway := &timeTravelLinkEndpoint{mtu: mtu}
	guest.peer = gateway
	gateway.peer = guest
	return guest, gateway
}

func (e *timeTravelLinkEndpoint) deliverPackets(pkts stack.PacketBufferList) {
	peer := e.peer
	if peer == nil {
		return
	}
	peer.mu.RLock()
	dispatcher := peer.dispatcher
	peer.mu.RUnlock()
	if dispatcher == nil {
		return
	}
	for _, pkt := range pkts.AsSlice() {
		clone := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: pkt.ToBuffer()})
		dispatcher.DeliverNetworkPacket(pkt.NetworkProtocolNumber, clone)
		clone.DecRef()
	}
}

func (e *timeTravelLinkEndpoint) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
	n := pkts.Len()
	e.deliverPackets(pkts)
	return n, nil
}

func (e *timeTravelLinkEndpoint) Attach(dispatcher stack.NetworkDispatcher) {
	e.mu.Lock()
	e.dispatcher = dispatcher
	e.mu.Unlock()
}

func (e *timeTravelLinkEndpoint) IsAttached() bool {
	e.mu.RLock()
	attached := e.dispatcher != nil
	e.mu.RUnlock()
	return attached
}

func (*timeTravelLinkEndpoint) Wait() {}

func (e *timeTravelLinkEndpoint) MTU() uint32 {
	e.mu.RLock()
	mtu := e.mtu
	e.mu.RUnlock()
	return mtu
}

func (e *timeTravelLinkEndpoint) SetMTU(mtu uint32) {
	e.mu.Lock()
	e.mtu = mtu
	e.mu.Unlock()
}

func (*timeTravelLinkEndpoint) Capabilities() stack.LinkEndpointCapabilities { return 0 }

func (*timeTravelLinkEndpoint) MaxHeaderLength() uint16 { return 0 }

func (e *timeTravelLinkEndpoint) LinkAddress() tcpip.LinkAddress {
	e.mu.RLock()
	addr := e.linkAddr
	e.mu.RUnlock()
	return addr
}

func (e *timeTravelLinkEndpoint) SetLinkAddress(addr tcpip.LinkAddress) {
	e.mu.Lock()
	e.linkAddr = addr
	e.mu.Unlock()
}

func (*timeTravelLinkEndpoint) ARPHardwareType() header.ARPHardwareType {
	return header.ARPHardwareNone
}

func (*timeTravelLinkEndpoint) AddHeader(*stack.PacketBuffer) {}

func (*timeTravelLinkEndpoint) ParseHeader(*stack.PacketBuffer) bool { return true }

func (*timeTravelLinkEndpoint) Close() {}

func (*timeTravelLinkEndpoint) SetOnCloseAction(func()) {}

// proxyRealm owns one branch's gateway stack and all host connections created
// by its user-mode NAT. It is intentionally unreachable from serialized guest
// state except through runtime-only live-fork hooks.
type proxyRealm struct {
	nat bool

	ctx    context.Context
	cancel context.CancelFunc

	mu    sync.Mutex
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup
	once  sync.Once

	gateway *stack.Stack
}

func newProxyRealm(nat bool) *proxyRealm {
	ctx, cancel := context.WithCancel(context.Background())
	return &proxyRealm{
		nat:    nat,
		ctx:    ctx,
		cancel: cancel,
		conns:  make(map[net.Conn]struct{}),
	}
}

func (r *proxyRealm) track(conns ...net.Conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx.Err() != nil {
		return false
	}
	for _, conn := range conns {
		r.conns[conn] = struct{}{}
	}
	return true
}

func (r *proxyRealm) untrack(conns ...net.Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, conn := range conns {
		delete(r.conns, conn)
	}
}

func (r *proxyRealm) relay(a, b net.Conn, lifetime time.Duration) {
	if !r.track(a, b) {
		a.Close()
		b.Close()
		return
	}
	var closeOnce sync.Once
	var timer *time.Timer
	closePair := func() {
		closeOnce.Do(func() {
			if timer != nil {
				timer.Stop()
			}
			a.Close()
			b.Close()
			r.untrack(a, b)
		})
	}
	if lifetime != 0 {
		timer = time.AfterFunc(lifetime, closePair)
	}
	r.wg.Add(2)
	copyOneWay := func(dst, src net.Conn) {
		defer r.wg.Done()
		_, _ = io.Copy(dst, src)
		closePair()
	}
	go copyOneWay(a, b)
	go copyOneWay(b, a)
}

func (r *proxyRealm) close() {
	r.once.Do(func() {
		r.cancel()
		r.mu.Lock()
		for conn := range r.conns {
			conn.Close()
		}
		r.mu.Unlock()
		if r.gateway != nil {
			stats := r.gateway.Stats()
			log.Debugf("Closing TTOS proxy realm (IPv4 received=%d valid=%d delivered=%d disabled=%d invalid-src=%d invalid-dst=%d malformed=%d prerouting-drop=%d input-drop=%d; TCP valid=%d invalid=%d checksum=%d; sent=%d)",
				stats.IP.PacketsReceived.Value(), stats.IP.ValidPacketsReceived.Value(), stats.IP.PacketsDelivered.Value(),
				stats.IP.DisabledPacketsReceived.Value(), stats.IP.InvalidSourceAddressesReceived.Value(), stats.IP.InvalidDestinationAddressesReceived.Value(), stats.IP.MalformedPacketsReceived.Value(),
				stats.IP.IPTablesPreroutingDropped.Value(), stats.IP.IPTablesInputDropped.Value(),
				stats.TCP.ValidSegmentsReceived.Value(), stats.TCP.InvalidSegmentsReceived.Value(), stats.TCP.ChecksumErrors.Value(), stats.IP.PacketsSent.Value())
			r.gateway.Close()
			r.gateway.Wait()
		}
		r.wg.Wait()
	})
}

func hostAddress(addr tcpip.Address, port uint16) string {
	ip := net.IP(addr.AsSlice())
	// 10.0.2.x aliases host 127.0.0.x. The gateway address retains the common
	// 127.0.0.1 mapping; other aliases make host stub resolvers such as
	// 127.0.0.53 reachable without exposing guest loopback traffic.
	if bytes := addr.AsSlice(); len(bytes) == 4 && bytes[0] == 10 && bytes[1] == 0 && bytes[2] == 2 && addr != timeTravelGuestAddress {
		last := bytes[3]
		if addr == timeTravelGatewayAddress {
			last = 1
		}
		ip = net.IPv4(127, 0, 0, last)
	}
	return net.JoinHostPort(ip.String(), fmt.Sprint(port))
}

func (r *proxyRealm) installTCPForwarder(gateway *stack.Stack) {
	forwarder := tcp.NewForwarder(gateway, 0, 1024, func(req *tcp.ForwarderRequest) {
		id := req.ID()
		target := hostAddress(id.LocalAddress, id.LocalPort)
		log.Debugf("TTOS NAT TCP flow %s:%d -> %s", id.RemoteAddress, id.RemotePort, target)
		hostConn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(r.ctx, "tcp", target)
		if err != nil {
			log.Debugf("TTOS NAT TCP dial %s failed: %v", target, err)
			req.Complete(true)
			return
		}
		var wq waiter.Queue
		ep, tcpErr := req.CreateEndpoint(&wq)
		if tcpErr != nil {
			hostConn.Close()
			req.Complete(true)
			return
		}
		req.Complete(false)
		r.relay(gonet.NewTCPConn(&wq, ep), hostConn, 0)
	})
	gateway.SetTransportProtocolHandler(tcp.ProtocolNumber, forwarder.HandlePacket)
}

func (r *proxyRealm) installUDPForwarder(gateway *stack.Stack) {
	forwarder := udp.NewForwarder(gateway, func(req *udp.ForwarderRequest) bool {
		id := req.ID()
		remote, err := net.ResolveUDPAddr("udp", hostAddress(id.LocalAddress, id.LocalPort))
		if err != nil {
			return false
		}
		hostConn, err := net.DialUDP("udp", nil, remote)
		if err != nil {
			return false
		}
		var wq waiter.Queue
		ep, udpErr := req.CreateEndpoint(&wq)
		if udpErr != nil {
			hostConn.Close()
			return false
		}
		// Bound UDP mappings are deliberately short-lived. A later datagram
		// creates a fresh mapping, just as an expired NAT entry would.
		r.relay(gonet.NewUDPConn(&wq, ep), hostConn, 2*time.Minute)
		return true
	})
	gateway.SetTransportProtocolHandler(udp.ProtocolNumber, forwarder.HandlePacket)
}

func configureTimeTravelNetwork(dst *sentrynetstack.Stack, cfg TimeTravelNetworkConfig, reset bool) (retErr error) {
	ensureTimeTravelIPTables(dst)
	if reset {
		dst.Stack.ResetConfig()
		if err := dst.Stack.CreateNICWithOptions(1, ethernet.New(loopback.New()), stack.NICOptions{
			Name:               "lo",
			DeliverLinkPackets: true,
		}); err != nil {
			return fmt.Errorf("creating TTOS loopback NIC: %v", err)
		}
		for _, addr := range []tcpip.ProtocolAddress{
			{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: header.IPv4Loopback.WithPrefix()},
			{Protocol: ipv6.ProtocolNumber, AddressWithPrefix: header.IPv6Loopback.WithPrefix()},
		} {
			if err := dst.Stack.AddProtocolAddress(1, addr, stack.AddressProperties{}); err != nil {
				return fmt.Errorf("adding TTOS loopback address: %v", err)
			}
		}
	}

	realm := newProxyRealm(cfg.NAT)
	defer func() {
		if retErr != nil {
			realm.close()
		}
	}()
	guestEP, gatewayEP := newTimeTravelLink(timeTravelMTU)
	if err := dst.Stack.CreateNICWithOptions(timeTravelNICID, guestEP, stack.NICOptions{
		Name:               timeTravelNICName,
		DeliverLinkPackets: true,
	}); err != nil {
		return fmt.Errorf("creating TTOS guest NIC: %v", err)
	}
	if err := dst.Stack.AddProtocolAddress(timeTravelNICID, tcpip.ProtocolAddress{
		Protocol: ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{
			Address:   timeTravelGuestAddress,
			PrefixLen: 24,
		},
	}, stack.AddressProperties{}); err != nil {
		return fmt.Errorf("adding TTOS guest address: %v", err)
	}
	dst.Stack.SetRouteTable([]tcpip.Route{
		{Destination: header.IPv4LoopbackSubnet, NIC: 1},
		{Destination: header.IPv6Loopback.WithPrefix().Subnet(), NIC: 1},
		// The in-memory point-to-point link has no L2 neighbor resolution; every
		// non-loopback address is delivered directly to the proxy NIC.
		{Destination: header.IPv4EmptySubnet, NIC: timeTravelNICID},
	})

	gateway := stack.New(stack.Options{
		NetworkProtocols: []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{
			tcp.NewProtocol,
			udp.NewProtocol,
			icmp.NewProtocol4,
			icmp.NewProtocol6,
		},
		Clock: dst.Stack.Clock(),
	})
	realm.gateway = gateway
	const gatewayNICID = tcpip.NICID(1)
	if err := gateway.CreateNIC(gatewayNICID, gatewayEP); err != nil {
		return fmt.Errorf("creating TTOS proxy NIC: %v", err)
	}
	if err := gateway.AddProtocolAddress(gatewayNICID, tcpip.ProtocolAddress{
		Protocol: ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{
			Address:   timeTravelGatewayAddress,
			PrefixLen: 24,
		},
	}, stack.AddressProperties{}); err != nil {
		return fmt.Errorf("adding TTOS proxy address: %v", err)
	}
	if err := gateway.SetPromiscuousMode(gatewayNICID, true); err != nil {
		return fmt.Errorf("enabling TTOS proxy promiscuous mode: %v", err)
	}
	if err := gateway.SetSpoofing(gatewayNICID, true); err != nil {
		return fmt.Errorf("enabling TTOS proxy spoofing: %v", err)
	}
	gateway.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: gatewayNICID}})
	if cfg.NAT {
		realm.installTCPForwarder(gateway)
		realm.installUDPForwarder(gateway)
	}

	hooks := &sentrynetstack.LiveForkNetworkHooks{
		Validate: validateTimeTravelNetwork,
		Clone: func(child *sentrynetstack.Stack) error {
			tables, ok := dst.Stack.IPTables().CloneUnmodifiedForLiveFork(child.Stack.Clock())
			if !ok {
				return fmt.Errorf("modified iptables/conntrack state is not yet supported")
			}
			child.Stack.SetIPTables(tables)
			return configureTimeTravelNetwork(child, cfg, true)
		},
		ResetChild: resetTimeTravelChildConnections,
		Close:      realm.close,
	}
	dst.SetLiveForkNetworkHooks(hooks)
	log.Infof("Configured TTOS virtual NIC %s at %s/24 (NAT=%t)", timeTravelNICName, timeTravelGuestAddress, cfg.NAT)
	return nil
}

func validateTimeTravelNetwork(s *sentrynetstack.Stack) error {
	if tables := s.Stack.IPTables(); tables == nil {
		return fmt.Errorf("network stack has no iptables configuration")
	} else if tables.Modified() {
		return fmt.Errorf("modified iptables/conntrack state is not yet supported")
	}
	if s.Stack.NFTables() != nil {
		return fmt.Errorf("nftables configuration is not yet supported")
	}
	interfaces := s.Interfaces()
	if len(interfaces) != 2 {
		return fmt.Errorf("TTOS network has %d interfaces, want loopback and %s", len(interfaces), timeTravelNICName)
	}
	if lo, ok := interfaces[1]; !ok || lo.Name != "lo" {
		return fmt.Errorf("TTOS network canonical loopback is missing")
	}
	if nic, ok := interfaces[int32(timeTravelNICID)]; !ok || nic.Name != timeTravelNICName {
		return fmt.Errorf("TTOS virtual NIC %s is missing", timeTravelNICName)
	}
	for _, route := range s.RouteTable() {
		if route.OutputInterface != 0 {
			if _, ok := interfaces[route.OutputInterface]; !ok {
				return fmt.Errorf("route uses unknown interface %d", route.OutputInterface)
			}
		}
	}
	return nil
}

func containedTimeTravelAddress(addr tcpip.Address) bool {
	return header.IPv4LoopbackSubnet.Contains(addr) || addr == header.IPv6Loopback || addr == timeTravelGuestAddress
}

func reverseEndpointID(id stack.TransportEndpointID) stack.TransportEndpointID {
	return stack.TransportEndpointID{
		LocalPort:     id.RemotePort,
		LocalAddress:  id.RemoteAddress,
		RemotePort:    id.LocalPort,
		RemoteAddress: id.LocalAddress,
	}
}

func resetTimeTravelChildConnections(s *sentrynetstack.Stack) error {
	var externalProxyPeers = make(map[stack.TransportEndpointID]struct{})
	for _, transportEP := range s.Stack.RegisteredEndpoints() {
		ep, ok := transportEP.(*tcp.Endpoint)
		if !ok || !ep.LiveForkExternal() {
			continue
		}
		info, ok := ep.Info().(*stack.TransportEndpointInfo)
		if ok {
			externalProxyPeers[reverseEndpointID(info.ID)] = struct{}{}
		}
	}
	for _, transportEP := range s.Stack.RegisteredEndpoints() {
		ep, ok := transportEP.(*tcp.Endpoint)
		if !ok || ep.EndpointState() == tcp.StateListen {
			continue
		}
		info, ok := ep.Info().(*stack.TransportEndpointInfo)
		if !ok {
			continue
		}
		_, proxyPeer := externalProxyPeers[info.ID]
		if ep.LiveForkExternal() || proxyPeer || (!info.ID.RemoteAddress.Unspecified() && !containedTimeTravelAddress(info.ID.RemoteAddress)) {
			ep.ResetForLiveForkChild()
		}
	}
	return nil
}

func ensureTimeTravelIPTables(s *sentrynetstack.Stack) {
	if s.Stack.IPTables() == nil {
		s.Stack.SetIPTables(netfilter.DefaultLinuxTables(s.Stack.Clock(), s.Stack.InsecureRNG()))
	}
}
