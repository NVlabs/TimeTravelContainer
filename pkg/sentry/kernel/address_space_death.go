// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kernel

import (
	"slices"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/sentry/mm"
	"gvisor.dev/gvisor/pkg/sentry/platform"
)

// watchAddressSpaceDeaths has k kill the processes whose AddressSpace's host
// process dies, if k's Platform says when one does.
//
// Preconditions: k.extMu must be locked.
func (k *Kernel) watchAddressSpaceDeaths() {
	if notifier, ok := k.Platform.(platform.AddressSpaceDeathNotifier); ok && k.stopAddressSpaceDeaths == nil {
		k.stopAddressSpaceDeaths = notifier.NotifyAddressSpaceDeath(k.addressSpaceDied)
	}
}

// addressSpaceDied kills the processes using as, whose host process died, as
// when the host's out-of-memory killer picked it. None of them can run again,
// and the pages they hold, which Linux frees as soon as its out-of-memory
// killer kills a process, go only when they exit. Like Linux's, it kills
// every process sharing the memory.
func (k *Kernel) addressSpaceDied(as platform.AddressSpace) {
	type user struct {
		tg *ThreadGroup
		mm *mm.MemoryManager
	}
	var users []user
	k.tasks.mu.RLock()
	k.tasks.forEachTaskLocked(func(t *Task) {
		t.mu.Lock()
		m := t.image.MemoryManager
		t.mu.Unlock()
		if m != nil {
			users = append(users, user{tg: t.tg, mm: m})
		}
	})
	k.tasks.mu.RUnlock()
	var killed []*ThreadGroup
	for _, u := range users {
		if slices.Contains(killed, u.tg) || !u.mm.UsesAddressSpace(as) {
			continue
		}
		killed = append(killed, u.tg)
		u.tg.SendSignal(SignalInfoPriv(linux.SIGKILL))
	}
}
