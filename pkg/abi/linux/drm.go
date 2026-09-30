// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package linux

import (
	"structs"
)

// DRM, the Direct Rendering Manager: include/uapi/drm/drm.h.
const (
	// DRM_MAJOR is the device major number of DRM nodes.
	DRM_MAJOR = 226
	// DRM_RENDER_MINOR_BASE is the minor number of the first render node,
	// /dev/dri/renderD128.
	DRM_RENDER_MINOR_BASE = 128

	// DRM_IOCTL_BASE is the ioctl type of DRM ioctls.
	DRM_IOCTL_BASE = 'd'
	// DRM_COMMAND_BASE is the first ioctl number of a driver's own ioctls.
	DRM_COMMAND_BASE = 0x40

	// Ioctl numbers of the DRM core, as IOC_NR gives them.
	DRM_NR_VERSION            = 0x00
	DRM_NR_GEM_CLOSE          = 0x09
	DRM_NR_GET_CAP            = 0x0c
	DRM_NR_SET_CLIENT_CAP     = 0x0d
	DRM_NR_PRIME_HANDLE_TO_FD = 0x2d
	DRM_NR_PRIME_FD_TO_HANDLE = 0x2e

	// DRM_CAP_* for DRM_IOCTL_GET_CAP.
	DRM_CAP_PRIME               = 0x5
	DRM_CAP_TIMESTAMP_MONOTONIC = 0x6
	DRM_CAP_SYNCOBJ             = 0x13
	DRM_CAP_SYNCOBJ_TIMELINE    = 0x14
	// DRM_PRIME_CAP_* are the bits of DRM_CAP_PRIME.
	DRM_PRIME_CAP_IMPORT = 0x1
	DRM_PRIME_CAP_EXPORT = 0x2

	// DRM_CLOEXEC and DRM_RDWR are the flags of DRM_IOCTL_PRIME_HANDLE_TO_FD.
	DRM_CLOEXEC = O_CLOEXEC
	DRM_RDWR    = O_RDWR
)

// DRMVersion is struct drm_version.
//
// +marshal
type DRMVersion struct {
	_                 structs.HostLayout
	VersionMajor      int32
	VersionMinor      int32
	VersionPatchlevel int32
	_                 uint32
	NameLen           uint64
	Name              uint64
	DateLen           uint64
	Date              uint64
	DescLen           uint64
	Desc              uint64
}

// DRMGemClose is struct drm_gem_close.
//
// +marshal
type DRMGemClose struct {
	_      structs.HostLayout
	Handle uint32
	Pad    uint32
}

// DRMGetCap is struct drm_get_cap, and struct drm_set_client_cap.
//
// +marshal
type DRMGetCap struct {
	_          structs.HostLayout
	Capability uint64
	Value      uint64
}

// DRMPrimeHandle is struct drm_prime_handle.
//
// +marshal
type DRMPrimeHandle struct {
	_      structs.HostLayout
	Handle uint32
	Flags  uint32
	FD     int32
}

