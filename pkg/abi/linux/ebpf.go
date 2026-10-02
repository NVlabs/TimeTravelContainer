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

package linux

import (
	"structs"

	"gvisor.dev/gvisor/pkg/marshal"
)

// EBPFInstruction is the userspace representation of an eBPF instruction that has not
// been validated.
//
// +marshal slice:EBPFInstructionSlice
// +stateify savable
type EBPFInstruction struct {
	_ structs.HostLayout

	Code      uint8
	Registers uint8 // LE: 4 LSBs are destination, 4 MSBs are source
	Offset    int16
	Immediate int32
}

// Constants defining eBPF-related limits.
const (
	// Maximum instruction count in an eBPF program
	BPF_COMPLEXITY_LIMIT_INSNS = 1_000_000

	// Maximum length of an eBPF program's name
	BPF_OBJ_NAME_LEN = 16

	// Size of an EbpfInstruction
	BPF_INSTRUCTION_SIZE = 8

	// Maximum number of cgroup eBPF programs per attachment type.
	BPF_CGROUP_MAX_PROGS = 64
)

// Valid values for `cmd` for bpf(2).
const (
	BPF_MAP_CREATE = iota
	BPF_MAP_LOOKUP_ELEM
	BPF_MAP_UPDATE_ELEM
	BPF_MAP_DELETE_ELEM
	BPF_MAP_GET_NEXT_KEY
	BPF_PROG_LOAD
	BPF_OBJ_PIN
	BPF_OBJ_GET
	BPF_PROG_ATTACH
	BPF_PROG_DETACH
	BPF_PROG_TEST_RUN
	BPF_PROG_GET_NEXT_ID
	BPF_MAP_GET_NEXT_ID
	BPF_PROG_GET_FD_BY_ID
	BPF_MAP_GET_FD_BY_ID
	BPF_OBJ_GET_INFO_BY_FD
	BPF_PROG_QUERY
	BPF_RAW_TRACEPOINT_OPEN
	BPF_BTF_LOAD
	BPF_BTF_GET_FD_BY_ID
	BPF_TASK_FD_QUERY
	BPF_MAP_LOOKUP_AND_DELETE_ELEM
	BPF_MAP_FREEZE
	BPF_BTF_GET_NEXT_ID
	BPF_MAP_LOOKUP_BATCH
	BPF_MAP_LOOKUP_AND_DELETE_BATCH
	BPF_MAP_UPDATE_BATCH
	BPF_MAP_DELETE_BATCH
	BPF_LINK_CREATE
	BPF_LINK_UPDATE
	BPF_LINK_GET_FD_BY_ID
	BPF_LINK_GET_NEXT_ID
	BPF_ENABLE_STATS
	BPF_ITER_CREATE
	BPF_LINK_DETACH
	BPF_PROG_BIND_MAP
	BPF_TOKEN_CREATE
	BPF_PROG_STREAM_READ_BY_FD
	BPF_PROG_ASSOC_STRUCT_OPS

	BPF_PROG_RUN = BPF_PROG_TEST_RUN
)

// BPFProgramType represents an type for an eBPF program.
type BPFProgramType uint

