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
