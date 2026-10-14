// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package boot

import (
	"fmt"
	"os"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/pkg/cputopology"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/specutils"
)

// applicationNUMANodes is how many host memory nodes the application memory
// files of this process's sandbox place pages on: those of the host CPU
// topology it shows, if it does; see pgalloc.NUMAPlacement.
var applicationNUMANodes int

// sandboxCPUTopology returns the host CPU topology that spec asks the sandbox
// to show its applications, or nil for the single topology, and makes it this
// process's sandbox's: that of the kernels it loads from checkpoints, of its
// memory files, and of the CPU load it shows.
func sandboxCPUTopology(spec *specs.Spec, conf *config.Config) (*cputopology.Topology, error) {
	kernel.SetSandboxCPUTopology(nil)
	applicationNUMANodes = 0
	value := spec.Annotations[specutils.AnnotationCPUTopology]
	if value == "" {
		return nil, nil
	}
	t, err := cputopology.Parse(value)
	if err != nil {
		return nil, fmt.Errorf("annotation %s: %w", specutils.AnnotationCPUTopology, err)
	}
	if t.Mode != cputopology.Host {
		return nil, nil
	}
	if conf.Platform != "systrap" {
		return nil, fmt.Errorf("the host CPU topology needs the systrap platform, whose application threads are host threads; the sandbox runs on %q", conf.Platform)
	}
	kernel.SetSandboxCPUTopology(&t)
	applicationNUMANodes = len(t.Nodes)
	// Opened now, before the Sentry's syscall filters forbid it.
	var files kernel.HostStatFiles
	for name, file := range map[string]**os.File{"/proc/stat": &files.Stat, "/proc/loadavg": &files.Loadavg} {
		f, err := os.Open(name)
		if err != nil {
			log.Warningf("Opening the host's %s: %v; applications see the sandbox's own CPU load", name, err)
			continue
		}
		*file = f
	}
	kernel.SetSandboxHostStatFiles(files)
	return &t, nil
}
