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

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ebpf

import (
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
)

// CgroupAttachType is an attachment type that is valid for a cgroup eBPF program.
type CgroupAttachType uint

// Subset of attachment types that are valid for cgroup eBPF programs.
const (
	CGROUP_INET_INGRESS CgroupAttachType = iota
	CGROUP_INET_EGRESS
	CGROUP_INET_SOCK_CREATE
	CGROUP_SOCK_OPS
	CGROUP_DEVICE
	CGROUP_INET4_BIND
	CGROUP_INET6_BIND
	CGROUP_INET4_CONNECT
	CGROUP_INET6_CONNECT
	CGROUP_UNIX_CONNECT
	CGROUP_INET4_POST_BIND
	CGROUP_INET6_POST_BIND
	CGROUP_UDP4_SENDMSG
	CGROUP_UDP6_SENDMSG
	CGROUP_UNIX_SENDMSG
	CGROUP_SYSCTL
	CGROUP_UDP4_RECVMSG
	CGROUP_UDP6_RECVMSG
	CGROUP_UNIX_RECVMSG
	CGROUP_GETSOCKOPT
	CGROUP_SETSOCKOPT
	CGROUP_INET4_GETPEERNAME
	CGROUP_INET6_GETPEERNAME
	CGROUP_UNIX_GETPEERNAME
	CGROUP_INET4_GETSOCKNAME
	CGROUP_INET6_GETSOCKNAME
	CGROUP_UNIX_GETSOCKNAME
	CGROUP_INET_SOCK_RELEASE
	MAX_CGROUP_BPF_ATTACH_TYPE uint = iota
)

// AttachType is an interface implemented by each subsystem's subset of valid
// attachment types.
type AttachType interface {
	// MatchingProgType returns the program type that expects to be attached
	// at this attachment type.
	//
	// Analogous to kernel/bpf/syscall.c:attach_type_to_prog_type() in Linux.
	MatchingProgType() linux.BPFProgramType
}

// MatchingProgType implements AttachType.MatchingProgType.
func (c CgroupAttachType) MatchingProgType() linux.BPFProgramType {
	switch c {
	case CGROUP_INET_INGRESS, CGROUP_INET_EGRESS:
		return linux.BPF_PROG_TYPE_CGROUP_SKB
	case CGROUP_INET_SOCK_CREATE, CGROUP_INET_SOCK_RELEASE, CGROUP_INET4_POST_BIND, CGROUP_INET6_POST_BIND:
		return linux.BPF_PROG_TYPE_CGROUP_SOCK
	case CGROUP_INET4_BIND, CGROUP_INET6_BIND, CGROUP_INET4_CONNECT, CGROUP_INET6_CONNECT, CGROUP_UNIX_CONNECT, CGROUP_INET4_GETPEERNAME, CGROUP_INET6_GETPEERNAME, CGROUP_UNIX_GETPEERNAME, CGROUP_INET4_GETSOCKNAME, CGROUP_INET6_GETSOCKNAME, CGROUP_UNIX_GETSOCKNAME, CGROUP_UDP4_SENDMSG, CGROUP_UDP6_SENDMSG, CGROUP_UNIX_SENDMSG, CGROUP_UDP4_RECVMSG, CGROUP_UDP6_RECVMSG, CGROUP_UNIX_RECVMSG:
		return linux.BPF_PROG_TYPE_CGROUP_SOCK_ADDR
	case CGROUP_SOCK_OPS:
		return linux.BPF_PROG_TYPE_SOCK_OPS
	case CGROUP_DEVICE:
		return linux.BPF_PROG_TYPE_CGROUP_DEVICE
	case CGROUP_SYSCTL:
		return linux.BPF_PROG_TYPE_CGROUP_SYSCTL
	case CGROUP_GETSOCKOPT, CGROUP_SETSOCKOPT:
		return linux.BPF_PROG_TYPE_CGROUP_SOCKOPT
	default:
		return linux.BPF_PROG_TYPE_UNSPEC
	}
}

