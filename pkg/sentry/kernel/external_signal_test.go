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
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

// A signal from outside the sandbox reaches a PID namespace's init as one
// from a container runtime does on Linux: SIGKILL always, and any other
// signal only through a handler the init installed. A process that is not
// init receives it either way.
func TestExternalSignalsReachInitOnlyThroughAHandler(t *testing.T) {
	k, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	k.PipeMount().IncRef()
	k.SetHostMount(k.PipeMount())
	ctx := k.SupervisorContext()
	creds := auth.CredentialsFromContext(ctx)
	mntns, err := k.VFS().NewMountNamespace(ctx, creds, "", tmpfs.Name, &vfs.MountOptions{}, k)
	if err != nil {
		t.Fatalf("NewMountNamespace: %v", err)
	}
	root := mntns.Root(ctx)
	defer root.DecRef(ctx)
	init, err := testutil.CreateTask(ctx, "init", k.GlobalInit(), mntns, root, root)
	if err != nil {
		t.Fatalf("CreateTask(init): %v", err)
	}
	ls, err := limits.NewLinuxLimitSet()
	if err != nil {
		t.Fatal(err)
	}
	otherTG := k.NewThreadGroup(k.RootPIDNamespace(), kernel.NewSignalHandlers(), linux.SIGCHLD, ls)
	other, err := testutil.CreateTask(ctx, "other", otherTG, mntns, root, root)
	if err != nil {
		t.Fatalf("CreateTask(other): %v", err)
	}
	pending := func(task *kernel.Task, sig linux.Signal) bool {
		return task.PendingSignals()&linux.SignalSetOf(sig) != 0
	}
	send := func(tg *kernel.ThreadGroup, sig linux.Signal) {
		t.Helper()
		if err := k.SendExternalSignalThreadGroup(tg, &linux.SignalInfo{Signo: int32(sig)}); err != nil {
			t.Fatalf("sending signal %d: %v", sig, err)
		}
	}

	send(k.GlobalInit(), linux.SIGTERM)
	if pending(init, linux.SIGTERM) {
		t.Error("SIGTERM reached an init that has no handler for it")
	}
	if err := k.SendContainerSignal(init.ContainerID(), &linux.SignalInfo{Signo: int32(linux.SIGINT)}); err != nil {
		t.Fatalf("SendContainerSignal: %v", err)
	}
	if pending(init, linux.SIGINT) {
		t.Error("SIGINT to every process reached an init that has no handler for it")
	}
	if !pending(other, linux.SIGINT) {
		t.Error("SIGINT to every process did not reach a process that is not init")
	}
	send(otherTG, linux.SIGTERM)
	if !pending(other, linux.SIGTERM) {
		t.Error("SIGTERM did not reach a process that is not init")
	}
	if _, err := k.GlobalInit().SetSigAction(linux.SIGTERM, &linux.SigAction{Handler: 0x1000}); err != nil {
		t.Fatalf("SetSigAction: %v", err)
	}
	send(k.GlobalInit(), linux.SIGTERM)
	if !pending(init, linux.SIGTERM) {
		t.Error("SIGTERM did not reach an init that handles it")
	}
	send(k.GlobalInit(), linux.SIGKILL)
	if !pending(init, linux.SIGKILL) {
		t.Error("SIGKILL did not reach init")
	}
}
