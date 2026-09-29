// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

// Package xdp provides the host-specific XDP link endpoint.
package xdp

import (
	"errors"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// MTU is retained for configuration compatibility. Darwin does not expose
// AF_XDP and trusted-mode macOS runtimes use userspace netstack/NAT instead.
const MTU = 1500

// Options specifies an XDP endpoint. It mirrors the Linux API so generic boot
// configuration can be compiled on every supported host.
type Options struct {
	FD                int
	ClosedFunc        func(tcpip.Error)
	Address           tcpip.LinkAddress
	SaveRestore       bool
	TXChecksumOffload bool
	RXChecksumOffload bool
	InterfaceIndex    int
	Bind              bool
	GRO               bool
	QueueID           uint32
}

// New rejects XDP on Darwin. Configuration validation normally prevents this
// path from being reached.
func New(_ *Options) (stack.LinkEndpoint, error) {
	return nil, errors.New("AF_XDP is unavailable on Darwin; use userspace netstack networking")
}
