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
