// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package linux

import (
	"testing"

	"gvisor.dev/gvisor/pkg/marshal"
)

// The sizes are those of the structs in Linux's uapi headers, on 64-bit
// architectures.
func TestDRMStructSizes(t *testing.T) {
	for _, test := range []struct {
		name string
		v    marshal.Marshallable
		want int
	}{
		{"drm_version", &DRMVersion{}, 64},
		{"drm_gem_close", &DRMGemClose{}, 8},
		{"drm_get_cap", &DRMGetCap{}, 16},
		{"drm_prime_handle", &DRMPrimeHandle{}, 12},
		{"drm_virtgpu_map", &DRMVirtgpuMap{}, 16},
		{"drm_virtgpu_execbuffer", &DRMVirtgpuExecbuffer{}, 64},
		{"drm_virtgpu_getparam", &DRMVirtgpuGetparam{}, 16},
		{"drm_virtgpu_resource_create", &DRMVirtgpuResourceCreate{}, 56},
		{"drm_virtgpu_resource_info", &DRMVirtgpuResourceInfo{}, 16},
		{"drm_virtgpu_3d_box", &DRMVirtgpu3DBox{}, 24},
		{"drm_virtgpu_3d_transfer_to_host", &DRMVirtgpu3DTransfer{}, 44},
		{"drm_virtgpu_3d_wait", &DRMVirtgpu3DWait{}, 8},
		{"drm_virtgpu_get_caps", &DRMVirtgpuGetCaps{}, 24},
		{"drm_virtgpu_resource_create_blob", &DRMVirtgpuResourceCreateBlob{}, 56},
		{"drm_virtgpu_context_set_param", &DRMVirtgpuContextSetParam{}, 16},
		{"drm_virtgpu_context_init", &DRMVirtgpuContextInit{}, 16},
		{"sync_merge_data", &SyncMergeData{}, 48},
		{"sync_file_info", &SyncFileInfo{}, 56},
		{"sync_fence_info", &SyncFenceInfo{}, 80},
		{"sync_set_deadline", &SyncSetDeadline{}, 16},
		{"dma_buf_sync", &DMABufSync{}, 8},
		{"dma_buf_export_sync_file", &DMABufSyncFile{}, 8},
	} {
		if got := test.v.SizeBytes(); got != test.want {
			t.Errorf("struct %s is %d bytes, want %d", test.name, got, test.want)
		}
	}
}

// The ioctl commands are those of Linux's uapi headers.
func TestSyncFileAndDMABufIoctls(t *testing.T) {
	for _, test := range []struct {
		name string
		got  uint32
		want uint32
	}{
		{"SYNC_IOC_MERGE", SYNC_IOC_MERGE, 0xc0303e03},
		{"SYNC_IOC_FILE_INFO", SYNC_IOC_FILE_INFO, 0xc0383e04},
		{"SYNC_IOC_SET_DEADLINE", SYNC_IOC_SET_DEADLINE, 0x40103e05},
		{"DMA_BUF_IOCTL_SYNC", DMA_BUF_IOCTL_SYNC, 0x40086200},
		{"DMA_BUF_SET_NAME_A", DMA_BUF_SET_NAME_A, 0x40046201},
		{"DMA_BUF_SET_NAME_B", DMA_BUF_SET_NAME_B, 0x40086201},
		{"DMA_BUF_IOCTL_EXPORT_SYNC_FILE", DMA_BUF_IOCTL_EXPORT_SYNC_FILE, 0xc0086202},
		{"DMA_BUF_IOCTL_IMPORT_SYNC_FILE", DMA_BUF_IOCTL_IMPORT_SYNC_FILE, 0x40086203},
	} {
		if test.got != test.want {
			t.Errorf("%s is %#x, want %#x", test.name, test.got, test.want)
		}
	}
}
