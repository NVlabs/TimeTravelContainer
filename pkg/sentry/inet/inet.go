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

// Package inet defines semantics for IP stacks.
package inet

import (
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/sentry/socket/netlink/nlmsg"
	"gvisor.dev/gvisor/pkg/syserr"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// Stack represents a TCP/IP stack.
type Stack interface {
	// Interfaces returns all network interfaces as a mapping from interface
	// indexes to interface properties. Interface indices are strictly positive
	// integers.
	Interfaces() map[int32]Interface

	// RemoveInterface removes the specified network interface.
	RemoveInterface(ctx context.Context, idx int32) error

	// InterfaceAddrs returns all network interface addresses as a mapping from
	// interface indexes to a slice of associated interface address properties.
	InterfaceAddrs() map[int32][]InterfaceAddr

	// InterfaceIDs returns all network interface indexes in a
	// deterministic order matching the interface registration order.
	// This matches Linux kernel behavior where interfaces are ordered
	// by their ifindex in the dev_index_head hash table.
	InterfaceIDs() []int32

	// AddInterfaceAddr adds an address to the network interface identified by
	// idx.
	AddInterfaceAddr(ctx context.Context, idx int32, addr InterfaceAddr) error

	// SetInterface modifies or adds a new interface.
	SetInterface(ctx context.Context, msg *nlmsg.Message) *syserr.Error

	// RemoveInterfaceAddr removes an address from the network interface
	// identified by idx.
	RemoveInterfaceAddr(ctx context.Context, idx int32, addr InterfaceAddr) error

	// SupportsIPv6 returns true if the stack supports IPv6 connectivity.
	SupportsIPv6() bool

	// TCPReceiveBufferSize returns TCP receive buffer size settings.
	TCPReceiveBufferSize() (TCPBufferSize, error)

	// SetTCPReceiveBufferSize attempts to change TCP receive buffer size
	// settings.
	SetTCPReceiveBufferSize(size TCPBufferSize) error

	// TCPSendBufferSize returns TCP send buffer size settings.
	TCPSendBufferSize() (TCPBufferSize, error)

	// SetTCPSendBufferSize attempts to change TCP send buffer size settings.
	SetTCPSendBufferSize(size TCPBufferSize) error

	// TCPSACKEnabled returns true if RFC 2018 TCP Selective Acknowledgements
	// are enabled.
	TCPSACKEnabled() (bool, error)

	// SetTCPSACKEnabled attempts to change TCP selective acknowledgement
	// settings.
	SetTCPSACKEnabled(enabled bool) error

	// TCPRecovery returns the TCP loss detection algorithm.
	TCPRecovery() (TCPLossRecovery, error)

	// SetTCPRecovery attempts to change TCP loss detection algorithm.
	SetTCPRecovery(recovery TCPLossRecovery) error

	// Statistics reports stack statistics.
	Statistics(stat any, arg string) error

	// RouteTable returns the network stack's route table.
	RouteTable() []Route

	// RemoveRoute deletes the specified route.
	RemoveRoute(ctx context.Context, msg *nlmsg.Message) *syserr.Error

	// NewRoute adds the given route to the network stack's route table.
	NewRoute(ctx context.Context, msg *nlmsg.Message) *syserr.Error

	// Pause pauses the network stack before save.
	Pause()

	// Resume resumes the network stack after save.
	Resume()

	// Restore restarts the network stack after restore.
	Restore()

	// ResetConfig resets the stack's NICs and configuration.
	ResetConfig()

	// Destroy the network stack.
	Destroy()

	// RegisteredEndpoints returns all endpoints which are currently registered.
	RegisteredEndpoints() []stack.TransportEndpoint

	// CleanupEndpoints returns endpoints currently in the cleanup state.
	CleanupEndpoints() []stack.TransportEndpoint

	// RestoreCleanupEndpoints adds endpoints to cleanup tracking. This is useful
	// for restoring a stack after a save.
	RestoreCleanupEndpoints([]stack.TransportEndpoint)

	// SetForwarding enables or disables packet forwarding between NICs.
	SetForwarding(protocol tcpip.NetworkProtocolNumber, enable bool) error

	// GetAllowExternalLoopbackTraffic returns whether acceptance of martian
	// loopback packets (Linux net.ipv4.conf.*.route_localnet) is enabled for the
	// protocol.
	GetAllowExternalLoopbackTraffic(protocol tcpip.NetworkProtocolNumber) (bool, error)

	// SetAllowExternalLoopbackTraffic enables or disables acceptance of martian
	// loopback packets (Linux net.ipv4.conf.*.route_localnet) for the protocol.
	SetAllowExternalLoopbackTraffic(protocol tcpip.NetworkProtocolNumber, enable bool) error

	// PortRange returns the UDP and TCP inclusive range of ephemeral ports
	// used in both IPv4 and IPv6.
	PortRange() (uint16, uint16)

	// SetPortRange sets the UDP and TCP IPv4 and IPv6 ephemeral port range
	// (inclusive).
	SetPortRange(start uint16, end uint16) error

	// Stats returns the network stats.
	Stats() tcpip.Stats

	// SetRemoveConf sets removeConf in stack to the given value.
	SetRemoveConf(bool)

	// GetRemoveConf returns the removeConf.
	GetRemoveConf() bool
}

// LiveForkStackValidator may be implemented by network stacks that can prove
// their complete link graph is contained within one Sentry.
type LiveForkStackValidator interface {
	// ValidateLiveFork returns nil only if cloning this stack cannot retain or
	// communicate through an external link endpoint.
	ValidateLiveFork() error
}

// LiveForkStackConfigCloner reconstructs intentionally unsaved network
// configuration in a cloned stack. Implementations must create new link and
// NIC objects rather than sharing them with the source stack.
type LiveForkStackConfigCloner interface {
	CloneLiveForkConfig(dst Stack) error
}

// LiveForkStackConfigSaver controls serialization of guest-managed network
// configuration that ordinary gVisor checkpoints reconstruct externally.
type LiveForkStackConfigSaver interface {
	SetLiveForkSaveConfig(bool)
}

// LiveForkStackConfigRestorer identifies a stack whose guest-managed
// configuration was decoded from a TTC branch snapshot.
type LiveForkStackConfigRestorer interface {
	PrepareLiveForkRestore() error

	HasLiveForkRestoredConfig() bool
}

// LiveForkStackChildResetter removes branch-external transport state from a
// freshly cloned stack after its endpoints have been restored.
type LiveForkStackChildResetter interface {
	ResetLiveForkChild() error
}

// LiveForkQuarantinable is a stack that can be cut off from the outside world
// for the life of one child.
//
// A quarantined child is forked to be asked something and then destroyed. Its
// connections stay up as far as its own programs can tell — a licensed tool
// does not learn that it has been copied — and nothing it sends leaves the
// sandbox, so nothing outside learns either.
type LiveForkQuarantinable interface {
	SetLiveForkQuarantine(bool)
}

// LiveForkQuarantineController changes whether a runtime-managed network link
// is connected to its external proxy. The owning Kernel must be paused while
// this operation runs. Entering quarantine preserves guest socket state and
// sends packets into a black hole; leaving quarantine recreates the external
// link and resets flows whose peer-side history is no longer valid.
type LiveForkQuarantineController interface {
	SetLiveForkQuarantineState(bool) error
}

// LiveForkDanglingEndpointOwner identifies globally tracked transport
// endpoints that belong to one stack. The ordinary checkpoint path assumes one
// Kernel per Sentry, but live fork keeps multiple Kernels in one process and
// must not serialize a sibling branch's dangling endpoints.
type LiveForkDanglingEndpointOwner interface {
	OwnsLiveForkDanglingEndpoint(tcpip.Endpoint) bool
}

// Interface contains information about a network interface.
type Interface struct {
	// DeviceType is the device type, a Linux ARPHRD_* constant.
	DeviceType uint16

	// Flags is the device flags; see netdevice(7), under "Ioctls",
	// "SIOCGIFFLAGS, SIOCSIFFLAGS".
	Flags uint32

	// Name is the device name.
	Name string

	// Addr is the hardware device address.
	Addr []byte

	// MTU is the maximum transmission unit.
	MTU uint32

	// Features are the device features queried from the host at
	// stack creation time. These are immutable after startup.
	Features []linux.EthtoolGetFeaturesBlock

	// Master is the index of the main controlling interface in a bonded setup.
	Master uint32

	// Kind is the link kind (e.g. "veth", "bridge").
	Kind string
}

// InterfaceAddr contains information about a network interface address.
type InterfaceAddr struct {
	// Family is the address family, a Linux AF_* constant.
	Family uint8

	// PrefixLen is the address prefix length.
	PrefixLen uint8

	// Flags is the address flags.
	Flags uint8

	// Addr is the actual address.
	Addr []byte
}

// TCPBufferSize contains settings controlling TCP buffer sizing.
//
// +stateify savable
type TCPBufferSize struct {
	// Min is the minimum size.
	Min int

	// Default is the default size.
	Default int

	// Max is the maximum size.
	Max int
}

// StatDev describes one line of /proc/net/dev, i.e., stats for one network
// interface.
type StatDev [16]uint64

// Route contains information about a network route.
type Route struct {
	// Family is the address family, a Linux AF_* constant.
	Family uint8

	// DstLen is the length of the destination address.
	DstLen uint8

	// SrcLen is the length of the source address.
	SrcLen uint8

	// TOS is the Type of Service filter.
	TOS uint8

	// Table is the routing table ID.
	Table uint8

	// Protocol is the route origin, a Linux RTPROT_* constant.
	Protocol uint8

	// Scope is the distance to destination, a Linux RT_SCOPE_* constant.
	Scope uint8

	// Type is the route origin, a Linux RTN_* constant.
	Type uint8

	// Flags are route flags. See rtnetlink(7) under "rtm_flags".
	Flags uint32

	// DstAddr is the route destination address (RTA_DST).
	DstAddr []byte

	// SrcAddr is the route source address (RTA_SRC).
	SrcAddr []byte

	// OutputInterface is the output interface index (RTA_OIF).
	OutputInterface int32

	// GatewayAddr is the route gateway address (RTA_GATEWAY).
	GatewayAddr []byte
}

// Below SNMP metrics are from Linux/usr/include/linux/snmp.h.

// StatSNMPIP describes Ip line of /proc/net/snmp.
type StatSNMPIP [19]uint64

// StatSNMPICMP describes Icmp line of /proc/net/snmp.
type StatSNMPICMP [27]uint64

// StatSNMPICMPMSG describes IcmpMsg line of /proc/net/snmp.
type StatSNMPICMPMSG [512]uint64

// StatSNMPTCP describes Tcp line of /proc/net/snmp.
type StatSNMPTCP [15]uint64

// StatSNMPUDP describes Udp line of /proc/net/snmp.
type StatSNMPUDP [8]uint64

// StatSNMPUDPLite describes UdpLite line of /proc/net/snmp.
type StatSNMPUDPLite [8]uint64

// TCPLossRecovery indicates TCP loss detection and recovery methods to use.
type TCPLossRecovery int32

// Loss recovery constants from include/net/tcp.h which are used to set
// /proc/sys/net/ipv4/tcp_recovery.
const (
	TCP_RACK_LOSS_DETECTION TCPLossRecovery = 1 << iota
	TCP_RACK_STATIC_REO_WND
	TCP_RACK_NO_DUPTHRESH
)

// InterfaceRequest contains information about an adding interface.
type InterfaceRequest struct {
	// Kind is the link type.
	Kind string
	// Name is the interface name.
	Name string
	// Addr is the hardware device address.
	Addr []byte
	// MTU is the maximum transmission unit.
	MTU uint32
	// Data is link type specific device properties.
	Data any
}

// VethPeerReq contains information about a second interface of a new veth pair.
type VethPeerReq struct {
	// Req is information about the second end of the new veth pair.
	Req InterfaceRequest
	// Stack is the stack where the second end has to be added.
	Stack Stack
}

// NetworkArgs configures a network stack directly.
type NetworkArgs interface {
	ConfigureNetwork(s Stack) error
}
