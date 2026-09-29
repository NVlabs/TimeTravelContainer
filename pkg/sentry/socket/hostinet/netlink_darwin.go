// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package hostinet

import (
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/sentry/inet"
	"gvisor.dev/gvisor/pkg/sentry/socket/netlink/nlmsg"
)

func getInterfaces() (map[int32]inet.Interface, error) { return nil, unix.EOPNOTSUPP }

func getInterfaceAddrs() (map[int32][]inet.InterfaceAddr, error) { return nil, unix.EOPNOTSUPP }

func getRoutes() ([]inet.Route, error) { return nil, unix.EOPNOTSUPP }

func removeInterface(int32) error { return unix.EOPNOTSUPP }

func addInterfaceAddr(int32, inet.InterfaceAddr) error { return unix.EOPNOTSUPP }

func removeInterfaceAddr(int32, inet.InterfaceAddr) error { return unix.EOPNOTSUPP }

func doRouteRequest(uint16, *nlmsg.Message) error { return unix.EOPNOTSUPP }
