// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package boot

import (
	"context"
	"fmt"
	"gvisor.dev/gvisor/runsc/boot/sentryapi"
	"io"
	"net"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/hostthread"
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
	timeTravelNICID = tcpip.NICID(2)
	// defaultTimeTravelNICName names the guest's NIC when its configuration
	// does not, as Docker names a container's interface.
	defaultTimeTravelNICName = "eth0"
	timeTravelMTU            = uint32(65535)
)

var (
	timeTravelGuestAddress   = tcpip.AddrFrom4([4]byte{10, 0, 2, 15})
	timeTravelGatewayAddress = tcpip.AddrFrom4([4]byte{10, 0, 2, 2})
	timeTravelGuestMAC       = net.HardwareAddr{0x02, 0x42, 0x0a, 0x00, 0x02, 0x0f}
)

// TimeTravelNetworkConfig configures TTC's rootless, in-process virtual NIC.
type TimeTravelNetworkConfig = sentryapi.TimeTravelNetworkConfig

// timeTravelInterfaceName returns the guest's name for c's NIC.
func timeTravelInterfaceName(c TimeTravelNetworkConfig) string {
	if c.InterfaceName != "" {
		return c.InterfaceName
	}
	return defaultTimeTravelNICName
}

func timeTravelAddresses(c TimeTravelNetworkConfig) (tcpip.Address, tcpip.Address, tcpip.LinkAddress, tcpip.LinkAddress) {
	guest := timeTravelGuestAddress
	if ip := c.GuestIP.To4(); ip != nil {
		guest = tcpip.AddrFromSlice(ip)
	}
	bytes := guest.AsSlice()
	gateway := tcpip.AddrFrom4([4]byte{bytes[0], bytes[1], bytes[2], 2})
	mac := c.GuestMAC
	if len(mac) != 6 {
		mac = timeTravelGuestMAC
	}
	gatewayMAC := append(net.HardwareAddr(nil), mac...)
	gatewayMAC[len(gatewayMAC)-1] ^= 1
	return guest, gateway, tcpip.LinkAddress(string(mac)), tcpip.LinkAddress(string(gatewayMAC))
}

// timeTravelLinkEndpoint is one end of TTC's in-process virtual wire. Its
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
	nat            bool
	guestAddress   tcpip.Address
	gatewayAddress tcpip.Address

	ctx    context.Context
	cancel context.CancelFunc

	mu    sync.Mutex
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup
	once  sync.Once

	gateway *stack.Stack
}

