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

package sandbox

import (
	"fmt"
	"net"

	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/urpc"
	"gvisor.dev/gvisor/runsc/boot/sentryapi"
	"gvisor.dev/gvisor/runsc/config"
)

// setupNetwork supports only network configurations that remain fully inside
// Sentry. Native macOS never imports host interfaces or host routes.
func setupNetwork(conn *urpc.Client, _ int, conf *config.Config, _ bool, isRestore bool) error {
	switch conf.Network {
	case config.NetworkNone:
		return createDefaultLoopbackInterface(conf, conn, isRestore)
	case config.NetworkTimeTravel:
		guestIP := net.ParseIP(conf.TimeTravelNetworkIP)
		guestMAC, err := net.ParseMAC(conf.TimeTravelNetworkMAC)
		if conf.TimeTravelNetworkMAC == "" {
			guestMAC = nil
		} else if err != nil {
			return fmt.Errorf("parsing TTC virtual NIC MAC address: %w", err)
		}
		link := sentryapi.DefaultLoopbackLink
		link.GVisorGRO = conf.GVisorGRO
		if err := conn.Call(sentryapi.ContMgrSetNetworkArgs, &sentryapi.CreateLinksAndRoutesArgs{
			LoopbackLinks: []sentryapi.LoopbackLink{link},
			TimeTravel: &sentryapi.TimeTravelNetworkConfig{
				NAT: conf.TimeTravelNetworkNAT, GuestIP: guestIP, GuestMAC: guestMAC,
				InterfaceName: conf.TimeTravelNetworkInterface,
			},
			IsRestore: isRestore,
		}, nil); err != nil {
			return fmt.Errorf("creating TTC virtual network: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("network mode %q is unsupported on native macOS", conf.Network)
	}
}

func createDefaultLoopbackInterface(conf *config.Config, conn *urpc.Client, isRestore bool) error {
	log.Infof("Network is disabled; creating the Sentry loopback interface only")
	link := sentryapi.DefaultLoopbackLink
	link.GVisorGRO = conf.GVisorGRO
	if err := conn.Call(sentryapi.ContMgrSetNetworkArgs, &sentryapi.CreateLinksAndRoutesArgs{
		LoopbackLinks: []sentryapi.LoopbackLink{link},
		IsRestore:     isRestore,
	}, nil); err != nil {
		return fmt.Errorf("creating loopback link and routes: %w", err)
	}
	return nil
}
