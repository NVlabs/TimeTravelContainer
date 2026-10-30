// Copyright 2020 The gVisor Authors.
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
	"math"
	"time"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/ktime"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/waiter"
)

var sizeofEpollEvent = (*linux.EpollEvent)(nil).SizeBytes()

// EpollCreate1 implements Linux syscall epoll_create1(2).
func EpollCreate1(t *kernel.Task, sysno uintptr, args arch.SyscallArguments) (uintptr, *kernel.SyscallControl, error) {
	flags := args[0].Int()
	if flags&^linux.EPOLL_CLOEXEC != 0 {
		return 0, nil, linuxerr.EINVAL
	}

	file, err := t.Kernel().VFS().NewEpollInstanceFD(t)
	if err != nil {
		return 0, nil, err
	}
	defer file.DecRef(t)

	fd, err := t.NewFDFrom(0, file, kernel.FDFlags{
		CloseOnExec: flags&linux.EPOLL_CLOEXEC != 0,
	})
	if err != nil {
		return 0, nil, err
	}
	return uintptr(fd), nil, nil
}

// EpollCreate implements Linux syscall epoll_create(2).
func EpollCreate(t *kernel.Task, sysno uintptr, args arch.SyscallArguments) (uintptr, *kernel.SyscallControl, error) {
	size := args[0].Int()

	// "Since Linux 2.6.8, the size argument is ignored, but must be greater
	// than zero" - epoll_create(2)
	if size <= 0 {
		return 0, nil, linuxerr.EINVAL
	}

	file, err := t.Kernel().VFS().NewEpollInstanceFD(t)
	if err != nil {
		return 0, nil, err
	}
	defer file.DecRef(t)

	fd, err := t.NewFDFrom(0, file, kernel.FDFlags{})
	if err != nil {
		return 0, nil, err
	}
	return uintptr(fd), nil, nil
}

// EpollCtl implements Linux syscall epoll_ctl(2).
func EpollCtl(t *kernel.Task, sysno uintptr, args arch.SyscallArguments) (uintptr, *kernel.SyscallControl, error) {
	epfd := args[0].Int()
	op := args[1].Int()
	fd := args[2].Int()
	eventAddr := args[3].Pointer()

	epfile := t.GetFile(epfd)
	if epfile == nil {
		return 0, nil, linuxerr.EBADF
	}
	defer epfile.DecRef(t)
	ep, ok := epfile.Impl().(*vfs.EpollInstance)
	if !ok {
		return 0, nil, linuxerr.EINVAL
	}
	file := t.GetFile(fd)
	if file == nil {
		return 0, nil, linuxerr.EBADF
	}
	defer file.DecRef(t)
	if epfile == file {
		return 0, nil, linuxerr.EINVAL
	}

	var event linux.EpollEvent
	switch op {
	case linux.EPOLL_CTL_ADD:
		if _, err := event.CopyIn(t, eventAddr); err != nil {
			return 0, nil, err
		}
		return 0, nil, ep.AddInterest(file, fd, event)
	case linux.EPOLL_CTL_DEL:
		return 0, nil, ep.DeleteInterest(file, fd)
	case linux.EPOLL_CTL_MOD:
		if _, err := event.CopyIn(t, eventAddr); err != nil {
			return 0, nil, err
		}
		return 0, nil, ep.ModifyInterest(file, fd, event)
	default:
		return 0, nil, linuxerr.EINVAL
	}
}

