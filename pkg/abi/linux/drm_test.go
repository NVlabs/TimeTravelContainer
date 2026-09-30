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
	} {
		if got := test.v.SizeBytes(); got != test.want {
			t.Errorf("struct %s is %d bytes, want %d", test.name, got, test.want)
		}
	}
}
