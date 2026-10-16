// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kernel_test

import (
	"testing"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/testutil"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/tmpfs"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/limits"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

// deathNotifyingPlatform tells its Kernel of the death of the AddressSpaces a
// test names, as a Platform whose AddressSpaces are host processes tells it
// of those the host kills.
type deathNotifyingPlatform struct {
	platform.Platform
	died func(platform.AddressSpace)
}

func (p *deathNotifyingPlatform) NotifyAddressSpaceDeath(f func(platform.AddressSpace)) func() {
	p.died = f
	return func() { p.died = nil }
}

// When the host process of an address space dies, the processes using it are
// killed, and no others.
func TestAddressSpaceDeathKillsItsProcesses(t *testing.T) {
	k, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	k.PipeMount().IncRef()
	k.SetHostMount(k.PipeMount())
	p := &deathNotifyingPlatform{Platform: k.Platform}
	k.Platform = p
	k.Pause()
	if err := k.StartPaused(); err != nil {
		t.Fatalf("StartPaused: %v", err)
	}
	if p.died == nil {
		t.Fatal("the started Kernel is not told of its address spaces' deaths")
	}
	ctx := k.SupervisorContext()
	creds := auth.CredentialsFromContext(ctx)
	mntns, err := k.VFS().NewMountNamespace(ctx, creds, "", tmpfs.Name, &vfs.MountOptions{}, k)
	if err != nil {
		t.Fatalf("NewMountNamespace: %v", err)
	}
	root := mntns.Root(ctx)
	defer root.DecRef(ctx)
	newProcess := func(name string) *kernel.Task {
		t.Helper()
		ls, err := limits.NewLinuxLimitSet()
		if err != nil {
			t.Fatal(err)
		}
		tg := k.NewThreadGroup(k.RootPIDNamespace(), kernel.NewSignalHandlers(), linux.SIGCHLD, ls)
		task, err := testutil.CreateTask(ctx, name, tg, mntns, root, root)
		if err != nil {
			t.Fatalf("CreateTask(%s): %v", name, err)
		}
		return task
	}
	victim := newProcess("victim")
	bystander := newProcess("bystander")
	var as platform.AddressSpace
	victim.WithMuLocked(func(t *kernel.Task) { as = t.MemoryManager().AddressSpace() })
	p.died(as)
	if !victim.Killed() {
		t.Error("the process whose address space died was not killed")
	}
	if bystander.Killed() {
		t.Error("a process with an address space of its own was killed")
	}
}