// virtio-gpu's DRM driver: include/uapi/drm/virtgpu_drm.h.
const (
	// DRM_VIRTGPU_* are the driver's ioctl numbers, relative to
	// DRM_COMMAND_BASE.
	DRM_VIRTGPU_MAP                  = 0x01
	DRM_VIRTGPU_EXECBUFFER           = 0x02
	DRM_VIRTGPU_GETPARAM             = 0x03
	DRM_VIRTGPU_RESOURCE_CREATE      = 0x04
	DRM_VIRTGPU_RESOURCE_INFO        = 0x05
	DRM_VIRTGPU_TRANSFER_FROM_HOST   = 0x06
	DRM_VIRTGPU_TRANSFER_TO_HOST     = 0x07
	DRM_VIRTGPU_WAIT                 = 0x08
	DRM_VIRTGPU_GET_CAPS             = 0x09
	DRM_VIRTGPU_RESOURCE_CREATE_BLOB = 0x0a
	DRM_VIRTGPU_CONTEXT_INIT         = 0x0b

	// VIRTGPU_EXECBUF_* are the flags of DRM_VIRTGPU_EXECBUFFER.
	VIRTGPU_EXECBUF_FENCE_FD_IN  = 0x01
	VIRTGPU_EXECBUF_FENCE_FD_OUT = 0x02
	VIRTGPU_EXECBUF_RING_IDX     = 0x04

	// VIRTGPU_PARAM_* are the parameters of DRM_VIRTGPU_GETPARAM.
	VIRTGPU_PARAM_3D_FEATURES          = 1
	VIRTGPU_PARAM_CAPSET_QUERY_FIX     = 2
	VIRTGPU_PARAM_RESOURCE_BLOB        = 3
	VIRTGPU_PARAM_HOST_VISIBLE         = 4
	VIRTGPU_PARAM_CROSS_DEVICE         = 5
	VIRTGPU_PARAM_CONTEXT_INIT         = 6
	VIRTGPU_PARAM_SUPPORTED_CAPSET_IDS = 7
	VIRTGPU_PARAM_EXPLICIT_DEBUG_NAME  = 8

	// VIRTGPU_DRM_CAPSET_* are the capability sets, and context types.
	VIRTGPU_DRM_CAPSET_VIRGL            = 1
	VIRTGPU_DRM_CAPSET_VIRGL2           = 2
	VIRTGPU_DRM_CAPSET_GFXSTREAM_VULKAN = 3
	VIRTGPU_DRM_CAPSET_VENUS            = 4
	VIRTGPU_DRM_CAPSET_CROSS_DOMAIN     = 5
	VIRTGPU_DRM_CAPSET_DRM              = 6

	// VIRTGPU_BLOB_MEM_* are where a blob resource's memory is.
	VIRTGPU_BLOB_MEM_GUEST        = 0x1
	VIRTGPU_BLOB_MEM_HOST3D       = 0x2
	VIRTGPU_BLOB_MEM_HOST3D_GUEST = 0x3

	// VIRTGPU_BLOB_FLAG_* are how a blob resource may be used.
	VIRTGPU_BLOB_FLAG_USE_MAPPABLE     = 0x1
	VIRTGPU_BLOB_FLAG_USE_SHAREABLE    = 0x2
	VIRTGPU_BLOB_FLAG_USE_CROSS_DEVICE = 0x4

	// VIRTGPU_CONTEXT_PARAM_* are the parameters of DRM_VIRTGPU_CONTEXT_INIT.
	VIRTGPU_CONTEXT_PARAM_CAPSET_ID       = 0x1
	VIRTGPU_CONTEXT_PARAM_NUM_RINGS       = 0x2
	VIRTGPU_CONTEXT_PARAM_POLL_RINGS_MASK = 0x3
	VIRTGPU_CONTEXT_PARAM_DEBUG_NAME      = 0x4

	// VIRTGPU_WAIT_NOWAIT makes DRM_VIRTGPU_WAIT only poll.
	VIRTGPU_WAIT_NOWAIT = 1
)

// DRMVirtgpuMap is struct drm_virtgpu_map.
//
// +marshal
type DRMVirtgpuMap struct {
	_      structs.HostLayout
	Offset uint64
	Handle uint32
	Pad    uint32
}

// DRMVirtgpuExecbuffer is struct drm_virtgpu_execbuffer. It has grown: older
// kernels' is 32 bytes long, without the ring index, or 40, without the sync
// objects.
//
// +marshal
type DRMVirtgpuExecbuffer struct {
	_              structs.HostLayout
	Flags          uint32
	Size           uint32
	Command        uint64
	BOHandles      uint64
	NumBOHandles   uint32
	FenceFD        int32
	RingIdx        uint32
	SyncobjStride  uint32
	NumInSyncobjs  uint32
	NumOutSyncobjs uint32
	InSyncobjs     uint64
	OutSyncobjs    uint64
}

// SizeofDRMVirtgpuExecbufferMin is the size of the oldest struct
// drm_virtgpu_execbuffer.
const SizeofDRMVirtgpuExecbufferMin = 32

