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

package boot

import (
	"testing"

	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
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