// ParseAttachmentType takes a raw linux.BPFAttachType provided by userspace and converts
// it an appropriately-typed BPFAttachType object depending on which subsystem it is to be
// attached to.
//
// ParseAttachmentType returns nil if the attachment type is unknown or unsupported.
func ParseAttachmentType(b linux.BPFAttachType) AttachType {
	switch b {
	case linux.BPF_CGROUP_INET_INGRESS:
		return CGROUP_INET_INGRESS
	case linux.BPF_CGROUP_INET_EGRESS:
		return CGROUP_INET_EGRESS
	case linux.BPF_CGROUP_INET_SOCK_CREATE:
		return CGROUP_INET_SOCK_CREATE
	case linux.BPF_CGROUP_SOCK_OPS:
		return CGROUP_SOCK_OPS
	case linux.BPF_CGROUP_DEVICE:
		return CGROUP_DEVICE
	case linux.BPF_CGROUP_INET4_BIND:
		return CGROUP_INET4_BIND
	case linux.BPF_CGROUP_INET6_BIND:
		return CGROUP_INET6_BIND
	case linux.BPF_CGROUP_INET4_CONNECT:
		return CGROUP_INET4_CONNECT
	case linux.BPF_CGROUP_INET6_CONNECT:
		return CGROUP_INET6_CONNECT
	case linux.BPF_CGROUP_UNIX_CONNECT:
		return CGROUP_UNIX_CONNECT
	case linux.BPF_CGROUP_INET4_POST_BIND:
		return CGROUP_INET4_POST_BIND
	case linux.BPF_CGROUP_INET6_POST_BIND:
		return CGROUP_INET6_POST_BIND
	case linux.BPF_CGROUP_UDP4_SENDMSG:
		return CGROUP_UDP4_SENDMSG
	case linux.BPF_CGROUP_UDP6_SENDMSG:
		return CGROUP_UDP6_SENDMSG
	case linux.BPF_CGROUP_UNIX_SENDMSG:
		return CGROUP_UNIX_SENDMSG
	case linux.BPF_CGROUP_SYSCTL:
		return CGROUP_SYSCTL
	case linux.BPF_CGROUP_UDP4_RECVMSG:
		return CGROUP_UDP4_RECVMSG
	case linux.BPF_CGROUP_UDP6_RECVMSG:
		return CGROUP_UDP6_RECVMSG
	case linux.BPF_CGROUP_UNIX_RECVMSG:
		return CGROUP_UNIX_RECVMSG
	case linux.BPF_CGROUP_GETSOCKOPT:
		return CGROUP_GETSOCKOPT
	case linux.BPF_CGROUP_SETSOCKOPT:
		return CGROUP_SETSOCKOPT
	case linux.BPF_CGROUP_INET4_GETPEERNAME:
		return CGROUP_INET4_GETPEERNAME
	case linux.BPF_CGROUP_INET6_GETPEERNAME:
		return CGROUP_INET6_GETPEERNAME
	case linux.BPF_CGROUP_UNIX_GETPEERNAME:
		return CGROUP_UNIX_GETPEERNAME
	case linux.BPF_CGROUP_INET4_GETSOCKNAME:
		return CGROUP_INET4_GETSOCKNAME
	case linux.BPF_CGROUP_INET6_GETSOCKNAME:
		return CGROUP_INET6_GETSOCKNAME
	case linux.BPF_CGROUP_UNIX_GETSOCKNAME:
		return CGROUP_UNIX_GETSOCKNAME
	case linux.BPF_CGROUP_INET_SOCK_RELEASE:
		return CGROUP_INET_SOCK_RELEASE
	}

	return nil
}

// SupportedProgType returns whether programs of type t may be loaded. They
// never run. Programs that attach to BTF types, or to other programs, are
// not supported.
func SupportedProgType(t linux.BPFProgramType) bool {
	switch t {
	case linux.BPF_PROG_TYPE_UNSPEC, linux.BPF_PROG_TYPE_TRACING, linux.BPF_PROG_TYPE_STRUCT_OPS,
		linux.BPF_PROG_TYPE_EXT, linux.BPF_PROG_TYPE_LSM, linux.BPF_PROG_TYPE_SYSCALL:
		return false
	}
	return t <= linux.BPF_PROG_TYPE_NETFILTER
}

// IsNetAdminProgType returns whether loading programs of type t needs
// CAP_NET_ADMIN, as Linux's is_net_admin_prog_type does.
func IsNetAdminProgType(t linux.BPFProgramType) bool {
	switch t {
	case linux.BPF_PROG_TYPE_SCHED_CLS, linux.BPF_PROG_TYPE_SCHED_ACT, linux.BPF_PROG_TYPE_XDP,
		linux.BPF_PROG_TYPE_LWT_IN, linux.BPF_PROG_TYPE_LWT_OUT, linux.BPF_PROG_TYPE_LWT_XMIT,
		linux.BPF_PROG_TYPE_LWT_SEG6LOCAL, linux.BPF_PROG_TYPE_SK_SKB, linux.BPF_PROG_TYPE_SK_MSG,
		linux.BPF_PROG_TYPE_FLOW_DISSECTOR, linux.BPF_PROG_TYPE_CGROUP_DEVICE,
		linux.BPF_PROG_TYPE_CGROUP_SOCK, linux.BPF_PROG_TYPE_CGROUP_SOCK_ADDR,
		linux.BPF_PROG_TYPE_CGROUP_SOCKOPT, linux.BPF_PROG_TYPE_CGROUP_SYSCTL,
		linux.BPF_PROG_TYPE_SOCK_OPS, linux.BPF_PROG_TYPE_EXT, linux.BPF_PROG_TYPE_NETFILTER:
		return true
	}
	return false
}