// DRMVirtgpuGetparam is struct drm_virtgpu_getparam.
//
// +marshal
type DRMVirtgpuGetparam struct {
	_     structs.HostLayout
	Param uint64
	Value uint64
}

// DRMVirtgpuResourceCreate is struct drm_virtgpu_resource_create.
//
// +marshal
type DRMVirtgpuResourceCreate struct {
	_         structs.HostLayout
	Target    uint32
	Format    uint32
	Bind      uint32
	Width     uint32
	Height    uint32
	Depth     uint32
	ArraySize uint32
	LastLevel uint32
	NrSamples uint32
	Flags     uint32
	BOHandle  uint32
	ResHandle uint32
	Size      uint32
	Stride    uint32
}

// DRMVirtgpuResourceInfo is struct drm_virtgpu_resource_info.
//
// +marshal
type DRMVirtgpuResourceInfo struct {
	_         structs.HostLayout
	BOHandle  uint32
	ResHandle uint32
	Size      uint32
	BlobMem   uint32
}

// DRMVirtgpu3DBox is struct drm_virtgpu_3d_box.
//
// +marshal
type DRMVirtgpu3DBox struct {
	_ structs.HostLayout
	X uint32
	Y uint32
	Z uint32
	W uint32
	H uint32
	D uint32
}

// DRMVirtgpu3DTransfer is struct drm_virtgpu_3d_transfer_to_host, and struct
// drm_virtgpu_3d_transfer_from_host.
//
// +marshal
type DRMVirtgpu3DTransfer struct {
	_           structs.HostLayout
	BOHandle    uint32
	Box         DRMVirtgpu3DBox
	Level       uint32
	Offset      uint32
	Stride      uint32
	LayerStride uint32
}

// DRMVirtgpu3DWait is struct drm_virtgpu_3d_wait.
//
// +marshal
type DRMVirtgpu3DWait struct {
	_      structs.HostLayout
	Handle uint32
	Flags  uint32
}

// DRMVirtgpuGetCaps is struct drm_virtgpu_get_caps.
//
// +marshal
type DRMVirtgpuGetCaps struct {
	_         structs.HostLayout
	CapSetID  uint32
	CapSetVer uint32
	Addr      uint64
	Size      uint32
	Pad       uint32
}

// DRMVirtgpuResourceCreateBlob is struct drm_virtgpu_resource_create_blob.
// Older kernels' is SizeofDRMVirtgpuResourceCreateBlobMin bytes long, without
// the hints.
//
// +marshal
type DRMVirtgpuResourceCreateBlob struct {
	_         structs.HostLayout
	BlobMem   uint32
	BlobFlags uint32
	BOHandle  uint32
	ResHandle uint32
	Size      uint64
	Pad       uint32
	CmdSize   uint32
	Cmd       uint64
	BlobID    uint64
	BlobHints uint32
	Pad2      uint32
}

// SizeofDRMVirtgpuResourceCreateBlobMin is the size of struct
// drm_virtgpu_resource_create_blob before blob hints were added.
const SizeofDRMVirtgpuResourceCreateBlobMin = 48

// DRMVirtgpuContextSetParam is struct drm_virtgpu_context_set_param.
//
// +marshal slice:DRMVirtgpuContextSetParamSlice
type DRMVirtgpuContextSetParam struct {
	_     structs.HostLayout
	Param uint64
	Value uint64
}

// DRMVirtgpuContextInit is struct drm_virtgpu_context_init.
//
// +marshal
type DRMVirtgpuContextInit struct {
	_            structs.HostLayout
	NumParams    uint32
	Pad          uint32
	CtxSetParams uint64
}

