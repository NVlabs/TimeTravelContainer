// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package boot

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"gvisor.dev/gvisor/pkg/sentry/devices/virtgpu"
	"gvisor.dev/gvisor/pkg/sentry/devices/virtgpu/rendererclient"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

// gfxCapset is a capability set as ttc-gfx-renderer's probe prints it.
type gfxCapset struct {
	ID         uint32 `json:"id"`
	MaxVersion uint32 `json:"max_version"`
	Data       []byte `json:"data"`
}

// decodeGFXCapsets decodes --gfx-capsets.
func decodeGFXCapsets(encoded string) ([]virtgpu.Capset, error) {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("--gfx-capsets: %w", err)
	}
	var decoded []gfxCapset
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, fmt.Errorf("--gfx-capsets: %w", err)
	}
	capsets := make([]virtgpu.Capset, 0, len(decoded))
	for _, c := range decoded {
		capsets = append(capsets, virtgpu.Capset{ID: c.ID, MaxVersion: c.MaxVersion, Data: c.Data})
	}
	return capsets, nil
}

// gfxForkPolicy parses --gfx-fork.
func gfxForkPolicy(value string) (virtgpu.ForkPolicy, error) {
	for _, policy := range []virtgpu.ForkPolicy{virtgpu.ForkIneligible, virtgpu.ForkDeviceLost, virtgpu.ForkSnapshot} {
		if value == policy.String() {
			return policy, nil
		}
	}
	return 0, fmt.Errorf("--gfx-fork=%q: want ineligible, device-lost, or snapshot", value)
}

// gfxRegisterDevices registers the virtual GPU, if the sandbox has one. It
// takes info.gfxFD.
func gfxRegisterDevices(info *containerInfo, vfsObj *vfs.VirtualFilesystem) error {
	conf := info.conf
	if !conf.GFX {
		return nil
	}
	capsets, err := decodeGFXCapsets(conf.GFXCapsets)
	if err != nil {
		return err
	}
	policy, err := gfxForkPolicy(conf.GFXFork)
	if err != nil {
		return err
	}
	if info.gfxFD < 0 {
		return fmt.Errorf("the virtual GPU has no connection to the GPU renderers' broker")
	}
	broker, err := rendererclient.NewBroker(info.gfxFD)
	info.gfxFD = -1
	if err != nil {
		return fmt.Errorf("connecting to the GPU renderers' broker: %w", err)
	}
	if err := virtgpu.Register(vfsObj, virtgpu.Options{
		Capsets:     capsets,
		NewRenderer: broker.NewRenderer,
		ForkPolicy:  policy,
	}); err != nil {
		broker.Close()
		return fmt.Errorf("registering the virtual GPU: %w", err)
	}
	return nil
}
