// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package boot

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/specutils"
)

// classifyPortableResources positively identifies checkpoints whose external
// resources are all reconstructed by TTC. Unknown mounts and devices are
// deliberately host-bound; adding support requires extending this audit.
func classifyPortableResources(conf *config.Config, containerSpecs map[string]*specs.Spec) (checkpoint.ResourceProfile, []string) {
	var reasons []string
	if conf == nil {
		reasons = append(reasons, "runtime configuration is unavailable")
	} else {
		switch conf.Network {
		case config.NetworkTimeTravel, config.NetworkNone:
		default:
			reasons = append(reasons, fmt.Sprintf("network mode %q uses host-bound resources", conf.Network.String()))
		}
		if conf.NVProxy {
			reasons = append(reasons, "NVIDIA proxy device is enabled")
		}
		if conf.TPUProxy {
			reasons = append(reasons, "TPU proxy device is enabled")
		}
		if conf.RDMAProxy {
			reasons = append(reasons, "RDMA proxy device is enabled")
		}
	}
	if len(containerSpecs) == 0 {
		reasons = append(reasons, "container specifications are unavailable")
	}
	names := make([]string, 0, len(containerSpecs))
	for name := range containerSpecs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		spec := containerSpecs[name]
		prefix := fmt.Sprintf("container %q", name)
		if spec == nil {
			reasons = append(reasons, prefix+" has no specification")
			continue
		}
		if spec.Annotations == nil {
			reasons = append(reasons, prefix+" was not created by TTC")
			continue
		}
		if _, ok := spec.Annotations[specutils.AnnotationTTCLiveForkEligible]; !ok {
			reasons = append(reasons, prefix+" lacks the TTC ownership marker")
		}
		internalMounts := make(map[string]struct{})
		if encoded := spec.Annotations[specutils.AnnotationTTCInternalMounts]; encoded != "" {
			var destinations []string
			if err := json.Unmarshal([]byte(encoded), &destinations); err != nil {
				reasons = append(reasons, fmt.Sprintf("%s has invalid TTC internal mounts: %v", prefix, err))
			} else {
				for _, destination := range destinations {
					internalMounts[filepath.Clean(destination)] = struct{}{}
				}
			}
		}
		repositories, err := specutils.TTCVolumeRepositories(spec)
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("%s has invalid TTC repositories: %v", prefix, err))
			continue
		}
		repositorySources := make(map[string]struct{}, len(repositories))
		for _, repository := range repositories {
			repositorySources[repository.Source] = struct{}{}
		}
		if spec.Root == nil {
			reasons = append(reasons, prefix+" has no root filesystem")
		} else if !spec.Root.Readonly {
			if _, ok := repositorySources[specutils.TTCRootfsUpperSource]; !ok {
				reasons = append(reasons, prefix+" has a writable rootfs without a TTC-managed upper repository")
			}
		}
		if spec.Linux != nil && len(spec.Linux.Devices) != 0 {
			reasons = append(reasons, fmt.Sprintf("%s has %d host device declaration(s)", prefix, len(spec.Linux.Devices)))
		}
		for _, mount := range spec.Mounts {
			switch mount.Type {
			case "proc", "sysfs", "tmpfs", "devpts", "mqueue", "cgroup", "cgroup2":
				// Sentry-owned virtual filesystems contain no host identity.
			case "ttcvol":
				if _, ok := repositorySources[mount.Source]; !ok {
					reasons = append(reasons, fmt.Sprintf("%s mount %q has no TTC repository", prefix, mount.Destination))
				}
			case "bind":
				if _, ok := internalMounts[filepath.Clean(mount.Destination)]; !ok {
					reasons = append(reasons, fmt.Sprintf("%s mount %q has host-bound type %q", prefix, mount.Destination, mount.Type))
				}
			default:
				reasons = append(reasons, fmt.Sprintf("%s mount %q has host-bound type %q", prefix, mount.Destination, mount.Type))
			}
		}
	}
	if len(reasons) != 0 {
		return checkpoint.ResourceProfileHostBound, reasons
	}
	return checkpoint.ResourceProfileTTCManaged, nil
}