// Explicit synchronization: include/uapi/linux/sync_file.h, and dma-bufs:
// include/uapi/linux/dma-buf.h.
const (
	// SYNC_IOC_MAGIC is the ioctl type of sync_file ioctls.
	SYNC_IOC_MAGIC = '>'
	// SYNC_IOC_*_NR are their numbers.
	SYNC_IOC_MERGE_NR        = 3
	SYNC_IOC_FILE_INFO_NR    = 4
	SYNC_IOC_SET_DEADLINE_NR = 5

	// DMA_BUF_BASE is the ioctl type of dma-buf ioctls.
	DMA_BUF_BASE = 'b'
	// DMA_BUF_*_NR are their numbers.
	DMA_BUF_IOCTL_SYNC_NR             = 0
	DMA_BUF_SET_NAME_NR               = 1
	DMA_BUF_IOCTL_EXPORT_SYNC_FILE_NR = 2
	DMA_BUF_IOCTL_IMPORT_SYNC_FILE_NR = 3

	// DMA_BUF_SYNC_* are the flags of DMA_BUF_IOCTL_SYNC, and, READ and
	// WRITE, of DMA_BUF_IOCTL_EXPORT_SYNC_FILE and
	// DMA_BUF_IOCTL_IMPORT_SYNC_FILE.
	DMA_BUF_SYNC_READ  = 1 << 0
	DMA_BUF_SYNC_WRITE = 2 << 0
	DMA_BUF_SYNC_RW    = DMA_BUF_SYNC_READ | DMA_BUF_SYNC_WRITE
	DMA_BUF_SYNC_START = 0 << 2
	DMA_BUF_SYNC_END   = 1 << 2

	// DMA_BUF_NAME_LEN is the size of a dma-buf's name, with its NUL.
	DMA_BUF_NAME_LEN = 32
)

// sync_file and dma-buf ioctls.
var (
	SYNC_IOC_MERGE                 = IOWR(SYNC_IOC_MAGIC, SYNC_IOC_MERGE_NR, 48)
	SYNC_IOC_FILE_INFO             = IOWR(SYNC_IOC_MAGIC, SYNC_IOC_FILE_INFO_NR, 56)
	SYNC_IOC_SET_DEADLINE          = IOW(SYNC_IOC_MAGIC, SYNC_IOC_SET_DEADLINE_NR, 16)
	DMA_BUF_IOCTL_SYNC             = IOW(DMA_BUF_BASE, DMA_BUF_IOCTL_SYNC_NR, 8)
	DMA_BUF_SET_NAME_A             = IOW(DMA_BUF_BASE, DMA_BUF_SET_NAME_NR, 4)
	DMA_BUF_SET_NAME_B             = IOW(DMA_BUF_BASE, DMA_BUF_SET_NAME_NR, 8)
	DMA_BUF_IOCTL_EXPORT_SYNC_FILE = IOWR(DMA_BUF_BASE, DMA_BUF_IOCTL_EXPORT_SYNC_FILE_NR, 8)
	DMA_BUF_IOCTL_IMPORT_SYNC_FILE = IOW(DMA_BUF_BASE, DMA_BUF_IOCTL_IMPORT_SYNC_FILE_NR, 8)
)

// SyncMergeData is struct sync_merge_data.
//
// +marshal
type SyncMergeData struct {
	_     structs.HostLayout
	Name  [32]byte
	FD2   int32
	Fence int32
	Flags uint32
	Pad   uint32
}

// SyncFileInfo is struct sync_file_info.
//
// +marshal
type SyncFileInfo struct {
	_             structs.HostLayout
	Name          [32]byte
	Status        int32
	Flags         uint32
	NumFences     uint32
	Pad           uint32
	SyncFenceInfo uint64
}

// SyncSetDeadline is struct sync_set_deadline.
//
// +marshal
type SyncSetDeadline struct {
	_          structs.HostLayout
	DeadlineNS uint64
	Pad        uint64
}

// DMABufSync is struct dma_buf_sync.
//
// +marshal
type DMABufSync struct {
	_     structs.HostLayout
	Flags uint64
}

// DMABufSyncFile is struct dma_buf_export_sync_file, and struct
// dma_buf_import_sync_file.
//
// +marshal
type DMABufSyncFile struct {
	_     structs.HostLayout
	Flags uint32
	FD    int32
}

// SyncFenceInfo is struct sync_fence_info.
//
// +marshal slice:SyncFenceInfoSlice
type SyncFenceInfo struct {
	_           structs.HostLayout
	ObjName     [32]byte
	DriverName  [32]byte
	Status      int32
	Flags       uint32
	TimestampNS uint64
}