// IsPerfmonProgType returns whether loading programs of type t needs
// CAP_PERFMON, as Linux's is_perfmon_prog_type does.
func IsPerfmonProgType(t linux.BPFProgramType) bool {
	switch t {
	case linux.BPF_PROG_TYPE_KPROBE, linux.BPF_PROG_TYPE_TRACEPOINT, linux.BPF_PROG_TYPE_PERF_EVENT,
		linux.BPF_PROG_TYPE_RAW_TRACEPOINT, linux.BPF_PROG_TYPE_RAW_TRACEPOINT_WRITABLE,
		linux.BPF_PROG_TYPE_TRACING, linux.BPF_PROG_TYPE_LSM, linux.BPF_PROG_TYPE_STRUCT_OPS,
		linux.BPF_PROG_TYPE_EXT:
		return true
	}
	return false
}

// FixupExpectedAttachType returns the attachment type that a program of
// type t, loaded with expected attachment type a, expects, as Linux's
// bpf_prog_load_fixup_attach_type does: 0, which is BPF_CGROUP_INET_INGRESS,
// also means none.
func FixupExpectedAttachType(t linux.BPFProgramType, a linux.BPFAttachType) linux.BPFAttachType {
	if a != 0 {
		return a
	}
	switch t {
	case linux.BPF_PROG_TYPE_CGROUP_SOCK:
		return linux.BPF_CGROUP_INET_SOCK_CREATE
	case linux.BPF_PROG_TYPE_SK_REUSEPORT:
		return linux.BPF_SK_REUSEPORT_SELECT
	}
	return a
}

// CheckExpectedAttachType checks that programs of type t may expect to
// attach as a, as Linux's bpf_prog_load_check_attach does.
func CheckExpectedAttachType(t linux.BPFProgramType, a linux.BPFAttachType) error {
	var ok bool
	switch t {
	case linux.BPF_PROG_TYPE_CGROUP_SOCK:
		switch a {
		case linux.BPF_CGROUP_INET_SOCK_CREATE, linux.BPF_CGROUP_INET_SOCK_RELEASE,
			linux.BPF_CGROUP_INET4_POST_BIND, linux.BPF_CGROUP_INET6_POST_BIND:
			ok = true
		}
	case linux.BPF_PROG_TYPE_CGROUP_SOCK_ADDR:
		switch a {
		case linux.BPF_CGROUP_INET4_BIND, linux.BPF_CGROUP_INET6_BIND,
			linux.BPF_CGROUP_INET4_CONNECT, linux.BPF_CGROUP_INET6_CONNECT, linux.BPF_CGROUP_UNIX_CONNECT,
			linux.BPF_CGROUP_INET4_GETPEERNAME, linux.BPF_CGROUP_INET6_GETPEERNAME, linux.BPF_CGROUP_UNIX_GETPEERNAME,
			linux.BPF_CGROUP_INET4_GETSOCKNAME, linux.BPF_CGROUP_INET6_GETSOCKNAME, linux.BPF_CGROUP_UNIX_GETSOCKNAME,
			linux.BPF_CGROUP_UDP4_SENDMSG, linux.BPF_CGROUP_UDP6_SENDMSG, linux.BPF_CGROUP_UNIX_SENDMSG,
			linux.BPF_CGROUP_UDP4_RECVMSG, linux.BPF_CGROUP_UDP6_RECVMSG, linux.BPF_CGROUP_UNIX_RECVMSG:
			ok = true
		}
	case linux.BPF_PROG_TYPE_CGROUP_SKB:
		ok = a == linux.BPF_CGROUP_INET_INGRESS || a == linux.BPF_CGROUP_INET_EGRESS
	case linux.BPF_PROG_TYPE_CGROUP_SOCKOPT:
		ok = a == linux.BPF_CGROUP_SETSOCKOPT || a == linux.BPF_CGROUP_GETSOCKOPT
	case linux.BPF_PROG_TYPE_SK_LOOKUP:
		ok = a == linux.BPF_SK_LOOKUP
	case linux.BPF_PROG_TYPE_SK_REUSEPORT:
		ok = a == linux.BPF_SK_REUSEPORT_SELECT || a == linux.BPF_SK_REUSEPORT_SELECT_OR_MIGRATE
	case linux.BPF_PROG_TYPE_NETFILTER:
		ok = a == linux.BPF_NETFILTER
	default:
		ok = true
	}
	if !ok {
		return linuxerr.EINVAL
	}
	return nil
}