func newProxyRealm(nat bool, guestAddress, gatewayAddress tcpip.Address) *proxyRealm {
	ctx, cancel := context.WithCancel(context.Background())
	return &proxyRealm{
		nat:            nat,
		ctx:            ctx,
		cancel:         cancel,
		conns:          make(map[net.Conn]struct{}),
		guestAddress:   guestAddress,
		gatewayAddress: gatewayAddress,
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
		// Its host system calls are on the host connection, which Go's network
		// poller keeps non-blocking.
		hostthread.SyscallsEnd()
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
			log.Debugf("Closing TTC proxy realm (IPv4 received=%d valid=%d delivered=%d disabled=%d invalid-src=%d invalid-dst=%d malformed=%d prerouting-drop=%d input-drop=%d; TCP valid=%d invalid=%d checksum=%d; sent=%d)",
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

func (r *proxyRealm) hostAddress(addr tcpip.Address, port uint16) string {
	ip := net.IP(addr.AsSlice())
	// The guest /24 aliases host 127.0.0.x. The gateway address retains the common
	// 127.0.0.1 mapping; other aliases make host stub resolvers such as
	// 127.0.0.53 reachable without exposing guest loopback traffic.
	guest := r.guestAddress.AsSlice()
	if bytes := addr.AsSlice(); len(bytes) == 4 && len(guest) == 4 && bytes[0] == guest[0] && bytes[1] == guest[1] && bytes[2] == guest[2] && addr != r.guestAddress {
		last := bytes[3]
		if addr == r.gatewayAddress {
			last = 1
		}
		ip = timeTravelHostLoopbackAddress(last)
	}
	return net.JoinHostPort(ip.String(), fmt.Sprint(port))
}

func (r *proxyRealm) installTCPForwarder(gateway *stack.Stack) {
	forwarder := tcp.NewForwarder(gateway, 0, 1024, func(req *tcp.ForwarderRequest) {
		id := req.ID()
		target := r.hostAddress(id.LocalAddress, id.LocalPort)
		log.Debugf("TTC NAT TCP flow %s:%d -> %s", id.RemoteAddress, id.RemotePort, target)
		hostConn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(r.ctx, "tcp", target)
		if err != nil {
			log.Debugf("TTC NAT TCP dial %s failed: %v", target, err)
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
		remote, err := net.ResolveUDPAddr("udp", r.hostAddress(id.LocalAddress, id.LocalPort))
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

// configureTimeTravelNetwork gives a stack the TTC link.
//
// quarantine builds the same link with nothing on the other end of it: the
// guest keeps its interface, its address and its routes, and the cable runs
// into the dark. What the guest writes is accepted and dropped, what it waits
// to read never comes, and a new connection gets as far as a SYN and no
// further. That is what a child forked to be asked one question and then
// destroyed should look like — from inside, a machine whose network has gone
// quiet; from outside, a machine that was never there.
func configureTimeTravelNetwork(dst *sentrynetstack.Stack, cfg TimeTravelNetworkConfig, reset, quarantine bool) (retErr error) {
	guestAddress, gatewayAddress, guestMAC, gatewayMAC := timeTravelAddresses(cfg)
	ensureTimeTravelIPTables(dst)
	if reset {
		dst.Stack.ResetConfig()
		if err := dst.Stack.CreateNICWithOptions(1, ethernet.New(loopback.New()), stack.NICOptions{
			Name:               "lo",
			DeliverLinkPackets: true,
		}); err != nil {
			return fmt.Errorf("creating TTC loopback NIC: %v", err)
		}
		for _, addr := range []tcpip.ProtocolAddress{
			{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: header.IPv4Loopback.WithPrefix()},
			{Protocol: ipv6.ProtocolNumber, AddressWithPrefix: header.IPv6Loopback.WithPrefix()},
		} {
			if err := dst.Stack.AddProtocolAddress(1, addr, stack.AddressProperties{}); err != nil {
				return fmt.Errorf("adding TTC loopback address: %v", err)
			}
		}
	}

	realm := newProxyRealm(cfg.NAT, guestAddress, gatewayAddress)
	defer func() {
		if retErr != nil {
			realm.close()
		}
	}()
	guestEP, gatewayEP := newTimeTravelLink(timeTravelMTU)
	_ = gatewayEP
	guestEP.SetLinkAddress(guestMAC)
	gatewayEP.SetLinkAddress(gatewayMAC)
	replacedRuntimeLink := false
	if _, ok := dst.Interfaces()[int32(timeTravelNICID)]; ok {
		if err := dst.Stack.ReplaceNICLinkEndpoint(timeTravelNICID, ethernet.New(guestEP)); err != nil {
			return fmt.Errorf("replacing TTC guest link: %v", err)
		}
		replacedRuntimeLink = true
	} else {
		if err := dst.Stack.CreateNICWithOptions(timeTravelNICID, ethernet.New(guestEP), stack.NICOptions{
			Name:               timeTravelInterfaceName(cfg),
			DeliverLinkPackets: true,
		}); err != nil {
			return fmt.Errorf("creating TTC guest NIC: %v", err)
		}
		if err := dst.Stack.AddProtocolAddress(timeTravelNICID, tcpip.ProtocolAddress{
			Protocol: ipv4.ProtocolNumber,
			AddressWithPrefix: tcpip.AddressWithPrefix{
				Address:   guestAddress,
				PrefixLen: 24,
			},
		}, stack.AddressProperties{}); err != nil {
			return fmt.Errorf("adding TTC guest address: %v", err)
		}
	}
	routes := dst.Stack.GetRouteTable()
	if reset {
		routes = []tcpip.Route{
			{Destination: header.IPv4LoopbackSubnet, NIC: 1},
			{Destination: header.IPv6Loopback.WithPrefix().Subnet(), NIC: 1},
		}
	}
	if !replacedRuntimeLink {
		routes = append(routes, tcpip.Route{Destination: header.IPv4EmptySubnet, Gateway: gatewayAddress, NIC: timeTravelNICID})
		dst.Stack.SetRouteTable(routes)
	}

	if quarantine {
		// No proxy at the other end, and nothing to close: the guest half of
		// the cable delivers to a peer with no dispatcher, which drops what it
		// is given and reports it sent. Every existing connection stays exactly
		// as it was — no reset, nothing for a program to notice.
		//
		// The gateway's address is given a fixed answer so that packets for it
		// are built and sent rather than held back. Without it the first thing
		// out is an ARP request nobody answers, and a new connection fails
		// immediately with "no route to host" — which a program can see and
		// report. With it, the SYN goes into the dark and the connection
		// attempt simply waits, which is what being cut off should look like
		// from inside.
		if err := dst.Stack.AddStaticNeighbor(timeTravelNICID, ipv4.ProtocolNumber, gatewayAddress, tcpip.LinkAddress(gatewayMAC)); err != nil {
			return fmt.Errorf("pinning the quarantined gateway address: %v", err)
		}
		realm.close()
	} else {
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
		if err := gateway.CreateNIC(gatewayNICID, ethernet.New(gatewayEP)); err != nil {
			return fmt.Errorf("creating TTC proxy NIC: %v", err)
		}
		if err := gateway.AddProtocolAddress(gatewayNICID, tcpip.ProtocolAddress{
			Protocol: ipv4.ProtocolNumber,
			AddressWithPrefix: tcpip.AddressWithPrefix{
				Address:   gatewayAddress,
				PrefixLen: 24,
			},
		}, stack.AddressProperties{}); err != nil {
			return fmt.Errorf("adding TTC proxy address: %v", err)
		}
		if err := gateway.SetPromiscuousMode(gatewayNICID, true); err != nil {
			return fmt.Errorf("enabling TTC proxy promiscuous mode: %v", err)
		}
		if err := gateway.SetSpoofing(gatewayNICID, true); err != nil {
			return fmt.Errorf("enabling TTC proxy spoofing: %v", err)
		}
		gateway.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: gatewayNICID}})
		if cfg.NAT {
			realm.installTCPForwarder(gateway)
			realm.installUDPForwarder(gateway)
		}
	}

	hooks := &sentrynetstack.LiveForkNetworkHooks{
		Validate: validateTimeTravelNetwork,
		Clone: func(child *sentrynetstack.Stack) error {
			// The child already contains the serialized Sentry-owned network
			// graph. Replace only TTC's runtime link, whose proxy peer is
			// deliberately not serialized. Removing the NIC would also close its
			// protocol addresses and silently unregister listening sockets.
			return configureTimeTravelNetwork(child, cfg, false, child.LiveForkQuarantine())
		},
		ResetChild: func(child *sentrynetstack.Stack) error {
			if child.LiveForkQuarantine() {
				// Deliberately not reset. A reset is how a fork tells the world
				// it happened: the peer sees an RST, the program sees its
				// connection die, and a licensed tool may say so out loud. A
				// quarantined child keeps its connections and finds them silent.
				return nil
			}
			return resetTimeTravelChildConnections(child, guestAddress)
		},
		SetQuarantine: func(current *sentrynetstack.Stack, quarantine bool) error {
			if err := configureTimeTravelNetwork(current, cfg, false, quarantine); err != nil {
				return err
			}
			if !quarantine {
				// Packets emitted while quarantined never reached the real peer.
				// Existing external flows therefore cannot be continued honestly;
				// reset them while leaving internal-only flows untouched.
				return resetTimeTravelChildConnections(current, guestAddress)
			}
			return nil
		},
		Close: realm.close,
	}
	dst.SetLiveForkQuarantine(quarantine)
	dst.SetLiveForkNetworkHooks(hooks)
	log.Infof("Configured TTC virtual NIC %s at %s/24, MAC %s (NAT=%t)", timeTravelInterfaceName(cfg), guestAddress, net.HardwareAddr(guestMAC), cfg.NAT)
	return nil
}

func validateTimeTravelNetwork(s *sentrynetstack.Stack) error {
	// All guest-visible configuration is Sentry-owned and serialized. The
	// runtime hook itself proves that the only external edge, TTC's proxy peer,
	// is omitted and can be recreated branch-locally.
	return nil
}

func containedTimeTravelAddress(addr, guestAddress tcpip.Address) bool {
	return header.IPv4LoopbackSubnet.Contains(addr) || addr == header.IPv6Loopback || addr == guestAddress
}

func reverseEndpointID(id stack.TransportEndpointID) stack.TransportEndpointID {
	return stack.TransportEndpointID{
		LocalPort:     id.RemotePort,
		LocalAddress:  id.RemoteAddress,
		RemotePort:    id.LocalPort,
		RemoteAddress: id.LocalAddress,
	}
}

func resetTimeTravelChildConnections(s *sentrynetstack.Stack, guestAddress tcpip.Address) error {
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
		if ep.LiveForkExternal() || proxyPeer || (!info.ID.RemoteAddress.Unspecified() && !containedTimeTravelAddress(info.ID.RemoteAddress, guestAddress)) {
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