// Valid types of eBPF programs.
const (
	BPF_PROG_TYPE_UNSPEC BPFProgramType = iota
	BPF_PROG_TYPE_SOCKET_FILTER
	BPF_PROG_TYPE_KPROBE
	BPF_PROG_TYPE_SCHED_CLS
	BPF_PROG_TYPE_SCHED_ACT
	BPF_PROG_TYPE_TRACEPOINT
	BPF_PROG_TYPE_XDP
	BPF_PROG_TYPE_PERF_EVENT
	BPF_PROG_TYPE_CGROUP_SKB
	BPF_PROG_TYPE_CGROUP_SOCK
	BPF_PROG_TYPE_LWT_IN
	BPF_PROG_TYPE_LWT_OUT
	BPF_PROG_TYPE_LWT_XMIT
	BPF_PROG_TYPE_SOCK_OPS
	BPF_PROG_TYPE_SK_SKB
	BPF_PROG_TYPE_CGROUP_DEVICE
	BPF_PROG_TYPE_SK_MSG
	BPF_PROG_TYPE_RAW_TRACEPOINT
	BPF_PROG_TYPE_CGROUP_SOCK_ADDR
	BPF_PROG_TYPE_LWT_SEG6LOCAL
	BPF_PROG_TYPE_LIRC_MODE2
	BPF_PROG_TYPE_SK_REUSEPORT
	BPF_PROG_TYPE_FLOW_DISSECTOR
	BPF_PROG_TYPE_CGROUP_SYSCTL
	BPF_PROG_TYPE_RAW_TRACEPOINT_WRITABLE
	BPF_PROG_TYPE_CGROUP_SOCKOPT
	BPF_PROG_TYPE_TRACING
	BPF_PROG_TYPE_STRUCT_OPS
	BPF_PROG_TYPE_EXT
	BPF_PROG_TYPE_LSM
	BPF_PROG_TYPE_SK_LOOKUP
	BPF_PROG_TYPE_SYSCALL
	BPF_PROG_TYPE_NETFILTER
)

// BPFAttachType represents an attachment type for an eBPF program.
type BPFAttachType uint

// All valid attachment types for eBPF programs.
const (
	BPF_CGROUP_INET_INGRESS BPFAttachType = iota
	BPF_CGROUP_INET_EGRESS
	BPF_CGROUP_INET_SOCK_CREATE
	BPF_CGROUP_SOCK_OPS
	BPF_SK_SKB_STREAM_PARSER
	BPF_SK_SKB_STREAM_VERDICT
	BPF_CGROUP_DEVICE
	BPF_SK_MSG_VERDICT
	BPF_CGROUP_INET4_BIND
	BPF_CGROUP_INET6_BIND
	BPF_CGROUP_INET4_CONNECT
	BPF_CGROUP_INET6_CONNECT
	BPF_CGROUP_INET4_POST_BIND
	BPF_CGROUP_INET6_POST_BIND
	BPF_CGROUP_UDP4_SENDMSG
	BPF_CGROUP_UDP6_SENDMSG
	BPF_LIRC_MODE2
	BPF_FLOW_DISSECTOR
	BPF_CGROUP_SYSCTL
	BPF_CGROUP_UDP4_RECVMSG
	BPF_CGROUP_UDP6_RECVMSG
	BPF_CGROUP_GETSOCKOPT
	BPF_CGROUP_SETSOCKOPT
	BPF_TRACE_RAW_TP
	BPF_TRACE_FENTRY
	BPF_TRACE_FEXIT
	BPF_MODIFY_RETURN
	BPF_LSM_MAC
	BPF_TRACE_ITER
	BPF_CGROUP_INET4_GETPEERNAME
	BPF_CGROUP_INET6_GETPEERNAME
	BPF_CGROUP_INET4_GETSOCKNAME
	BPF_CGROUP_INET6_GETSOCKNAME
	BPF_XDP_DEVMAP
	BPF_CGROUP_INET_SOCK_RELEASE
	BPF_XDP_CPUMAP
	BPF_SK_LOOKUP
	BPF_XDP
	BPF_SK_SKB_VERDICT
	BPF_SK_REUSEPORT_SELECT
	BPF_SK_REUSEPORT_SELECT_OR_MIGRATE
	BPF_PERF_EVENT
	BPF_TRACE_KPROBE_MULTI
	BPF_LSM_CGROUP
	BPF_STRUCT_OPS
	BPF_NETFILTER
	BPF_TCX_INGRESS
	BPF_TCX_EGRESS
	BPF_TRACE_UPROBE_MULTI
	BPF_CGROUP_UNIX_CONNECT
	BPF_CGROUP_UNIX_SENDMSG
	BPF_CGROUP_UNIX_RECVMSG
	BPF_CGROUP_UNIX_GETPEERNAME
	BPF_CGROUP_UNIX_GETSOCKNAME
	BPF_NETKIT_PRIMARY
	BPF_NETKIT_PEER
	BPF_TRACE_KPROBE_SESSION
	BPF_TRACE_UPROBE_SESSION
	BPF_TRACE_FSESSION
)

