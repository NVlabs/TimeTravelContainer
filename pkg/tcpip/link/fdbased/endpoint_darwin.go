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

//go:build darwin

// Package fdbased provides host-FD netstack links.
package fdbased

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// PacketDispatchMode identifies a host packet receive implementation.
type PacketDispatchMode int

const (
	Readv PacketDispatchMode = iota
	RecvMMsg
	PacketMMap

	BatchSize      = 47
	MaxMsgsPerRecv = 8
)

func (p PacketDispatchMode) String() string {
	switch p {
	case Readv:
		return "Readv"
	case RecvMMsg:
		return "RecvMMsg"
	case PacketMMap:
		return "PacketMMap"
	default:
		return fmt.Sprintf("unknown packet dispatch mode '%d'", p)
	}
}

// Options mirrors the Linux endpoint API so common boot configuration can be
// decoded on Darwin. The initial macOS product uses userspace netstack/NAT and
// never creates one of these host-backed links.
type Options struct {
	FDs                   []int
	MTU                   uint32
	EthernetHeader        bool
	ClosedFunc            func(tcpip.Error)
	Address               tcpip.LinkAddress
	SaveRestore           bool
	GSOMaxSize            uint32
	GVisorGSOEnabled      bool
	PacketDispatchMode    PacketDispatchMode
	TXChecksumOffload     bool
	RXChecksumOffload     bool
	MaxSyscallHeaderBytes int
	InterfaceIndex        int
	GRO                   bool
	ProcessorsPerChannel  int
	IsPacketSocket        []bool
	PreConfigured         bool
}

var errUnavailable = errors.New("host FD packet links are unavailable on Darwin; use userspace netstack networking")

// New rejects host-backed packet links on Darwin.
func New(_ *Options) (stack.LinkEndpoint, error) {
	return nil, errUnavailable
}

// IsSocketFD checks whether fd refers to a socket.
func IsSocketFD(fd int) (bool, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return false, fmt.Errorf("unix.Fstat(%v, ...): %w", fd, err)
	}
	return stat.Mode&unix.S_IFSOCK == unix.S_IFSOCK, nil
}

// IsPacketSocket is always false because Darwin has no Linux AF_PACKET.
func IsPacketSocket(_ int, _ bool) (bool, error) {
	return false, nil
}

// CreatePacketFanoutGroup rejects Linux PACKET_FANOUT on Darwin.
func CreatePacketFanoutGroup(_ int) (int32, error) {
	return 0, errUnavailable
}

// JoinPacketFanoutGroup rejects Linux PACKET_FANOUT on Darwin.
func JoinPacketFanoutGroup(_ int, _ int32) error {
	return errUnavailable
}