// waitEpoll implements the wait of epoll_wait(2) and epoll_pwait(2): it
// waits forever if timeoutInNanos is negative. tempMask is whether the
// caller set a temporary signal mask.
//
// Linux fails an interrupted wait with EINTR. So does the sentry if a signal
// handler runs. Otherwise, as after its pause for a checkpoint or a live
// fork, it restarts the wait, with the same deadline and signal mask, rather
// than have the application retry it: after a pause, every waiting thread
// would do so at once.
func waitEpoll(t *kernel.Task, epfd int32, eventsAddr hostarch.Addr, maxEvents int, timeoutInNanos int64, tempMask bool) (uintptr, *kernel.SyscallControl, error) {
	var deadline ktime.Time
	if timeoutInNanos > 0 {
		deadline = t.Kernel().MonotonicClock().Now().Add(time.Duration(timeoutInNanos) * time.Nanosecond)
	}
	n, err := epollWaitUntil(t, epfd, eventsAddr, maxEvents, timeoutInNanos, deadline)
	if linuxerr.Equals(linuxerr.ErrInterrupted, err) {
		switch {
		case timeoutInNanos < 0:
			err = linuxerr.ERESTARTNOHAND
		case !tempMask:
			t.SetSyscallRestartBlock(&epollWaitRestartBlock{
				epfd:       epfd,
				eventsAddr: eventsAddr,
				maxEvents:  maxEvents,
				deadline:   deadline,
			})
			err = linuxerr.ERESTART_RESTARTBLOCK
		default:
			t.SetSyscallRestartBlock(&epollPwaitRestartBlock{
				epollWaitRestartBlock: epollWaitRestartBlock{
					epfd:       epfd,
					eventsAddr: eventsAddr,
					maxEvents:  maxEvents,
					deadline:   deadline,
				},
				mask: t.SignalMask(),
			})
			err = linuxerr.ERESTART_RESTARTBLOCK
		}
	}
	return n, nil, err
}

// epollWaitRestartBlock restarts a timed epoll wait via restart_syscall(2),
// until its deadline.
//
// +stateify savable
type epollWaitRestartBlock struct {
	epfd       int32
	eventsAddr hostarch.Addr
	maxEvents  int
	deadline   ktime.Time
}

// Restart implements kernel.SyscallRestartBlock.Restart.
func (r *epollWaitRestartBlock) Restart(t *kernel.Task) (uintptr, error) {
	return r.wait(t, r)
}

// wait waits until r's deadline, and has restart_syscall(2) run self if
// interrupted again.
func (r *epollWaitRestartBlock) wait(t *kernel.Task, self kernel.SyscallRestartBlock) (uintptr, error) {
	// A deadline in the past times out at once.
	remaining := max(r.deadline.Sub(t.Kernel().MonotonicClock().Now()).Nanoseconds(), 1)
	n, err := epollWaitUntil(t, r.epfd, r.eventsAddr, r.maxEvents, remaining, r.deadline)
	if linuxerr.Equals(linuxerr.ErrInterrupted, err) {
		t.SetSyscallRestartBlock(self)
		err = linuxerr.ERESTART_RESTARTBLOCK
	}
	return n, err
}

// epollPwaitRestartBlock restarts a timed epoll wait with a temporary signal
// mask. The task restores its own mask before restart_syscall(2) runs, so the
// restart sets the temporary one again.
//
// +stateify savable
type epollPwaitRestartBlock struct {
	epollWaitRestartBlock
	mask linux.SignalSet
}

// Restart implements kernel.SyscallRestartBlock.Restart.
func (r *epollPwaitRestartBlock) Restart(t *kernel.Task) (uintptr, error) {
	t.SetSavedSignalMask(t.SignalMask())
	t.SetSignalMask(r.mask)
	return r.wait(t, r)
}