// BPFAttr represents the parameters to a bpf(2) call.
type BPFAttr interface {
	marshal.Marshallable

	implementsBPFAttr()
}

func (a *BPFAttrProgLoad) implementsBPFAttr()   {}
func (a *BPFAttrProgQuery) implementsBPFAttr()  {}
func (a *BPFAttrProgAttach) implementsBPFAttr() {}
func (a *BPFAttrMapCreate) implementsBPFAttr()  {}
func (a *BPFAttrMapElem) implementsBPFAttr()    {}
func (a *BPFAttrObj) implementsBPFAttr()        {}
func (a *BPFAttrObjGetInfo) implementsBPFAttr() {}

// BPFAttrProgLoad contains parameters for a BPF_PROG_LOAD command.
//
// +marshal
type BPFAttrProgLoad struct {
	_ structs.HostLayout

	ProgType           uint32
	InstructionCount   uint32
	Instructions       uint64
	License            uint64
	LogLevel           uint32
	LogSize            uint32
	LogBuf             uint64
	KernVersion        uint32
	ProgFlags          uint32
	ProgName           [BPF_OBJ_NAME_LEN]byte
	ProgInterfaceIndex uint32
	ExpectedAttachType uint32
	ProgBTFFD          uint32
	FuncInfoRecSize    uint32
	FuncInfo           uint64
	FuncInfoCount      uint32
	LineInfoRecSize    uint32
	LineInfo           uint64
	LineInfoCount      uint32
	AttachBTFID        uint32
	AttachFD           uint32 // union of either attach_prog_fd or attach_btf_obj_fd
	CoreReloCount      uint32
	FDArray            uint64
	CoreRelos          uint64
	CoreReloRecSize    uint32
	LogTrueSize        uint32
	ProgTokenFD        int32
	FDArrayCount       uint32
	Signature          uint64
	SignatureSize      uint32
	KeyringID          int32
}

// BPFAttrProgQuery contains parameters for a BPF_PROG_QUERY command.
//
// +marshal
type BPFAttrProgQuery struct {
	_ structs.HostLayout

	Target          uint32 // union of either target_fd or target_ifindex
	AttachType      uint32
	QueryFlags      uint32
	AttachFlags     uint32
	ProgIDs         uint64
	Count           uint32 // union of either prog_cnt or count
	_               uint32 // padding
	ProgAttachFlags uint64
	LinkIDs         uint64
	LinkAttachFlags uint64
	Revision        uint64
}

// BPFAttrProgAttach contains parameters for a BPF_PROG_ATTACH command.
//
// +marshal
type BPFAttrProgAttach struct {
	_ structs.HostLayout

	Target           uint32 // union of either target_fd or target_ifindex
	AttachBPFFD      uint32
	AttachType       uint32
	AttachFlags      uint32
	ReplaceBPFFD     uint32
	Relative         uint32 // union of either relative_fd or relative_id
	ExpectedRevision uint64
}

// BPF_ATTR_SIZE is the size of union bpf_attr, which is the largest of
// the sub-command attribute structures.
//
// BPF_ATTR_SIZE is immutable.
var BPF_ATTR_SIZE = max((*BPFAttrProgLoad)(nil).SizeBytes(), (*BPFAttrProgQuery)(nil).SizeBytes(), (*BPFAttrProgAttach)(nil).SizeBytes(),
	(*BPFAttrMapCreate)(nil).SizeBytes(), (*BPFAttrMapElem)(nil).SizeBytes(), (*BPFAttrObj)(nil).SizeBytes(), (*BPFAttrObjGetInfo)(nil).SizeBytes())

// BPFAttrMapCreate contains parameters for a BPF_MAP_CREATE command.
//
// +marshal
type BPFAttrMapCreate struct {
	_ structs.HostLayout

	MapType               uint32
	KeySize               uint32
	ValueSize             uint32
	MaxEntries            uint32
	MapFlags              uint32
	InnerMapFD            uint32
	NumaNode              uint32
	MapName               [BPF_OBJ_NAME_LEN]byte
	MapIfindex            uint32
	BTFFD                 uint32
	BTFKeyTypeID          uint32
	BTFValueTypeID        uint32
	BTFVmlinuxValueTypeID uint32
	MapExtra              uint64
	ValueTypeBTFObjFD     int32
	MapTokenFD            int32
}

