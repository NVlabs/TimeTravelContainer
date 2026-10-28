// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package boot

import (
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/runsc/boot/sentryapi"
)

func TestAliasLiveForkFilesystemResources(t *testing.T) {
	current := checkpoint.ResourceID{ContainerName: "ttc-volume", Path: "node-a-repository"}
	incoming := checkpoint.ResourceID{ContainerName: "ttc-volume", Path: "node-b-repository"}
	resource := new(int)
	resources := vfs.LiveForkFilesystemResources{current: resource}

	aliased, err := aliasLiveForkFilesystemResources(resources, map[string]string{current.Path: incoming.Path})
	if err != nil {
		t.Fatalf("aliasLiveForkFilesystemResources: %v", err)
	}
	if got := aliased[current]; got != resource {
		t.Fatalf("current identity maps to %p, want %p", got, resource)
	}
	if got := aliased[incoming]; got != resource {
		t.Fatalf("incoming identity maps to %p, want %p", got, resource)
	}
	if _, ok := resources[incoming]; ok {
		t.Fatal("aliasing mutated the caller's resource map")
	}
}

func TestAliasLiveForkFilesystemResourcesRejectsMissingCurrent(t *testing.T) {
	_, err := aliasLiveForkFilesystemResources(vfs.LiveForkFilesystemResources{}, map[string]string{"missing": "incoming"})
	if err == nil {
		t.Fatal("aliasLiveForkFilesystemResources succeeded without the active resource")
	}
}

func TestAliasRestoreFilesystemResourcesAcrossReturnHop(t *testing.T) {
	current := checkpoint.ResourceID{ContainerName: "ttc-volume", Path: "node-b-current"}
	stale := checkpoint.ResourceID{ContainerName: "ttc-volume", Path: "node-b-first-import"}
	incoming := checkpoint.ResourceID{ContainerName: "ttc-volume", Path: "node-a-return-hop"}
	resource := vfs.RestoreFilesystemResource{Resource: new(int), Branch: "branch"}
	resources := vfs.RestoreFilesystemResources{current: resource, stale: resource}

	aliased, err := aliasRestoreFilesystemResources(resources, map[string]string{current.Path: incoming.Path})
	if err != nil {
		t.Fatalf("aliasRestoreFilesystemResources: %v", err)
	}
	for _, id := range []checkpoint.ResourceID{current, stale, incoming} {
		if got, ok := aliased[id]; !ok || got.Resource != resource.Resource || got.Branch != resource.Branch {
			t.Fatalf("identity %q maps to %+v, want %+v", id, got, resource)
		}
	}
	if _, ok := resources[incoming]; ok {
		t.Fatal("aliasing mutated the caller's resource map")
	}
}

func TestAliasRestoreFilesystemResourcesRejectsMissingCurrent(t *testing.T) {
	_, err := aliasRestoreFilesystemResources(vfs.RestoreFilesystemResources{}, map[string]string{"missing": "incoming"})
	if err == nil {
		t.Fatal("aliasRestoreFilesystemResources succeeded without the active resource")
	}
}

// While a restore holds the loader's lock, a runtime that asks whether the
// container still runs is told at once that it is being created, instead of
// waiting for the restore to finish.
func TestContainerRuntimeStateDuringRestore(t *testing.T) {
	l := &Loader{}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.restoring.Store(true)
	defer l.restoring.Store(false)
	answered := make(chan sentryapi.ContainerRuntimeState, 1)
	go func() { answered <- l.containerRuntimeState("restoring") }()
	select {
	case state := <-answered:
		if state != sentryapi.RuntimeStateCreating {
			t.Errorf("containerRuntimeState during a restore=%v, want %v", state, sentryapi.RuntimeStateCreating)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("containerRuntimeState waited for the restore")
	}
}
