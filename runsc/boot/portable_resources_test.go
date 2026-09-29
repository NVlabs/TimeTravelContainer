// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package boot

import (
	"encoding/json"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/specutils"
)

func portableTestSpec(t *testing.T) *specs.Spec {
	t.Helper()
	repositories, err := json.Marshal([]specutils.TTCVolumeRepository{
		{Source: specutils.TTCRootfsUpperSource, Path: "/managed/root", Branch: "main", CopyMode: "reflink"},
		{Source: "/run/ttc/volumes/data", Path: "/managed/data", Branch: "main", CopyMode: "reflink"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &specs.Spec{
		Root:  &specs.Root{Path: "/image/rootfs"},
		Linux: &specs.Linux{},
		Annotations: map[string]string{
			specutils.AnnotationTTCLiveForkEligible:   "true",
			specutils.AnnotationTTCVolumeRepositories: string(repositories),
			specutils.AnnotationTTCInternalMounts:     `["/etc/hosts"]`,
		},
		Mounts: []specs.Mount{
			{Destination: "/proc", Type: "proc", Source: "proc"},
			{Destination: "/data", Type: "ttcvol", Source: "/run/ttc/volumes/data"},
			{Destination: "/etc/hosts", Type: "bind", Source: "/ttc/internal/hosts"},
		},
	}
}

func TestClassifyPortableResourcesManaged(t *testing.T) {
	profile, reasons := classifyPortableResources(&config.Config{Network: config.NetworkTimeTravel}, map[string]*specs.Spec{"root": portableTestSpec(t)})
	if profile != checkpoint.ResourceProfileTTCManaged || len(reasons) != 0 {
		t.Fatalf("profile=%q reasons=%v", profile, reasons)
	}
}

func TestClassifyPortableResourcesRejectsHostEdges(t *testing.T) {
	spec := portableTestSpec(t)
	spec.Mounts = append(spec.Mounts, specs.Mount{Destination: "/host", Type: "bind", Source: "/host"})
	spec.Linux.Devices = append(spec.Linux.Devices, specs.LinuxDevice{Path: "/dev/example"})
	profile, reasons := classifyPortableResources(&config.Config{Network: config.NetworkHost}, map[string]*specs.Spec{"root": spec})
	if profile != checkpoint.ResourceProfileHostBound || len(reasons) != 3 {
		t.Fatalf("profile=%q reasons=%v, want three host-bound reasons", profile, reasons)
	}
}

func TestClassifyPortableResourcesRequiresManagedWritableRoot(t *testing.T) {
	spec := portableTestSpec(t)
	delete(spec.Annotations, specutils.AnnotationTTCVolumeRepositories)
	profile, reasons := classifyPortableResources(&config.Config{Network: config.NetworkNone}, map[string]*specs.Spec{"root": spec})
	if profile != checkpoint.ResourceProfileHostBound || len(reasons) != 2 {
		t.Fatalf("profile=%q reasons=%v, want missing marker repository and ttcvol reasons", profile, reasons)
	}
}

func TestClassifyPortableResourcesRejectsUnannotatedInternalBind(t *testing.T) {
	spec := portableTestSpec(t)
	delete(spec.Annotations, specutils.AnnotationTTCInternalMounts)
	profile, reasons := classifyPortableResources(&config.Config{Network: config.NetworkNone}, map[string]*specs.Spec{"root": spec})
	if profile != checkpoint.ResourceProfileHostBound || len(reasons) != 1 {
		t.Fatalf("profile=%q reasons=%v, want one host-bound reason", profile, reasons)
	}
}
