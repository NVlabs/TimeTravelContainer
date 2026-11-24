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

package sandboxsetup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/unet"
	"gvisor.dev/gvisor/runsc/cmd/util"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/specutils"
)

// ProcFDBindMount is retained as part of the gofer setup interface. Darwin's
// trusted mode doesn't create a private mount tree.
const ProcFDBindMount = "/proc/fs"

const vfioPathDir = "/dev/vfio"

// MountOpener is unused on Darwin because the gofer serves explicitly named
// host paths in trusted mode.
type MountOpener func(m *specs.Mount, flags uint32) (*os.File, error)

// NewSocket creates a unet.Socket from a file descriptor.
func NewSocket(ioFD int) *unet.Socket {
	socket, err := unet.NewSocket(ioFD)
	if err != nil {
		util.Fatalf("creating server on FD %d: %v", ioFD, err)
	}
	return socket
}

// WriteMounts serializes mounts to the mount-description pipe.
func WriteMounts(mountsFD int, mounts []specs.Mount) error {
	b, err := json.Marshal(mounts)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(mountsFD), "mounts file")
	defer f.Close()
	for len(b) != 0 {
		n, err := f.Write(b)
		if err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

func requireDarwinTrustedMode(conf *config.Config) error {
	if !conf.UnsafeNoHostIsolation {
		return fmt.Errorf("native macOS execution requires --unsafe-no-host-isolation")
	}
	return nil
}

// SetupRootFS validates the trusted-mode root and executes OCI creation hooks.
// There is deliberately no chroot, bind mount, or host mount-namespace setup:
// each lisafs server opens the configured host source directly.
func SetupRootFS(spec *specs.Spec, conf *config.Config, mountConfs []specutils.GoferMountConf, devIoFD int, _ MountOpener, containerID, bundleDir string) error {
	if err := requireDarwinTrustedMode(conf); err != nil {
		return err
	}
	if spec.Root == nil || !filepath.IsAbs(spec.Root.Path) {
		return fmt.Errorf("macOS gofer root must be an absolute host path")
	}
	if len(mountConfs) == 0 || !mountConfs[0].ShouldUseLisafs() {
		return fmt.Errorf("macOS root filesystem requires lisafs")
	}
	if devIoFD >= 0 {
		return fmt.Errorf("macOS does not support host device gofers: %w", unix.EOPNOTSUPP)
	}
	if spec.Hooks != nil && len(spec.Hooks.CreateContainer) != 0 {
		state := specs.State{
			Version: specs.Version, ID: containerID, Status: specs.StateCreating,
			Pid: -1, Bundle: bundleDir, Annotations: spec.Annotations,
		}
		if err := specutils.ExecuteHooks(spec.Hooks.CreateContainer, state); err != nil {
			return fmt.Errorf("executing CreateContainer hooks: %w", err)
		}
	}
	return nil
}

// SetupMounts validates the trusted-mode contract. No host mounts are made.
func SetupMounts(conf *config.Config, _ []specs.Mount, _, _ string, _ []specutils.GoferMountConf, _ MountOpener) error {
	return requireDarwinTrustedMode(conf)
}

// ShouldExposeNvidiaDevice reports whether path names a supported NVIDIA node.
func ShouldExposeNvidiaDevice(path string) bool {
	if path == "/dev/nvidiactl" || path == "/dev/nvidia-uvm" || path == "/dev/nvidia-uvm-tools" {
		return true
	}
	return regexp.MustCompile(`^/dev/nvidia(\d+)$`).MatchString(path)
}

// ShouldExposeVFIODevice reports whether path names a VFIO node.
func ShouldExposeVFIODevice(path string) bool { return strings.HasPrefix(path, vfioPathDir) }

// ShouldExposeTpuDevice reports whether path names a TPU or VFIO node.
func ShouldExposeTpuDevice(path string) bool {
	valid, _ := util.IsTPUDeviceValid(path)
	return valid || ShouldExposeVFIODevice(path)
}

// ShouldExposeRDMADevice reports whether path names an RDMA userspace node.
func ShouldExposeRDMADevice(path string) bool {
	return strings.HasPrefix(path, "/dev/infiniband/uverbs")
}

// SetupDev rejects host-device passthrough on native macOS.
func SetupDev(_ *specs.Spec, _ *config.Config, _, _ string) error { return unix.EOPNOTSUPP }

func mountOptionsPath(conf *config.Config, root, destination, source string) string {
	if conf.UnsafeNoHostIsolation {
		return source
	}
	return filepath.Join(root, destination)
}

// ResolveMounts converts mount destinations to canonical guest-absolute paths
// and attaches filesystem-specific gofer options. Sources remain host paths.
func ResolveMounts(conf *config.Config, mounts []specs.Mount, _ string, mountConfs []specutils.GoferMountConf) ([]specs.Mount, error) {
	if err := requireDarwinTrustedMode(conf); err != nil {
		return nil, err
	}
	mountIdx := 1
	clean := make([]specs.Mount, 0, len(mounts))
	for _, m := range mounts {
		if !specutils.HasMountConfig(m) {
			clean = append(clean, m)
			continue
		}
		if mountIdx >= len(mountConfs) {
			return nil, fmt.Errorf("missing gofer mount configuration for %q", m.Destination)
		}
		mountConf := mountConfs[mountIdx]
		mountIdx++
		if !mountConf.ShouldUseLisafs() {
			clean = append(clean, m)
			continue
		}
		if !filepath.IsAbs(m.Source) || !filepath.IsAbs(m.Destination) {
			return nil, fmt.Errorf("macOS gofer mount source and destination must be absolute: %+v", m)
		}
		opts, err := AdjustMountOptions(conf, m.Source, m.Options)
		if err != nil {
			return nil, fmt.Errorf("adjusting options for mount source %q: %w", m.Source, err)
		}
		cpy := m
		cpy.Destination = filepath.Clean(m.Destination)
		cpy.Options = opts
		clean = append(clean, cpy)
	}
	return clean, nil
}