// BPFAttrMapElem contains parameters for the BPF_MAP_*_ELEM commands, and
// BPF_MAP_FREEZE.
//
// +marshal
type BPFAttrMapElem struct {
	_ structs.HostLayout

	MapFD uint32
	_     uint32 // padding
	Key   uint64
	Value uint64 // union of either value or next_key
	Flags uint64
}

// BPFAttrObj contains parameters for the BPF_OBJ_PIN and BPF_OBJ_GET
// commands.
//
// +marshal
type BPFAttrObj struct {
	_ structs.HostLayout

	Pathname  uint64
	BPFFD     uint32
	FileFlags uint32
	PathFD    int32
	// Pad is the struct's padding, which is past its last field, and so,
	// as the rest of the union past it, must be zero.
	Pad uint32
}

// BPFAttrObjGetInfo contains parameters for a BPF_OBJ_GET_INFO_BY_FD
// command.
//
// +marshal
type BPFAttrObjGetInfo struct {
	_ structs.HostLayout

	BPFFD   uint32
	InfoLen uint32
	Info    uint64
}

// BPFMapInfo is struct bpf_map_info, which BPF_OBJ_GET_INFO_BY_FD returns
// for maps.
//
// +marshal
type BPFMapInfo struct {
	_ structs.HostLayout

	Type                  uint32
	ID                    uint32
	KeySize               uint32
	ValueSize             uint32
	MaxEntries            uint32
	MapFlags              uint32
	Name                  [BPF_OBJ_NAME_LEN]byte
	Ifindex               uint32
	BTFVmlinuxValueTypeID uint32
	NetnsDev              uint64
	NetnsIno              uint64
	BTFID                 uint32
	BTFKeyTypeID          uint32
	BTFValueTypeID        uint32
	BTFVmlinuxID          uint32
	MapExtra              uint64
}

// BPF_TAG_SIZE is the size of a program's tag.
const BPF_TAG_SIZE = 8

// BPFProgInfo is struct bpf_prog_info, which BPF_OBJ_GET_INFO_BY_FD returns
// for programs.
//
// +marshal
type BPFProgInfo struct {
	_ structs.HostLayout

	Type                 uint32
	ID                   uint32
	Tag                  [BPF_TAG_SIZE]byte
	JitedProgLen         uint32
	XlatedProgLen        uint32
	JitedProgInsns       uint64
	XlatedProgInsns      uint64
	LoadTime             uint64
	CreatedByUID         uint32
	NrMapIDs             uint32
	MapIDs               uint64
	Name                 [BPF_OBJ_NAME_LEN]byte
	Ifindex              uint32
	GPLCompatible        uint32 // bit 0; the rest are padding
	NetnsDev             uint64
	NetnsIno             uint64
	NrJitedKsyms         uint32
	NrJitedFuncLens      uint32
	JitedKsyms           uint64
	JitedFuncLens        uint64
	BTFID                uint32
	FuncInfoRecSize      uint32
	FuncInfo             uint64
	NrFuncInfo           uint32
	NrLineInfo           uint32
	LineInfo             uint64
	JitedLineInfo        uint64
	NrJitedLineInfo      uint32
	LineInfoRecSize      uint32
	JitedLineInfoRecSize uint32
	NrProgTags           uint32
	ProgTags             uint64
	RunTimeNs            uint64
	RunCnt               uint64
	RecursionMisses      uint64
	VerifiedInsns        uint32
	AttachBTFObjID       uint32
	AttachBTFID          uint32
	_                    uint32 // padding
}

// BPFMapType is enum bpf_map_type, the type of an eBPF map.
type BPFMapType uint32

