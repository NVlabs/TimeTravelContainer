// Copyright 2018 The gVisor Authors.
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

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package netstack

import (
	"fmt"
	"maps"
	"slices"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/refs"
	"gvisor.dev/gvisor/pkg/sentry/inet"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/socket/netlink/nlmsg"
	"gvisor.dev/gvisor/pkg/syserr"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/link/packetsocket"
	"gvisor.dev/gvisor/pkg/tcpip/link/veth"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/nftables"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

// Stack implements inet.Stack for netstack/tcpip/stack.Stack.
//
// +stateify savable
type Stack struct {
	Stack *stack.Stack `state:".(*stack.Stack)"`

	eventSubscriber inet.InterfaceEventSubscriber

	// linkMu serializes link creation, modification and deletion.
	// It is a rough parallel to the per-netns rtnl_mutex in Linux.
	linkMu netstackLinkMutex `state:"nosave"`

	// id is a unique identifier for this stack, it is currently only
	// used for a deterministic lock ordering.
	id uint64

	// liveForkNetwork is runtime-only configuration for a contained link whose
	// external side must never enter serialized kernel state. Clone recreates a
	// new link/proxy realm in the child; reset then drops only inherited flows
	// that crossed that link.
	liveForkNetwork *LiveForkNetworkHooks `state:"nosave"`

	// liveForkQuarantine asks the runtime hooks to give this stack a link with
	// nothing on the other end of it.
	//
	// For a child forked to be asked a question and then destroyed. Its
	// connections stay up as far as the programs holding them can tell, what
	// they write is accepted and goes nowhere, what they read never arrives,
	// and nothing outside the sandbox learns that the copy existed at all.
	liveForkQuarantine bool `state:"nosave"`
}

// LiveForkNetworkHooks integrates a contained runtime link with live fork.
// All functions must be safe to call while the owning Kernel is paused.
type LiveForkNetworkHooks struct {
	Validate   func(*Stack) error
	Clone      func(dst *Stack) error
	ResetChild func(*Stack) error
	// SetQuarantine replaces only the runtime-owned external edge. Guest-owned
	// NICs, routes, namespaces, filters, and internal connections remain in the
	// Sentry graph.
	SetQuarantine func(*Stack, bool) error
	Close         func()
}

// SetLiveForkNetworkHooks installs runtime-only contained-network hooks. Any
// previous runtime realm is closed before replacement.
func (s *Stack) SetLiveForkNetworkHooks(hooks *LiveForkNetworkHooks) {
	if s.liveForkNetwork != nil && s.liveForkNetwork.Close != nil {
		s.liveForkNetwork.Close()
	}
	s.liveForkNetwork = hooks
}

// SetLiveForkQuarantine marks a child stack as one that must be cut off from
// the outside world. Set before the runtime hooks rebuild its links.
func (s *Stack) SetLiveForkQuarantine(quarantine bool) {
	s.liveForkQuarantine = quarantine
}

// LiveForkQuarantine reports whether this stack is a quarantined child.
func (s *Stack) LiveForkQuarantine() bool {
	return s.liveForkQuarantine
}

// SetLiveForkQuarantineState changes quarantine on a live, paused stack.
func (s *Stack) SetLiveForkQuarantineState(quarantine bool) error {
	if s.liveForkQuarantine == quarantine {
		return nil
	}
	hooks := s.liveForkNetwork
	if hooks == nil {
		// A netstack without a runtime-owned external edge (for example,
		// --network=none) needs no link replacement. Keep the semantic bit so
		// that later forks and restores continue to inherit it.
		s.liveForkQuarantine = quarantine
		return nil
	}
	if hooks.SetQuarantine == nil {
		return fmt.Errorf("network stack does not support changing TTC quarantine")
	}
	previous := s.liveForkQuarantine
	s.liveForkQuarantine = quarantine
	if err := hooks.SetQuarantine(s, quarantine); err != nil {
		s.liveForkQuarantine = previous
		if rollbackErr := hooks.SetQuarantine(s, previous); rollbackErr != nil {
			return fmt.Errorf("changing TTC quarantine: %w; restoring previous link: %v", err, rollbackErr)
		}
		return err
	}
	return nil
}

// NewStack creates a new netstack.Stack which wraps the given tcpip.Stack.
func NewStack(s *stack.Stack, id uint64) *Stack {
	return &Stack{
		Stack: s,
		id:    id,
	}
}

// AddInterfaceEventSubscriber implements inet.InterfaceEventPublisher.AddInterfaceEventSubscriber.
func (s *Stack) AddInterfaceEventSubscriber(sub inet.InterfaceEventSubscriber) {
	if s.eventSubscriber != nil {
		panic("AddInterfaceEventSubscriber called twice: multiple subscribers yet to be supported")
	}
	s.eventSubscriber = sub
}

func makeInterfaceInfo(ni *stack.NICInfo) inet.Interface {
	return inet.Interface{
		Name:       ni.Name,
		Addr:       []byte(ni.LinkAddress),
		Flags:      uint32(nicStateFlagsToLinux(ni.Flags)),
		DeviceType: toLinuxARPHardwareType(ni.ARPHardwareType),
		MTU:        ni.MTU,
		Master:     uint32(ni.Primary),
		Kind:       ni.Kind,
	}
}

func (s *Stack) sendChangeEvent(ctx context.Context, id tcpip.NICID) {
	if s.eventSubscriber == nil {
		return
	}
	if nicInfo, ok := s.Stack.SingleNICInfo(id); ok {
		s.eventSubscriber.OnInterfaceChangeEvent(ctx, int32(id), makeInterfaceInfo(nicInfo))
	}
}

func (s *Stack) sendDeleteEvent(ctx context.Context, id tcpip.NICID, nicInfo *stack.NICInfo) {
	if s.eventSubscriber == nil {
		return
	}
	s.eventSubscriber.OnInterfaceDeleteEvent(ctx, int32(id), makeInterfaceInfo(nicInfo))
}

// sendAddrEvent tells subscribers that an interface gained an address, or
// with added false, lost one. Netlink listeners such as Android's IpClient
// learn the interface's addresses from these events alone.
func (s *Stack) sendAddrEvent(idx int32, addr inet.InterfaceAddr, added bool) {
	if s.eventSubscriber == nil {
		return
	}
	// Delivery queues messages on the listeners' sockets, which needs no
	// task.
	s.eventSubscriber.OnInterfaceAddrEvent(context.Background(), idx, addr, added)
}

// Destroy implements inet.Stack.Destroy.
func (s *Stack) Destroy() {
	if s.liveForkNetwork != nil && s.liveForkNetwork.Close != nil {
		s.liveForkNetwork.Close()
		s.liveForkNetwork = nil
	}
	if s.Stack != nil {
		s.Stack.Close()
		refs.CleanupSync.Add(1)
		go func() {
			s.Stack.Wait()
			refs.CleanupSync.Done()
		}()
	}
}

// SupportsIPv6 implements Stack.SupportsIPv6.
func (s *Stack) SupportsIPv6() bool {
	return s.Stack.CheckNetworkProtocol(ipv6.ProtocolNumber)
}

// Converts Netstack's ARPHardwareType to equivalent linux constants.
func toLinuxARPHardwareType(t header.ARPHardwareType) uint16 {
	switch t {
	case header.ARPHardwareNone:
		return linux.ARPHRD_NONE
	case header.ARPHardwareLoopback:
		return linux.ARPHRD_LOOPBACK
	case header.ARPHardwareEther:
		return linux.ARPHRD_ETHER
	default:
		panic(fmt.Sprintf("unknown ARPHRD type: %d", t))
	}
}

// Interfaces implements inet.Stack.Interfaces.
func (s *Stack) Interfaces() map[int32]inet.Interface {
	is := make(map[int32]inet.Interface)
	for id, ni := range s.Stack.NICInfo() {
		is[int32(id)] = makeInterfaceInfo(&ni)
	}
	return is
}

// InterfaceIDs implements inet.Stack.InterfaceIDs.
func (s *Stack) InterfaceIDs() []int32 {
	// Since gVisor allocates NIC IDs monotonically (like Linux ifindex),
	// sorting by ID is equivalent to registration order.
	return slices.Sorted(maps.Keys(s.Interfaces()))
}

// RemoveInterface implements inet.Stack.RemoveInterface.
func (s *Stack) RemoveInterface(ctx context.Context, idx int32) error {
	s.linkMu.Lock()
	defer s.linkMu.Unlock()

	nic := tcpip.NICID(idx)

	nicInfo, ok := s.Stack.SingleNICInfo(nic)
	if !ok {
		return syserr.ErrUnknownNICID.ToError()
	}

	// Don't allow removing the loopback interface.
	if nicInfo.Flags.Loopback {
		return syserr.ErrNotSupported.ToError()
	}

	if err := syserr.TranslateNetstackError(s.Stack.RemoveNIC(nic)); err != nil {
		return err.ToError()
	}
	s.sendDeleteEvent(ctx, nic, nicInfo)
	return nil

}

// SetInterface implements inet.Stack.SetInterface.
func (s *Stack) SetInterface(ctx context.Context, msg *nlmsg.Message) *syserr.Error {
	var ifinfomsg linux.InterfaceInfoMessage
	attrsView, ok := msg.GetData(&ifinfomsg)
	if !ok {
		return syserr.ErrInvalidArgument
	}
	attrs, ok := attrsView.Parse()
	if !ok {
		return syserr.ErrInvalidArgument
	}
	ifname := ""
	for attr := range attrs {
		value := attrs[attr]
		switch attr {
		case linux.IFLA_IFNAME:
			if len(value) < 1 {
				return syserr.ErrInvalidArgument
			}
			if ifinfomsg.Index == 0 {
				ifname = value.String()
				for idx, ifa := range s.Interfaces() {
					if ifname == ifa.Name {
						ifinfomsg.Index = idx
						break
					}
				}
			}
		case linux.IFLA_MASTER:
		case linux.IFLA_LINKINFO:
		case linux.IFLA_ADDRESS:
		case linux.IFLA_MTU:
		case linux.IFLA_NET_NS_FD:
		case linux.IFLA_TXQLEN:
		default:
			ctx.Warningf("unexpected attribute: %x", attr)
			return syserr.ErrNotSupported
		}
	}
	flags := msg.Header().Flags
	if ifinfomsg.Index == 0 {
		if flags&linux.NLM_F_CREATE != 0 {
			return s.newInterface(ctx, msg, attrs)
		}
		return syserr.ErrNoDevice
	}

	if flags&(linux.NLM_F_EXCL|linux.NLM_F_REPLACE) != 0 {
		return syserr.ErrExists
	}
	changeUp, up := false, false
	if ifinfomsg.Flags != 0 || ifinfomsg.Change != 0 {
		if ifinfomsg.Change & ^uint32(linux.IFF_UP) != 0 {
			ctx.Warningf("Unsupported ifi_change flags: %x", ifinfomsg.Change)
			return syserr.ErrInvalidArgument
		}
		if ifinfomsg.Flags & ^uint32(linux.IFF_UP) != 0 {
			ctx.Warningf("Unsupported ifi_flags: %x", ifinfomsg.Flags)
			return syserr.ErrInvalidArgument
		}
		// A zero ifi_change makes ifi_flags replace every flag, as in Linux's
		// rtnl_dev_combine_flags.
		changeUp = ifinfomsg.Change == 0 || ifinfomsg.Change&linux.IFF_UP != 0
		up = ifinfomsg.Flags&linux.IFF_UP != 0
	}

	dstNs, err := s.lockSrcAndDst(ctx, attrs)
	if err != nil {
		return err
	}
	defer s.unlockSrcAndDst(ctx, dstNs)
	if changeUp {
		// Only the loopback device changes state, and Linux never moves it
		// to another namespace, so this precedes any IFLA_NET_NS_FD move.
		if err := s.setLinkUpLocked(ctx, tcpip.NICID(ifinfomsg.Index), up); err != nil {
			return err
		}
	}
	return s.setLinkLocked(ctx, tcpip.NICID(ifinfomsg.Index), attrs, dstNs)
}

// loopbackAutoAddresses are the addresses Linux assigns to a loopback device
// when it comes up: 127.0.0.1/8 (inetdev_event) and ::1/128 (addrconf).
var loopbackAutoAddresses = []inet.InterfaceAddr{
	{Family: linux.AF_INET, PrefixLen: 8, Addr: []byte{127, 0, 0, 1}},
	{Family: linux.AF_INET6, PrefixLen: 128, Addr: []byte{15: 1}},
}

// setLinkUpLocked brings interface id administratively up or down for an
// RTM_NEWLINK or SIOCSIFFLAGS request. Only the loopback device has such a
// state: other netstack interfaces are always up, and requests to change
// them are accepted without effect.
//
// As in Linux, bringing the loopback device up assigns it 127.0.0.1/8 and
// ::1/128 unless already assigned, and taking it down removes its IPv6
// addresses (addrconf_ifdown). The loopback device of a new network
// namespace therefore has no address until userspace brings it up, or adds
// one itself as Bubblewrap does before bringing it up.
//
// Preconditions: s.linkMu is locked.
func (s *Stack) setLinkUpLocked(ctx context.Context, id tcpip.NICID, up bool) *syserr.Error {
	info, ok := s.Stack.SingleNICInfo(id)
	if !ok {
		return syserr.ErrNoDevice
	}
	if !info.Flags.Loopback || info.Flags.Running == up {
		return nil
	}
	if up {
		if err := s.Stack.EnableNIC(id); err != nil {
			return syserr.TranslateNetstackError(err)
		}
		for _, addr := range loopbackAutoAddresses {
			if addr.Family == linux.AF_INET6 && s.Stack.NetworkProtocolInstance(ipv6.ProtocolNumber) == nil {
				continue
			}
			assigned := false
			for _, existing := range info.ProtocolAddresses {
				if existing.AddressWithPrefix.Address == tcpip.AddrFromSlice(addr.Addr) {
					assigned = true
					break
				}
			}
			if assigned {
				continue
			}
			if err := s.AddInterfaceAddr(int32(id), addr); err != nil && !linuxerr.Equals(linuxerr.EEXIST, err) {
				return syserr.FromError(err)
			}
		}
	} else {
		for _, existing := range info.ProtocolAddresses {
			if existing.Protocol != ipv6.ProtocolNumber {
				continue
			}
			addr := inet.InterfaceAddr{
				Family:    linux.AF_INET6,
				PrefixLen: uint8(existing.AddressWithPrefix.PrefixLen),
				Addr:      existing.AddressWithPrefix.Address.AsSlice(),
			}
			if err := s.RemoveInterfaceAddr(int32(id), addr); err != nil {
				return syserr.FromError(err)
			}
		}
		if err := s.Stack.DisableNIC(id); err != nil {
			return syserr.TranslateNetstackError(err)
		}
	}
	s.sendChangeEvent(ctx, id)
	return nil
}

// If the linkAttrs map contains IFLA_NET_NS_FD, locks the source and
// destination stacks and returns the destination netns, which must
// be passed into unlockSrcAndDst by the caller.
//
// If instead the linkAttrs map does not contain IFLA_NET_NS_FD, or if it
// points to the same netns as the source stack, only locks the source stack
// and returns nil.
//
// If the netns fd is invalid, or the calling context lacks CAP_NET_ADMIN,
// returns an error.
func (s *Stack) lockSrcAndDst(ctx context.Context, linkAttrs map[uint16]nlmsg.BytesView) (*inet.Namespace, *syserr.Error) {
	if linkAttrs == nil {
		s.linkMu.Lock()
		return nil, nil
	}
	v, ok := linkAttrs[linux.IFLA_NET_NS_FD]
	if !ok {
		s.linkMu.Lock()
		return nil, nil
	}

	fd, ok := v.Uint32()
	if !ok {
		return nil, syserr.ErrInvalidArgument
	}
	f := inet.NamespaceByFDFromContext(ctx)
	if f == nil {
		return nil, syserr.ErrInvalidArgument
	}
	ns, err := f(int32(fd))
	if err != nil {
		return nil, syserr.FromError(err)
	}
	if !auth.CredentialsFromContext(ctx).HasCapabilityIn(linux.CAP_NET_ADMIN, ns.UserNamespace()) {
		ns.DecRef(ctx)
		return nil, syserr.ErrNotPermittedNet
	}

	dst := ns.Stack().(*Stack)
	if s == dst {
		ns.DecRef(ctx)
		s.linkMu.Lock()
		return nil, nil
	}
	// No failures from this point on, ns.DecRef() is called in unlockSrcAndDst().

	if s.id < dst.id {
		s.linkMu.Lock()
		dst.linkMu.NestedLock(netstackLinkLockDeststack)
	} else {
		dst.linkMu.Lock()
		s.linkMu.NestedLock(netstackLinkLockDeststack)
	}
	return ns, nil
}

func (s *Stack) unlockSrcAndDst(ctx context.Context, ns *inet.Namespace) {
	if ns == nil {
		s.linkMu.Unlock()
		return
	}
	dst := ns.Stack().(*Stack)
	if s == dst {
		// This cannot happen because lockSrcAndDst() returns nil in this case.
		panic("unlockSrcAndDst called with dst == src")
	}

	if s.id < dst.id {
		dst.linkMu.NestedUnlock(netstackLinkLockDeststack)
		s.linkMu.Unlock()
	} else {
		s.linkMu.NestedUnlock(netstackLinkLockDeststack)
		dst.linkMu.Unlock()
	}
	ns.DecRef(ctx)
}

// precondition: s.linkLock is held. If dstNs is not nil, dstNs.Stack.linkMu is
// also held.
func (s *Stack) setLinkLocked(ctx context.Context, id tcpip.NICID, linkAttrs map[uint16]nlmsg.BytesView, dstNs *inet.Namespace) *syserr.Error {
	src := s
	changed := false
	nicInfo, ok := src.Stack.SingleNICInfo(id)
	if !ok {
		return syserr.ErrUnknownNICID
	}

	dst := src
	if dstNs != nil {
		dst = dstNs.Stack().(*Stack)
	}
	if dst != src {
		var err tcpip.Error
		oldID := id
		id, err = src.Stack.SetNICStack(id, dst.Stack)
		if err != nil {
			return syserr.TranslateNetstackError(err)
		}

		src.sendDeleteEvent(ctx, oldID, nicInfo) // inform about exit from old ns
		changed = true                           // inform about entry into new ns

		nicInfo, ok = dst.Stack.SingleNICInfo(id)
		if !ok {
			// Because we hold dst.linkMu, this should never happen.
			ctx.Warningf("Newly rehomed NIC %d not found in new stack %p", id, dst.Stack)
			return syserr.ErrUnknownNICID
		}
		src = dst

		// TODO (b/465141970): Once we support IFLA_LINK_NETNSID, we need to call sendChangeEvent on
		// the peer interface if this interface is part of a veth pair.
	}

	for t, v := range linkAttrs {
		switch t {
		case linux.IFLA_MASTER:
			master, ok := v.Uint32()
			if !ok {
				return syserr.ErrInvalidArgument
			}
			if mid, ok := src.Stack.GetNICCoordinatorID(id); ok && mid == tcpip.NICID(master) {
				continue
			}
			if master != 0 {
				if err := src.Stack.SetNICCoordinator(id, tcpip.NICID(master)); err != nil {
					return syserr.TranslateNetstackError(err)
				}
				changed = true
			}
		case linux.IFLA_ADDRESS:
			if len(v) != tcpip.LinkAddressSize {
				return syserr.ErrInvalidArgument
			}
			addr := tcpip.LinkAddress(v)
			if nicInfo.LinkAddress == addr {
				continue
			}
			if err := src.Stack.SetNICAddress(id, addr); err != nil {
				return syserr.TranslateNetstackError(err)
			}
			changed = true
		case linux.IFLA_IFNAME:
			if nicInfo.Name == v.String() {
				continue
			}
			if err := src.Stack.SetNICName(id, v.String()); err != nil {
				return syserr.TranslateNetstackError(err)
			}
			changed = true
		case linux.IFLA_MTU:
			mtu, ok := v.Uint32()
			if !ok {
				return syserr.ErrInvalidArgument
			}
			if nicInfo.MTU == mtu {
				continue
			}
			if err := src.Stack.SetNICMTU(id, mtu); err != nil {
				return syserr.TranslateNetstackError(err)
			}
			changed = true
		case linux.IFLA_TXQLEN:
			// TODO(b/340388892): support IFLA_TXQLEN.
		}
	}

	if changed {
		src.sendChangeEvent(ctx, id)
	}
	return nil
}

const defaultMTU = 1500

// allocDeviceName returns the first name of the form prefix+index, counting
// from zero, that no device in the stack has, the way Linux's dev_alloc_name
// names devices created without one. When the device is about to be moved to
// dstNs, the name has to be free there too.
func (s *Stack) allocDeviceName(prefix string, dstNs *inet.Namespace) string {
	taken := make(map[string]struct{})
	for _, nicInfo := range s.Stack.NICInfo() {
		taken[nicInfo.Name] = struct{}{}
	}
	if dstNs != nil {
		for _, nicInfo := range dstNs.Stack().(*Stack).Stack.NICInfo() {
			taken[nicInfo.Name] = struct{}{}
		}
	}
	for i := 0; ; i++ {
		name := fmt.Sprintf("%s%d", prefix, i)
		if _, ok := taken[name]; !ok {
			return name
		}
	}
}

func (s *Stack) newVeth(ctx context.Context, linkAttrs map[uint16]nlmsg.BytesView, linkInfoAttrs map[uint16]nlmsg.BytesView) *syserr.Error {
	var (
		linkInfoData  map[uint16]nlmsg.BytesView
		ifinfomsg     linux.InterfaceInfoMessage
		peerLinkAttrs map[uint16]nlmsg.BytesView
	)

	peerStack := s
	peerName := ""
	ifname := ""

	if v, ok := linkAttrs[linux.IFLA_IFNAME]; ok {
		ifname = v.String()
	}
	if value, ok := linkInfoAttrs[linux.IFLA_INFO_DATA]; ok {
		linkInfoData, ok = nlmsg.AttrsView(value).Parse()
		if !ok {
			return syserr.ErrInvalidArgument
		}
		if v, ok := linkInfoData[linux.VETH_INFO_PEER]; ok {
			if len(v) < ifinfomsg.SizeBytes() {
				return syserr.ErrInvalidArgument
			}
			attrsView := nlmsg.AttrsView(v[ifinfomsg.SizeBytes():])
			peerLinkAttrs, ok = attrsView.Parse()
			if !ok {
				return syserr.ErrInvalidArgument
			}
			if v, ok = peerLinkAttrs[linux.IFLA_IFNAME]; ok {
				peerName = v.String()
			}
			if v, ok = peerLinkAttrs[linux.IFLA_NET_NS_FD]; ok {
				fd, ok := v.Uint32()
				if !ok {
					return syserr.ErrInvalidArgument
				}
				f := inet.NamespaceByFDFromContext(ctx)
				if f == nil {
					return syserr.ErrInvalidArgument
				}
				ns, err := f(int32(fd))
				if err != nil {
					return syserr.FromError(err)
				}
				defer ns.DecRef(ctx)
				peerStack = ns.Stack().(*Stack)
			}
		}
	}

	dstNs, sysErr := s.lockSrcAndDst(ctx, linkAttrs)
	if sysErr != nil {
		return sysErr
	}
	ep, peerEP := veth.NewPair(defaultMTU, veth.DefaultBacklogSize)
	id := s.Stack.NextNICID()
	peerID := peerStack.Stack.NextNICID()
	if ifname == "" {
		ifname = s.allocDeviceName("veth", dstNs)
	}
	err := s.Stack.CreateNICWithOptions(id, packetsocket.New(ethernet.New(ep)), stack.NICOptions{
		Name: ifname,
		Kind: "veth",
	})
	if err != nil {
		s.unlockSrcAndDst(ctx, dstNs)
		peerEP.Close()
		return syserr.TranslateNetstackError(err)
	}
	if err := s.setLinkLocked(ctx, id, linkAttrs, dstNs); err != nil {
		s.unlockSrcAndDst(ctx, dstNs)
		peerEP.Close()
		return err
	}
	s.unlockSrcAndDst(ctx, dstNs)

	peerDstNs, sysErr := peerStack.lockSrcAndDst(ctx, peerLinkAttrs)
	if sysErr != nil {
		peerEP.Close()
		return sysErr
	}
	defer peerStack.unlockSrcAndDst(ctx, peerDstNs)

	if peerName == "" {
		peerName = peerStack.allocDeviceName("veth", peerDstNs)
	}

	err = peerStack.Stack.CreateNICWithOptions(peerID, packetsocket.New(ethernet.New(peerEP)), stack.NICOptions{
		Name: peerName,
		Kind: "veth",
	})
	if err != nil {
		peerEP.Close()
		return syserr.TranslateNetstackError(err)
	}
	if peerLinkAttrs != nil {
		if err := peerStack.setLinkLocked(ctx, peerID, peerLinkAttrs, peerDstNs); err != nil {
			peerStack.Stack.RemoveNIC(peerID)
			peerEP.Close()
			return err
		}
	}

	return nil
}

func (s *Stack) newBridge(ctx context.Context, linkAttrs map[uint16]nlmsg.BytesView, linkInfoAttrs map[uint16]nlmsg.BytesView) *syserr.Error {
	dstNs, sysErr := s.lockSrcAndDst(ctx, linkAttrs)
	if sysErr != nil {
		return sysErr
	}
	defer s.unlockSrcAndDst(ctx, dstNs)

	ifname := ""

	if v, ok := linkAttrs[linux.IFLA_IFNAME]; ok {
		ifname = v.String()
	}
	if ifname == "" {
		ifname = s.allocDeviceName("bridge", dstNs)
	}
	ep := stack.NewBridgeEndpoint(defaultMTU)
	id := s.Stack.NextNICID()
	err := s.Stack.CreateNICWithOptions(id, ep, stack.NICOptions{
		Name: ifname,
		Kind: "bridge",
	})
	if err != nil {
		return syserr.TranslateNetstackError(err)
	}
	if err := s.setLinkLocked(ctx, id, linkAttrs, dstNs); err != nil {
		return err
	}

	return nil
}

func (s *Stack) newInterface(ctx context.Context, msg *nlmsg.Message, linkAttrs map[uint16]nlmsg.BytesView) *syserr.Error {
	var (
		linkInfoAttrs map[uint16]nlmsg.BytesView
		kind          string
	)

	if v, ok := linkAttrs[linux.IFLA_LINKINFO]; ok {
		linkInfoAttrs, ok = nlmsg.AttrsView(v).Parse()
		if !ok {
			return syserr.ErrInvalidArgument
		}

		for attr := range linkInfoAttrs {
			value := linkInfoAttrs[attr]
			switch attr {
			case linux.IFLA_INFO_KIND:
				kind = value.String()
			case linux.IFLA_INFO_DATA:
			default:
				ctx.Warningf("unexpected link info attribute: %x", attr)
				return syserr.ErrNotSupported
			}
		}
	}
	switch kind {
	case "":
		return syserr.ErrInvalidArgument
	case "bridge":
		return s.newBridge(ctx, linkAttrs, linkInfoAttrs)
	case "veth":
		return s.newVeth(ctx, linkAttrs, linkInfoAttrs)
	}
	return syserr.ErrNotSupported
}

// InterfaceAddrs implements inet.Stack.InterfaceAddrs.
func (s *Stack) InterfaceAddrs() map[int32][]inet.InterfaceAddr {
	nicAddrs := make(map[int32][]inet.InterfaceAddr)
	for id, ni := range s.Stack.NICInfo() {
		var addrs []inet.InterfaceAddr
		for _, a := range ni.ProtocolAddresses {
			var family uint8
			switch a.Protocol {
			case ipv4.ProtocolNumber:
				family = linux.AF_INET
			case ipv6.ProtocolNumber:
				family = linux.AF_INET6
			default:
				log.Warningf("Unknown network protocol in %+v", a)
				continue
			}

			addrCopy := a.AddressWithPrefix.Address
			addrs = append(addrs, inet.InterfaceAddr{
				Family:    family,
				PrefixLen: uint8(a.AddressWithPrefix.PrefixLen),
				Addr:      addrCopy.AsSlice(),
				// TODO(b/68878065): Other fields.
			})
		}
		nicAddrs[int32(id)] = addrs
	}
	return nicAddrs
}

// convertAddr converts an InterfaceAddr to a ProtocolAddress.
func convertAddr(addr inet.InterfaceAddr) (tcpip.ProtocolAddress, error) {
	var (
		protocol        tcpip.NetworkProtocolNumber
		address         tcpip.Address
		protocolAddress tcpip.ProtocolAddress
	)
	switch addr.Family {
	case linux.AF_INET:
		if len(addr.Addr) != header.IPv4AddressSize {
			return protocolAddress, linuxerr.EINVAL
		}
		if addr.PrefixLen > header.IPv4AddressSize*8 {
			return protocolAddress, linuxerr.EINVAL
		}
		protocol = ipv4.ProtocolNumber
		address = tcpip.AddrFrom4Slice(addr.Addr)
	case linux.AF_INET6:
		if len(addr.Addr) != header.IPv6AddressSize {
			return protocolAddress, linuxerr.EINVAL
		}
		if addr.PrefixLen > header.IPv6AddressSize*8 {
			return protocolAddress, linuxerr.EINVAL
		}
		protocol = ipv6.ProtocolNumber
		address = tcpip.AddrFrom16Slice(addr.Addr)
	default:
		return protocolAddress, linuxerr.ENOTSUP
	}

	protocolAddress = tcpip.ProtocolAddress{
		Protocol: protocol,
		AddressWithPrefix: tcpip.AddressWithPrefix{
			Address:   address,
			PrefixLen: int(addr.PrefixLen),
		},
	}
	return protocolAddress, nil
}

// AddInterfaceAddr implements inet.Stack.AddInterfaceAddr.
func (s *Stack) AddInterfaceAddr(idx int32, addr inet.InterfaceAddr) error {
	protocolAddress, err := convertAddr(addr)
	if err != nil {
		return err
	}

	// Attach address to interface.
	nicID := tcpip.NICID(idx)
	if err := s.Stack.AddProtocolAddress(nicID, protocolAddress, stack.AddressProperties{}); err != nil {
		return syserr.TranslateNetstackError(err).ToError()
	}
	s.sendAddrEvent(idx, addr, true /* added */)

	// Add route for local network if it doesn't exist already.
	localRoute := tcpip.Route{
		Destination: protocolAddress.AddressWithPrefix.Subnet(),
		Gateway:     tcpip.Address{}, // No gateway for local network.
		NIC:         nicID,
	}

	for _, rt := range s.Stack.GetRouteTable() {
		if rt.Equal(localRoute) {
			return nil
		}
	}

	// Local route does not exist yet. Add it.
	s.Stack.AddRoute(localRoute)

	return nil
}

// RemoveInterfaceAddr implements inet.Stack.RemoveInterfaceAddr.
func (s *Stack) RemoveInterfaceAddr(idx int32, addr inet.InterfaceAddr) error {
	protocolAddress, err := convertAddr(addr)
	if err != nil {
		return err
	}

	// Remove addresses matching the address and prefix.
	nicID := tcpip.NICID(idx)
	if err := s.Stack.RemoveAddress(nicID, protocolAddress.AddressWithPrefix.Address); err != nil {
		return syserr.TranslateNetstackError(err).ToError()
	}
	s.sendAddrEvent(idx, addr, false /* added */)

	// Remove the corresponding local network route if it exists.
	localRoute := tcpip.Route{
		Destination: protocolAddress.AddressWithPrefix.Subnet(),
		Gateway:     tcpip.Address{}, // No gateway for local network.
		NIC:         nicID,
	}
	s.Stack.RemoveRoutes(func(rt tcpip.Route) bool {
		return rt.Equal(localRoute)
	})

	return nil
}

// TCPReceiveBufferSize implements inet.Stack.TCPReceiveBufferSize.
func (s *Stack) TCPReceiveBufferSize() (inet.TCPBufferSize, error) {
	var rs tcpip.TCPReceiveBufferSizeRangeOption
	err := s.Stack.TransportProtocolOption(tcp.ProtocolNumber, &rs)
	return inet.TCPBufferSize{
		Min:     rs.Min,
		Default: rs.Default,
		Max:     rs.Max,
	}, syserr.TranslateNetstackError(err).ToError()
}

// SetTCPReceiveBufferSize implements inet.Stack.SetTCPReceiveBufferSize.
func (s *Stack) SetTCPReceiveBufferSize(size inet.TCPBufferSize) error {
	rs := tcpip.TCPReceiveBufferSizeRangeOption{
		Min:     size.Min,
		Default: size.Default,
		Max:     size.Max,
	}
	return syserr.TranslateNetstackError(s.Stack.SetTransportProtocolOption(tcp.ProtocolNumber, &rs)).ToError()
}

// TCPSendBufferSize implements inet.Stack.TCPSendBufferSize.
func (s *Stack) TCPSendBufferSize() (inet.TCPBufferSize, error) {
	var ss tcpip.TCPSendBufferSizeRangeOption
	err := s.Stack.TransportProtocolOption(tcp.ProtocolNumber, &ss)
	return inet.TCPBufferSize{
		Min:     ss.Min,
		Default: ss.Default,
		Max:     ss.Max,
	}, syserr.TranslateNetstackError(err).ToError()
}

// SetTCPSendBufferSize implements inet.Stack.SetTCPSendBufferSize.
func (s *Stack) SetTCPSendBufferSize(size inet.TCPBufferSize) error {
	ss := tcpip.TCPSendBufferSizeRangeOption{
		Min:     size.Min,
		Default: size.Default,
		Max:     size.Max,
	}
	return syserr.TranslateNetstackError(s.Stack.SetTransportProtocolOption(tcp.ProtocolNumber, &ss)).ToError()
}

// TCPSACKEnabled implements inet.Stack.TCPSACKEnabled.
func (s *Stack) TCPSACKEnabled() (bool, error) {
	var sack tcpip.TCPSACKEnabled
	err := s.Stack.TransportProtocolOption(tcp.ProtocolNumber, &sack)
	return bool(sack), syserr.TranslateNetstackError(err).ToError()
}

// SetTCPSACKEnabled implements inet.Stack.SetTCPSACKEnabled.
func (s *Stack) SetTCPSACKEnabled(enabled bool) error {
	opt := tcpip.TCPSACKEnabled(enabled)
	return syserr.TranslateNetstackError(s.Stack.SetTransportProtocolOption(tcp.ProtocolNumber, &opt)).ToError()
}

// TCPRecovery implements inet.Stack.TCPRecovery.
func (s *Stack) TCPRecovery() (inet.TCPLossRecovery, error) {
	var recovery tcpip.TCPRecovery
	if err := s.Stack.TransportProtocolOption(tcp.ProtocolNumber, &recovery); err != nil {
		return 0, syserr.TranslateNetstackError(err).ToError()
	}
	return inet.TCPLossRecovery(recovery), nil
}

// SetTCPRecovery implements inet.Stack.SetTCPRecovery.
func (s *Stack) SetTCPRecovery(recovery inet.TCPLossRecovery) error {
	opt := tcpip.TCPRecovery(recovery)
	return syserr.TranslateNetstackError(s.Stack.SetTransportProtocolOption(tcp.ProtocolNumber, &opt)).ToError()
}

// TCPCongestionControl returns the stack-wide default TCP congestion control
// algorithm.
func (s *Stack) TCPCongestionControl() (string, error) {
	var opt tcpip.CongestionControlOption
	err := s.Stack.TransportProtocolOption(tcp.ProtocolNumber, &opt)
	return string(opt), syserr.TranslateNetstackError(err).ToError()
}

// SetTCPCongestionControl changes the stack-wide default TCP congestion
// control algorithm.
func (s *Stack) SetTCPCongestionControl(name string) error {
	opt := tcpip.CongestionControlOption(name)
	return syserr.TranslateNetstackError(s.Stack.SetTransportProtocolOption(tcp.ProtocolNumber, &opt)).ToError()
}

// TCPAvailableCongestionControl returns the TCP congestion control algorithms
// implemented by netstack.
func (s *Stack) TCPAvailableCongestionControl() (string, error) {
	var opt tcpip.TCPAvailableCongestionControlOption
	err := s.Stack.TransportProtocolOption(tcp.ProtocolNumber, &opt)
	return string(opt), syserr.TranslateNetstackError(err).ToError()
}

// TCPSynRetries returns the stack-wide SYN retransmission limit.
func (s *Stack) TCPSynRetries() (uint8, error) {
	var opt tcpip.TCPSynRetriesOption
	err := s.Stack.TransportProtocolOption(tcp.ProtocolNumber, &opt)
	return uint8(opt), syserr.TranslateNetstackError(err).ToError()
}

// SetTCPSynRetries changes the stack-wide SYN retransmission limit.
func (s *Stack) SetTCPSynRetries(retries uint8) error {
	opt := tcpip.TCPSynRetriesOption(retries)
	return syserr.TranslateNetstackError(s.Stack.SetTransportProtocolOption(tcp.ProtocolNumber, &opt)).ToError()
}

// TCPMaxRetries returns the stack-wide established-connection retransmission
// limit exposed by Linux as tcp_retries2.
func (s *Stack) TCPMaxRetries() (uint8, error) {
	var opt tcpip.TCPMaxRetriesOption
	err := s.Stack.TransportProtocolOption(tcp.ProtocolNumber, &opt)
	return uint8(opt), syserr.TranslateNetstackError(err).ToError()
}

// SetTCPMaxRetries changes the stack-wide established-connection
// retransmission limit exposed by Linux as tcp_retries2.
func (s *Stack) SetTCPMaxRetries(retries uint8) error {
	opt := tcpip.TCPMaxRetriesOption(retries)
	return syserr.TranslateNetstackError(s.Stack.SetTransportProtocolOption(tcp.ProtocolNumber, &opt)).ToError()
}

// Statistics implements inet.Stack.Statistics.
func (s *Stack) Statistics(stat any, arg string) error {
	netStats := s.Stats()
	switch stats := stat.(type) {
	case *inet.StatDev:
		for _, ni := range s.Stack.NICInfo() {
			if ni.Name != arg {
				continue
			}
			// TODO(gvisor.dev/issue/2103) Support stubbed stats.
			*stats = inet.StatDev{
				// Receive section.
				ni.Stats.Rx.Bytes.Value(),   // bytes.
				ni.Stats.Rx.Packets.Value(), // packets.
				0,                           // errs.
				0,                           // drop.
				0,                           // fifo.
				0,                           // frame.
				0,                           // compressed.
				0,                           // multicast.
				// Transmit section.
				ni.Stats.Tx.Bytes.Value(),   // bytes.
				ni.Stats.Tx.Packets.Value(), // packets.
				0,                           // errs.
				0,                           // drop.
				0,                           // fifo.
				0,                           // colls.
				0,                           // carrier.
				0,                           // compressed.
			}
			break
		}
	case *inet.StatSNMPIP:
		ip := netStats.IP
		// TODO(gvisor.dev/issue/969) Support stubbed stats.
		*stats = inet.StatSNMPIP{
			0,                          // Ip/Forwarding.
			0,                          // Ip/DefaultTTL.
			ip.PacketsReceived.Value(), // InReceives.
			0,                          // Ip/InHdrErrors.
			ip.InvalidDestinationAddressesReceived.Value(), // InAddrErrors.
			0,                               // Ip/ForwDatagrams.
			0,                               // Ip/InUnknownProtos.
			0,                               // Ip/InDiscards.
			ip.PacketsDelivered.Value(),     // InDelivers.
			ip.PacketsSent.Value(),          // OutRequests.
			ip.OutgoingPacketErrors.Value(), // OutDiscards.
			0,                               // Ip/OutNoRoutes.
			0,                               // Support Ip/ReasmTimeout.
			0,                               // Support Ip/ReasmReqds.
			0,                               // Support Ip/ReasmOKs.
			0,                               // Support Ip/ReasmFails.
			0,                               // Support Ip/FragOKs.
			0,                               // Support Ip/FragFails.
			0,                               // Support Ip/FragCreates.
		}
	case *inet.StatSNMPICMP:
		in := netStats.ICMP.V4.PacketsReceived.ICMPv4PacketStats
		out := netStats.ICMP.V4.PacketsSent.ICMPv4PacketStats
		// TODO(gvisor.dev/issue/969) Support stubbed stats.
		*stats = inet.StatSNMPICMP{
			0, // Icmp/InMsgs.
			Metrics.ICMP.V4.PacketsSent.Dropped.Value(), // InErrors.
			0,                         // Icmp/InCsumErrors.
			in.DstUnreachable.Value(), // InDestUnreachs.
			in.TimeExceeded.Value(),   // InTimeExcds.
			in.ParamProblem.Value(),   // InParmProbs.
			in.SrcQuench.Value(),      // InSrcQuenchs.
			in.Redirect.Value(),       // InRedirects.
			in.EchoRequest.Value(),    // InEchos.
			in.EchoReply.Value(),      // InEchoReps.
			in.Timestamp.Value(),      // InTimestamps.
			in.TimestampReply.Value(), // InTimestampReps.
			in.InfoRequest.Value(),    // InAddrMasks.
			in.InfoReply.Value(),      // InAddrMaskReps.
			0,                         // Icmp/OutMsgs.
			Metrics.ICMP.V4.PacketsReceived.Invalid.Value(), // OutErrors.
			out.DstUnreachable.Value(),                      // OutDestUnreachs.
			out.TimeExceeded.Value(),                        // OutTimeExcds.
			out.ParamProblem.Value(),                        // OutParmProbs.
			out.SrcQuench.Value(),                           // OutSrcQuenchs.
			out.Redirect.Value(),                            // OutRedirects.
			out.EchoRequest.Value(),                         // OutEchos.
			out.EchoReply.Value(),                           // OutEchoReps.
			out.Timestamp.Value(),                           // OutTimestamps.
			out.TimestampReply.Value(),                      // OutTimestampReps.
			out.InfoRequest.Value(),                         // OutAddrMasks.
			out.InfoReply.Value(),                           // OutAddrMaskReps.
		}
	case *inet.StatSNMPTCP:
		tcp := netStats.TCP
		// RFC 2012 (updates 1213):  SNMPv2-MIB-TCP.
		*stats = inet.StatSNMPTCP{
			1,                                     // RtoAlgorithm.
			200,                                   // RtoMin.
			120000,                                // RtoMax.
			(1<<64 - 1),                           // MaxConn.
			tcp.ActiveConnectionOpenings.Value(),  // ActiveOpens.
			tcp.PassiveConnectionOpenings.Value(), // PassiveOpens.
			tcp.FailedConnectionAttempts.Value(),  // AttemptFails.
			tcp.EstablishedResets.Value(),         // EstabResets.
			tcp.CurrentEstablished.Value(),        // CurrEstab.
			tcp.ValidSegmentsReceived.Value(),     // InSegs.
			tcp.SegmentsSent.Value(),              // OutSegs.
			tcp.Retransmits.Value(),               // RetransSegs.
			tcp.InvalidSegmentsReceived.Value(),   // InErrs.
			tcp.ResetsSent.Value(),                // OutRsts.
			tcp.ChecksumErrors.Value(),            // InCsumErrors.
		}
	case *inet.StatSNMPUDP:
		udp := netStats.UDP
		// TODO(gvisor.dev/issue/969) Support stubbed stats.
		*stats = inet.StatSNMPUDP{
			udp.PacketsReceived.Value(),     // InDatagrams.
			udp.UnknownPortErrors.Value(),   // NoPorts.
			0,                               // Udp/InErrors.
			udp.PacketsSent.Value(),         // OutDatagrams.
			udp.ReceiveBufferErrors.Value(), // RcvbufErrors.
			0,                               // Udp/SndbufErrors.
			udp.ChecksumErrors.Value(),      // Udp/InCsumErrors.
			0,                               // Udp/IgnoredMulti.
		}
	default:
		return syserr.ErrEndpointOperation.ToError()
	}
	return nil
}

// Stats implements inet.Stack.Stats.
func (s *Stack) Stats() tcpip.Stats {
	return s.Stack.Stats()
}

// RouteTable implements inet.Stack.RouteTable.
func (s *Stack) RouteTable() []inet.Route {
	var routeTable []inet.Route

	for _, rt := range s.Stack.GetRouteTable() {
		var family uint8
		switch rt.Destination.ID().BitLen() {
		case header.IPv4AddressSizeBits:
			family = linux.AF_INET
		case header.IPv6AddressSizeBits:
			family = linux.AF_INET6
		default:
			log.Warningf("Unknown network protocol in route %+v", rt)
			continue
		}

		dstAddr := rt.Destination.ID()
		routeTable = append(routeTable, inet.Route{
			Family: family,
			DstLen: uint8(rt.Destination.Prefix()), // The CIDR prefix for the destination.

			// Always return unspecified protocol since we have no notion of
			// protocol for routes.
			Protocol: linux.RTPROT_UNSPEC,
			// Set statically to LINK scope for now.
			//
			// TODO(gvisor.dev/issue/595): Set scope for routes.
			Scope: linux.RT_SCOPE_LINK,
			Type:  linux.RTN_UNICAST,

			DstAddr:         dstAddr.AsSlice(),
			OutputInterface: int32(rt.NIC),
			GatewayAddr:     rt.Gateway.AsSlice(),
		})
	}

	return routeTable
}

// localRoute constructs a local route from the netlink message.
func (s *Stack) localRoute(msg *nlmsg.Message) (tcpip.Route, *syserr.Error) {
	var rtMsg linux.RouteMessage
	attrs, ok := msg.GetData(&rtMsg)
	if !ok {
		return tcpip.Route{}, syserr.ErrInvalidArgument
	}

	route := inet.Route{
		Family:   rtMsg.Family,
		DstLen:   rtMsg.DstLen,
		SrcLen:   rtMsg.SrcLen,
		TOS:      rtMsg.TOS,
		Table:    rtMsg.Table,
		Protocol: rtMsg.Protocol,
		Scope:    rtMsg.Scope,
		Type:     rtMsg.Type,
		Flags:    rtMsg.Flags,
	}

	for !attrs.Empty() {
		ahdr, value, rest, ok := attrs.ParseFirst()
		if !ok {
			return tcpip.Route{}, syserr.ErrInvalidArgument
		}
		attrs = rest

		switch ahdr.Type {
		case linux.RTA_DST:
			if len(value) < 1 {
				return tcpip.Route{}, syserr.ErrInvalidArgument
			}
			route.DstAddr = value
		case linux.RTA_SRC:
			if len(value) < 1 {
				return tcpip.Route{}, syserr.ErrInvalidArgument
			}
			route.SrcAddr = value
		case linux.RTA_OIF:
			oif := nlmsg.BytesView(value)
			outputInterface, ok := oif.Int32()
			if !ok {
				return tcpip.Route{}, syserr.ErrInvalidArgument
			}
			if _, exist := s.Interfaces()[outputInterface]; !exist {
				return tcpip.Route{}, syserr.ErrNoDevice
			}
			route.OutputInterface = outputInterface
		case linux.RTA_GATEWAY:
			if len(value) < 1 {
				return tcpip.Route{}, syserr.ErrInvalidArgument
			}
			route.GatewayAddr = value
		case linux.RTA_PRIORITY:
		default:
			log.Warningf("Unknown attribute: %v", ahdr.Type)
			return tcpip.Route{}, syserr.ErrNotSupported
		}
	}
	var dest tcpip.Subnet
	// When no destination address is provided, the new route might be the default route.
	if route.DstAddr == nil {
		if route.GatewayAddr == nil {
			return tcpip.Route{}, syserr.ErrInvalidArgument
		}
		switch len(route.GatewayAddr) {
		case header.IPv4AddressSize:
			subnet, err := tcpip.NewSubnet(tcpip.AddrFromSlice(tcpip.IPv4Zero), tcpip.MaskFromBytes(tcpip.IPv4Zero))
			if err != nil {
				return tcpip.Route{}, syserr.ErrInvalidArgument
			}
			dest = subnet
		case header.IPv6AddressSize:
			subnet, err := tcpip.NewSubnet(tcpip.AddrFromSlice(tcpip.IPv6Zero), tcpip.MaskFromBytes(tcpip.IPv6Zero))
			if err != nil {
				return tcpip.Route{}, syserr.ErrInvalidArgument
			}
			dest = subnet
		default:
			return tcpip.Route{}, syserr.ErrInvalidArgument
		}
	} else {
		dest = tcpip.AddressWithPrefix{
			Address:   tcpip.AddrFromSlice(route.DstAddr),
			PrefixLen: int(route.DstLen)}.Subnet()
	}

	localRoute := tcpip.Route{
		Destination: dest,
		Gateway:     tcpip.AddrFromSlice(route.GatewayAddr),
		NIC:         tcpip.NICID(route.OutputInterface),
	}

	if len(route.SrcAddr) != 0 {
		localRoute.SourceHint = tcpip.AddrFromSlice(route.SrcAddr)
	}

	return localRoute, nil
}

// RemoveRoute implements inte.Stack.RemoveRoute.
func (s *Stack) RemoveRoute(ctx context.Context, msg *nlmsg.Message) *syserr.Error {
	localRoute, err := s.localRoute(msg)
	if err != nil {
		return err
	}
	if removed := s.Stack.RemoveRoutes(func(rt tcpip.Route) bool {
		// Both gateway and NIC are compared with existing routes
		// only when they are present in the netlink message.
		if localRoute.Gateway.Len() > 0 && !localRoute.Gateway.Equal(rt.Gateway) {
			return false
		}
		if localRoute.NIC > 0 && localRoute.NIC != rt.NIC {
			return false
		}
		return rt.Destination.Equal(localRoute.Destination)
	}); removed == 0 {
		return syserr.ErrNoProcess
	}
	return nil
}

// NewRoute implements inet.Stack.NewRoute.
func (s *Stack) NewRoute(ctx context.Context, msg *nlmsg.Message) *syserr.Error {
	localRoute, err := s.localRoute(msg)
	if err != nil {
		return err
	}
	found := false
	for _, rt := range s.Stack.GetRouteTable() {
		if localRoute.Equal(rt) {
			found = true
			break
		}
	}
	flags := msg.Header().Flags
	switch {
	case !found && flags&linux.NLM_F_CREATE == linux.NLM_F_CREATE:
		s.Stack.AddRoute(localRoute)
	case found && flags&linux.NLM_F_REPLACE != linux.NLM_F_REPLACE:
		return syserr.ErrExists
	}
	if flags&linux.NLM_F_REPLACE == linux.NLM_F_REPLACE {
		s.Stack.ReplaceRoute(localRoute)
	}
	return nil
}

// IPTables returns the stack's iptables.
func (s *Stack) IPTables() (*stack.IPTables, error) {
	return s.Stack.IPTables(), nil
}

// Pause implements inet.Stack.Pause.
func (s *Stack) Pause() {
	s.Stack.Pause()
}

// Restore implements inet.Stack.Restore.
func (s *Stack) Restore() {
	s.Stack.Restore()
}

// ResetConfig implements inet.Stack.ResetConfig.
func (s *Stack) ResetConfig() {
	s.Stack.ResetConfig()
}

// ReplaceConfig replaces config in the loaded stack.
func (s *Stack) ReplaceConfig(st inet.Stack) {
	if _, ok := st.(*Stack); !ok {
		panic("netstack.Stack cannot be nil when netstack s/r is enabled")
	}
	s.Stack.ReplaceConfig(st.(*Stack).Stack)
}

// Resume implements inet.Stack.Resume.
func (s *Stack) Resume() {
	s.Stack.Resume()
}

// RegisteredEndpoints implements inet.Stack.RegisteredEndpoints.
func (s *Stack) RegisteredEndpoints() []stack.TransportEndpoint {
	return s.Stack.RegisteredEndpoints()
}

// OwnsLiveForkDanglingEndpoint implements inet.LiveForkDanglingEndpointOwner.
func (s *Stack) OwnsLiveForkDanglingEndpoint(endpoint tcpip.Endpoint) bool {
	tcpEndpoint, ok := endpoint.(*tcp.Endpoint)
	return ok && tcpEndpoint.OwnedByStack(s.Stack)
}

// CleanupEndpoints implements inet.Stack.CleanupEndpoints.
func (s *Stack) CleanupEndpoints() []stack.TransportEndpoint {
	return s.Stack.CleanupEndpoints()
}

// RestoreCleanupEndpoints implements inet.Stack.RestoreCleanupEndpoints.
func (s *Stack) RestoreCleanupEndpoints(es []stack.TransportEndpoint) {
	s.Stack.RestoreCleanupEndpoints(es)
}

// SetForwarding implements inet.Stack.SetForwarding.
func (s *Stack) SetForwarding(protocol tcpip.NetworkProtocolNumber, enable bool) error {
	if err := s.Stack.SetForwardingDefaultAndAllNICs(protocol, enable); err != nil {
		return fmt.Errorf("SetForwardingDefaultAndAllNICs(%d, %t): %s", protocol, enable, err)
	}
	return nil
}

// GetAllowExternalLoopbackTraffic implements inet.Stack.GetAllowExternalLoopbackTraffic.
func (s *Stack) GetAllowExternalLoopbackTraffic(protocol tcpip.NetworkProtocolNumber) (bool, error) {
	var opt tcpip.AllowExternalLoopbackTrafficOption
	if err := s.Stack.NetworkProtocolOption(protocol, &opt); err != nil {
		return false, fmt.Errorf("NetworkProtocolOption(%d, ...): %s", protocol, err)
	}
	return bool(opt), nil
}

// SetAllowExternalLoopbackTraffic implements inet.Stack.SetAllowExternalLoopbackTraffic.
func (s *Stack) SetAllowExternalLoopbackTraffic(protocol tcpip.NetworkProtocolNumber, enable bool) error {
	opt := tcpip.AllowExternalLoopbackTrafficOption(enable)
	if err := s.Stack.SetNetworkProtocolOption(protocol, &opt); err != nil {
		return fmt.Errorf("SetNetworkProtocolOption(%d, &%t): %s", protocol, enable, err)
	}
	return nil
}

// PortRange implements inet.Stack.PortRange.
func (s *Stack) PortRange() (uint16, uint16) {
	return s.Stack.PortRange()
}

// SetPortRange implements inet.Stack.SetPortRange.
func (s *Stack) SetPortRange(start uint16, end uint16) error {
	return syserr.TranslateNetstackError(s.Stack.SetPortRange(start, end)).ToError()
}

// SetRemoveConf implements inet.Stack.SetRemoveConf.
func (s *Stack) SetRemoveConf(removeConf bool) {
	s.Stack.SetRemoveConf(removeConf)
}

// SetLiveForkSaveConfig implements inet.LiveForkStackConfigSaver.
func (s *Stack) SetLiveForkSaveConfig(enabled bool) {
	s.Stack.SetLiveForkSaveConfig(enabled)
}

// HasLiveForkRestoredConfig implements inet.LiveForkStackConfigRestorer.
func (s *Stack) HasLiveForkRestoredConfig() bool {
	return s.Stack.HasLiveForkRestoredConfig()
}

// PrepareLiveForkRestore implements inet.LiveForkStackConfigRestorer.
func (s *Stack) PrepareLiveForkRestore() error {
	if err := s.Stack.PrepareLiveForkRestore(); err != nil {
		return err
	}
	// The nftables interpreter is omitted because eligible TTC families reject
	// mutation. Recreate the empty interpreter for every restored namespace,
	// not only the root namespace that receives runtime network hooks.
	if s.Stack.NFTables() == nil && nftables.IsNFTablesEnabled() {
		s.Stack.SetNFTables(nftables.NewNFTables(s.Stack, s.Stack.Clock(), s.Stack.SecureRNG()))
	}
	return nil
}

// ValidateLiveFork implements inet.LiveForkStackValidator. A netstack without
// runtime hooks is wholly represented in the Sentry state graph; this includes
// guest-created network namespaces, veth pairs, routes, and filtering state.
// Runtime hooks are required for links whose peer lives outside that graph.
func (s *Stack) ValidateLiveFork() error {
	if s.liveForkNetwork != nil {
		if s.liveForkNetwork.Validate == nil {
			return fmt.Errorf("contained live-fork network has no validator")
		}
		return s.liveForkNetwork.Validate(s)
	}
	return nil
}

// CloneLiveForkConfig implements inet.LiveForkStackConfigCloner. Sentry-owned
// configuration is already present in the decoded child. A runtime hook only
// reconstructs the deliberately unsaved external side of a managed link.
func (s *Stack) CloneLiveForkConfig(dst inet.Stack) error {
	if err := s.ValidateLiveFork(); err != nil {
		return err
	}
	dstStack, ok := dst.(*Stack)
	if !ok {
		return fmt.Errorf("cannot clone netstack config into %T", dst)
	}
	if dstStack.Stack.IPTables() == nil {
		tables, ok := s.Stack.IPTables().CloneUnmodifiedForLiveFork(dstStack.Stack.Clock())
		if !ok {
			return fmt.Errorf("modified iptables state was absent from live-fork metadata")
		}
		dstStack.Stack.SetIPTables(tables)
	}
	// nftables is not stateified by upstream netstack. Eligible TTC guests do
	// not receive NETLINK_NETFILTER mutation support, so recreating the empty
	// interpreter preserves their observable configuration.
	if s.Stack.NFTables() != nil && dstStack.Stack.NFTables() == nil {
		dstStack.Stack.SetNFTables(nftables.NewNFTables(dstStack.Stack, dstStack.Stack.Clock(), dstStack.Stack.SecureRNG()))
	}
	if s.liveForkNetwork != nil {
		if s.liveForkNetwork.Clone == nil {
			return fmt.Errorf("contained live-fork network has no config cloner")
		}
		return s.liveForkNetwork.Clone(dstStack)
	}
	return nil
}

// ResetLiveForkChild implements inet.LiveForkStackChildResetter.
func (s *Stack) ResetLiveForkChild() error {
	if s.liveForkNetwork == nil || s.liveForkNetwork.ResetChild == nil {
		return nil
	}
	return s.liveForkNetwork.ResetChild(s)
}

// GetRemoveConf implements inet.Stack.GetRemoveConf.
func (s *Stack) GetRemoveConf() bool {
	return s.Stack.GetRemoveConf()
}
