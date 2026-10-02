// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package linux

import (
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
)

// Reboot implements Linux syscall reboot(2) as Linux does in a PID namespace
// other than its first (kernel/pid_namespace.c:reboot_pid_ns()), which a
// container's are: restarting ends the namespace as SIGHUP would, and
// powering off and halting, as SIGINT would. A container's init that fails,
// such as Android's, reboots, and would otherwise retry forever.
func Reboot(t *kernel.Task, sysno uintptr, args arch.SyscallArguments) (uintptr, *kernel.SyscallControl, error) {
	magic1 := uint32(args[0].Int())
	magic2 := uint32(args[1].Int())
	cmd := uint32(args[2].Int())

	pidns := t.PIDNamespace()
	if !t.HasCapabilityIn(linux.CAP_SYS_BOOT, pidns.UserNamespace()) {
		return 0, nil, linuxerr.EPERM
	}
	if magic1 != linux.LINUX_REBOOT_MAGIC1 ||
		(magic2 != linux.LINUX_REBOOT_MAGIC2 && magic2 != linux.LINUX_REBOOT_MAGIC2A &&
			magic2 != linux.LINUX_REBOOT_MAGIC2B && magic2 != linux.LINUX_REBOOT_MAGIC2C) {
		return 0, nil, linuxerr.EINVAL
	}
	var sig linux.Signal
	switch cmd {
	case linux.LINUX_REBOOT_CMD_RESTART, linux.LINUX_REBOOT_CMD_RESTART2:
		sig = linux.SIGHUP
	case linux.LINUX_REBOOT_CMD_POWER_OFF, linux.LINUX_REBOOT_CMD_HALT:
		sig = linux.SIGINT
	default:
		return 0, nil, linuxerr.EINVAL
	}
	// The namespace ends with its init.
	init := pidns.ThreadGroupWithID(1)
	if init == nil || init == t.ThreadGroup() {
		t.PrepareGroupExit(linux.WaitStatusTerminationSignal(sig))
		return 0, kernel.CtrlDoExit, nil
	}
	if err := init.SendSignal(kernel.SignalInfoPriv(linux.SIGKILL)); err != nil {
		return 0, nil, err
	}
	t.PrepareExit(linux.WaitStatusExit(0))
	return 0, kernel.CtrlDoExit, nil
}