// Types of eBPF maps.
const (
	BPF_MAP_TYPE_UNSPEC BPFMapType = iota
	BPF_MAP_TYPE_HASH
	BPF_MAP_TYPE_ARRAY
	BPF_MAP_TYPE_PROG_ARRAY
	BPF_MAP_TYPE_PERF_EVENT_ARRAY
	BPF_MAP_TYPE_PERCPU_HASH
	BPF_MAP_TYPE_PERCPU_ARRAY
	BPF_MAP_TYPE_STACK_TRACE
	BPF_MAP_TYPE_CGROUP_ARRAY
	BPF_MAP_TYPE_LRU_HASH
	BPF_MAP_TYPE_LRU_PERCPU_HASH
	BPF_MAP_TYPE_LPM_TRIE
	BPF_MAP_TYPE_ARRAY_OF_MAPS
	BPF_MAP_TYPE_HASH_OF_MAPS
	BPF_MAP_TYPE_DEVMAP
	BPF_MAP_TYPE_SOCKMAP
	BPF_MAP_TYPE_CPUMAP
	BPF_MAP_TYPE_XSKMAP
	BPF_MAP_TYPE_SOCKHASH
	BPF_MAP_TYPE_CGROUP_STORAGE
	BPF_MAP_TYPE_REUSEPORT_SOCKARRAY
	BPF_MAP_TYPE_PERCPU_CGROUP_STORAGE
	BPF_MAP_TYPE_QUEUE
	BPF_MAP_TYPE_STACK
	BPF_MAP_TYPE_SK_STORAGE
	BPF_MAP_TYPE_DEVMAP_HASH
	BPF_MAP_TYPE_STRUCT_OPS
	BPF_MAP_TYPE_RINGBUF
	BPF_MAP_TYPE_INODE_STORAGE
	BPF_MAP_TYPE_TASK_STORAGE
	BPF_MAP_TYPE_BLOOM_FILTER
	BPF_MAP_TYPE_USER_RINGBUF
	BPF_MAP_TYPE_CGRP_STORAGE
	BPF_MAP_TYPE_ARENA
)

// Flags of BPF_MAP_CREATE, and of BPF_OBJ_GET.
const (
	BPF_F_NO_PREALLOC      = 1 << 0
	BPF_F_NO_COMMON_LRU    = 1 << 1
	BPF_F_NUMA_NODE        = 1 << 2
	BPF_F_RDONLY           = 1 << 3
	BPF_F_WRONLY           = 1 << 4
	BPF_F_STACK_BUILD_ID   = 1 << 5
	BPF_F_ZERO_SEED        = 1 << 6
	BPF_F_RDONLY_PROG      = 1 << 7
	BPF_F_WRONLY_PROG      = 1 << 8
	BPF_F_CLONE            = 1 << 9
	BPF_F_MMAPABLE         = 1 << 10
	BPF_F_PRESERVE_ELEMS   = 1 << 11
	BPF_F_INNER_MAP        = 1 << 12
	BPF_F_PATH_FD          = 1 << 14
	BPF_F_VTYPE_BTF_OBJ_FD = 1 << 15
	BPF_F_TOKEN_FD         = 1 << 16
)

// Flags of BPF_MAP_UPDATE_ELEM.
const (
	BPF_ANY     = 0
	BPF_NOEXIST = 1
	BPF_EXIST   = 2
	BPF_F_LOCK  = 4
)

// BPF_PSEUDO_MAP_FD and BPF_PSEUDO_MAP_VALUE are values of the source
// register of a BPF_LD_IMM64 instruction whose immediate is a map's file
// descriptor.
const (
	BPF_PSEUDO_MAP_FD    = 1
	BPF_PSEUDO_MAP_VALUE = 2
)

// BPF_MAXINSNS is the most instructions that a program of an unprivileged
// user may have.
const BPF_MAXINSNS = 4096

// eBPF-related flags
const (
	BPF_F_ALLOW_OVERRIDE = 1 << iota
	BPF_F_ALLOW_MULTI
	BPF_F_REPLACE
	BPF_F_BEFORE
	BPF_F_AFTER
	BPF_F_ID
	BPF_F_PREORDER
	BPF_F_LINK = 1 << 13
)