// epollWaitUntil reads epfd's ready events to eventsAddr. If there are none,
// it returns at once if timeoutInNanos is 0, and otherwise waits for them:
// forever if timeoutInNanos is negative, else until deadline.
func epollWaitUntil(t *kernel.Task, epfd int32, eventsAddr hostarch.Addr, maxEvents int, timeoutInNanos int64, deadline ktime.Time) (uintptr, error) {
	var _EP_MAX_EVENTS = math.MaxInt32 / sizeofEpollEvent // Linux: fs/eventpoll.c:EP_MAX_EVENTS
	if maxEvents <= 0 || maxEvents > _EP_MAX_EVENTS {
		return 0, linuxerr.EINVAL
	}

	epfile := t.GetFile(epfd)
	if epfile == nil {
		return 0, linuxerr.EBADF
	}
	defer epfile.DecRef(t)
	ep, ok := epfile.Impl().(*vfs.EpollInstance)
	if !ok {
		return 0, linuxerr.EINVAL
	}

	// Record what this wait is actually for. The syscall number says only that
	// the task called epoll_wait; the registered descriptors and the deadline
	// are what distinguish an idle prompt from an outstanding socket read.
	interested, files := ep.InterestedFDs()
	t.SetWaitInfo("epoll", interested, classifyWaitFDs(t, interested, files, true), timeoutInNanos)
	defer t.ClearWaitInfo()

	// Allocate space for a few events on the stack for the common case in
	// which we don't have too many events.
	var (
		eventsArr [16]linux.EpollEvent
		ch        chan struct{}
	)
	for {
		events := ep.ReadEvents(eventsArr[:0], maxEvents)
		if len(events) != 0 {
			copiedBytes, err := linux.CopyEpollEventSliceOut(t, eventsAddr, events)
			copiedEvents := copiedBytes / sizeofEpollEvent // rounded down
			if copiedEvents != 0 {
				return uintptr(copiedEvents), nil
			}
			return 0, err
		}
		if timeoutInNanos == 0 {
			return 0, nil
		}
		// In the first iteration of this loop, register with the epoll
		// instance for readability events, but then immediately continue the
		// loop since we need to retry ReadEvents() before blocking. In all
		// subsequent iterations, block until events are available, the timeout
		// expires, or an interrupt arrives.
		if ch == nil {
			var w waiter.Entry
			w, ch = waiter.NewChannelEntry(waiter.ReadableEvents)
			if err := epfile.EventRegister(&w); err != nil {
				return 0, err
			}
			defer epfile.EventUnregister(&w)
		} else {
			if err := t.BlockWithDeadline(ch, timeoutInNanos > 0, deadline); err != nil {
				if linuxerr.Equals(linuxerr.ETIMEDOUT, err) {
					err = nil
				}
				return 0, err
			}
		}
	}

}

// EpollWait implements Linux syscall epoll_wait(2).
func EpollWait(t *kernel.Task, sysno uintptr, args arch.SyscallArguments) (uintptr, *kernel.SyscallControl, error) {
	epfd := args[0].Int()
	eventsAddr := args[1].Pointer()
	maxEvents := int(args[2].Int())
	timeoutInNanos := int64(args[3].Int()) * 1000000

	return waitEpoll(t, epfd, eventsAddr, maxEvents, timeoutInNanos, false /* tempMask */)
}

// EpollPwait implements Linux syscall epoll_pwait(2).
func EpollPwait(t *kernel.Task, sysno uintptr, args arch.SyscallArguments) (uintptr, *kernel.SyscallControl, error) {
	maskAddr := args[4].Pointer()
	maskSize := uint(args[5].Uint())

	if err := setTempSignalSet(t, maskAddr, maskSize); err != nil {
		return 0, nil, err
	}

	epfd := args[0].Int()
	eventsAddr := args[1].Pointer()
	maxEvents := int(args[2].Int())
	timeoutInNanos := int64(args[3].Int()) * 1000000
	return waitEpoll(t, epfd, eventsAddr, maxEvents, timeoutInNanos, maskAddr != 0)
}

// EpollPwait2 implements Linux syscall epoll_pwait(2).
func EpollPwait2(t *kernel.Task, sysno uintptr, args arch.SyscallArguments) (uintptr, *kernel.SyscallControl, error) {
	epfd := args[0].Int()
	eventsAddr := args[1].Pointer()
	maxEvents := int(args[2].Int())
	timeoutPtr := args[3].Pointer()
	maskAddr := args[4].Pointer()
	maskSize := uint(args[5].Uint())
	haveTimeout := timeoutPtr != 0

	var timeoutInNanos int64 = -1
	if haveTimeout {
		var timeout linux.Timespec
		if _, err := timeout.CopyIn(t, timeoutPtr); err != nil {
			return 0, nil, err
		}
		timeoutInNanos = timeout.ToNsec()
	}

	if err := setTempSignalSet(t, maskAddr, maskSize); err != nil {
		return 0, nil, err
	}

	return waitEpoll(t, epfd, eventsAddr, maxEvents, timeoutInNanos, maskAddr != 0)
}
