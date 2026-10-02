// Copyright 2018 The gVisor Authors.
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

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package linux

import (
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/limits"
)

const (
	onlyPriority = 0

	// maxRTPriority is MAX_RT_PRIO-1, the highest real-time priority.
	maxRTPriority = 99
)

// isSchedulerSupported returns whether the sentry records policy. It does not
// enforce policies: the Go runtime schedules tasks.
func isSchedulerSupported(policy uint) bool {
	switch policy {
	case linux.SCHED_NORMAL, linux.SCHED_FIFO, linux.SCHED_RR, linux.SCHED_BATCH, linux.SCHED_IDLE:
		return true
	}
	// SCHED_DEADLINE is unsupported.
	return false
}

// schedTask returns the task that tid names in the sched_* syscalls: 0 names
// the caller.
func schedTask(t *kernel.Task, tid kernel.ThreadID) (*kernel.Task, error) {
	if tid < 0 {
		return nil, linuxerr.EINVAL
	}
	if tid == 0 {
		return t, nil
	}
	task := t.PIDNamespace().TaskWithID(tid)
	if task == nil {
		return nil, linuxerr.ESRCH
	}
	return task, nil
}

// setSchedPolicy gives target the scheduling policy p on behalf of t. See
// kernel/sched/syscalls.c:__sched_setscheduler().
func setSchedPolicy(t, target *kernel.Task, p kernel.SchedPolicy) error {
	if !isSchedulerSupported(p.Policy) {
		return linuxerr.EINVAL
	}
	// Real-time policies have priorities 1 to 99, others only 0.
	if p.Priority < 0 || p.Priority > maxRTPriority || p.IsRealTime() != (p.Priority != 0) {
		return linuxerr.EINVAL
	}
	if !maySetSchedPolicy(t, target, p) {
		return linuxerr.EPERM
	}
	target.SetSchedPolicy(p)
	return nil
}

// maySetSchedPolicy returns whether t may give target the scheduling policy p.
// Without CAP_SYS_NICE, a task may change only its owner's tasks, may raise
// their real-time priorities only as far as their RLIMIT_RTPRIO, and may not
// clear their SCHED_RESET_ON_FORK. See
// kernel/sched/syscalls.c:user_check_sched_setscheduler().
func maySetSchedPolicy(t, target *kernel.Task, p kernel.SchedPolicy) bool {
	creds := t.Credentials()
	targetCreds := target.Credentials()
	if creds.HasCapabilityIn(linux.CAP_SYS_NICE, targetCreds.UserNamespace) {
		return true
	}
	if creds.EffectiveKUID != targetCreds.RealKUID && creds.EffectiveKUID != targetCreds.EffectiveKUID {
		return false
	}
	current := target.SchedPolicy()
	if p.IsRealTime() {
		rtprio := target.ThreadGroup().Limits().Get(limits.RealTimePriority).Cur
		if p.Policy != current.Policy && rtprio == 0 {
			return false
		}
		if p.Priority > current.Priority && uint64(p.Priority) > rtprio {
			return false
		}
	}
	return !current.ResetOnFork || p.ResetOnFork
}

// SchedParam replicates struct sched_param in sched.h.
//
// +marshal
type SchedParam struct {
	schedPriority int32
}

// SchedGetparam implements linux syscall sched_getparam(2).
func SchedGetparam(t *kernel.Task, sysno uintptr, args arch.SyscallArguments) (uintptr, *kernel.SyscallControl, error) {
	pid := kernel.ThreadID(args[0].Int())
	param := args[1].Pointer()
	if param == 0 {
		return 0, nil, linuxerr.EINVAL
	}
	task, err := schedTask(t, pid)
	if err != nil {
		return 0, nil, err
	}
	r := SchedParam{schedPriority: task.SchedPolicy().Priority}
	if _, err := r.CopyOut(t, param); err != nil {
		return 0, nil, err
	}

	return 0, nil, nil
}

// SchedSetparam implements linux syscall sched_setparam(2).
func SchedSetparam(t *kernel.Task, sysno uintptr, args arch.SyscallArguments) (uintptr, *kernel.SyscallControl, error) {
	pid := kernel.ThreadID(args[0].Int())
	param := args[1].Pointer()
	if pid < 0 || param == 0 {
		return 0, nil, linuxerr.EINVAL
	}
	var r SchedParam
	if _, err := r.CopyIn(t, param); err != nil {
		return 0, nil, err
	}
	task, err := schedTask(t, pid)
	if err != nil {
		return 0, nil, err
	}
	p := task.SchedPolicy()
	p.Priority = r.schedPriority
	return 0, nil, setSchedPolicy(t, task, p)
}

// SchedGetscheduler implements linux syscall sched_getscheduler(2).
func SchedGetscheduler(t *kernel.Task, sysno uintptr, args arch.SyscallArguments) (uintptr, *kernel.SyscallControl, error) {
	task, err := schedTask(t, kernel.ThreadID(args[0].Int()))
	if err != nil {
		return 0, nil, err
	}
	p := task.SchedPolicy()
	policy := uintptr(p.Policy)
	if p.ResetOnFork {
		policy |= linux.SCHED_RESET_ON_FORK
	}
	return policy, nil, nil
}

// SchedSetscheduler implements linux syscall sched_setscheduler(2).
func SchedSetscheduler(t *kernel.Task, sysno uintptr, args arch.SyscallArguments) (uintptr, *kernel.SyscallControl, error) {
	pid := kernel.ThreadID(args[0].Int())
	policy := args[1].Int()
	param := args[2].Pointer()

	if pid < 0 || policy < 0 || param == 0 {
		return 0, nil, linuxerr.EINVAL
	}
	var r SchedParam
	if _, err := r.CopyIn(t, param); err != nil {
		return 0, nil, err
	}
	task, err := schedTask(t, pid)
	if err != nil {
		return 0, nil, err
	}
	return 0, nil, setSchedPolicy(t, task, kernel.SchedPolicy{
		Policy:      uint(policy &^ linux.SCHED_RESET_ON_FORK),
		Priority:    r.schedPriority,
		ResetOnFork: policy&linux.SCHED_RESET_ON_FORK != 0,
	})
}

// schedPriorityRange returns the static priorities of policy, as Linux's
// sched_get_priority_min(2) and sched_get_priority_max(2) do: real-time
// policies have 1 to MAX_RT_PRIO-1, others only 0. Callers such as Android's
// libbinder reject a real-time policy of priority 0.
func schedPriorityRange(policy int32) (min, max uintptr, err error) {
	switch policy {
	case linux.SCHED_FIFO, linux.SCHED_RR:
		return 1, 99, nil
	case linux.SCHED_NORMAL, linux.SCHED_BATCH, linux.SCHED_IDLE, linux.SCHED_DEADLINE:
		return onlyPriority, onlyPriority, nil
	}
	return 0, 0, linuxerr.EINVAL
}

// SchedGetPriorityMax implements linux syscall sched_get_priority_max(2).
func SchedGetPriorityMax(t *kernel.Task, sysno uintptr, args arch.SyscallArguments) (uintptr, *kernel.SyscallControl, error) {
	_, max, err := schedPriorityRange(args[0].Int())
	return max, nil, err
}

// SchedGetPriorityMin implements linux syscall sched_get_priority_min(2).
func SchedGetPriorityMin(t *kernel.Task, sysno uintptr, args arch.SyscallArguments) (uintptr, *kernel.SyscallControl, error) {
	min, _, err := schedPriorityRange(args[0].Int())
	return min, nil, err
}
