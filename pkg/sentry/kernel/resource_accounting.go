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

package kernel

import "gvisor.dev/gvisor/pkg/sentry/mm"

// ResidentPageVersions returns the physical-and-logical page identities mapped
// by any task in k. Multiple threads and processes mapping the same resident
// page contribute one identity.
func (k *Kernel) ResidentPageVersions() map[mm.ResidentPageVersion]struct{} {
	managers := make(map[*mm.MemoryManager]struct{})
	ts := k.TaskSet()
	ts.mu.RLock()
	for tg := range k.RootPIDNamespace().tgids {
		tg.ForEachTaskLocked(func(task *Task) bool {
			if memoryManager := task.MemoryManager(); memoryManager != nil {
				if _, exists := managers[memoryManager]; !exists && memoryManager.IncUsers() {
					managers[memoryManager] = struct{}{}
				}
			}
			return true
		})
	}
	ts.mu.RUnlock()
	defer func() {
		for memoryManager := range managers {
			memoryManager.DecUsers(k.SupervisorContext())
		}
	}()
	pages := make(map[mm.ResidentPageVersion]struct{})
	for memoryManager := range managers {
		for page := range memoryManager.ResidentPageVersions() {
			pages[page] = struct{}{}
		}
	}
	return pages
}
