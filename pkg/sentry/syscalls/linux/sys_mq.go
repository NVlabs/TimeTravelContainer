// Copyright 2021 The gVisor Authors.
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
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/marshal/primitive"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/mqfs"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/mq"
	"gvisor.dev/gvisor/pkg/sentry/ktime"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/waiter"
)

// MqOpen implements mq_open(2).
func MqOpen(t *kernel.Task, sysno uintptr, args arch.SyscallArguments) (uintptr, *kernel.SyscallControl, error) {
	nameAddr := args[0].Pointer()
	flag := args[1].Int()
	mode := args[2].ModeT()
	attrAddr := args[3].Pointer()

	name, err := t.CopyInString(nameAddr, mq.MaxName)
	if err != nil {
		return 0, nil, err
	}

	rOnly := flag&linux.O_RDONLY == linux.O_RDONLY
	wOnly := flag&linux.O_WRONLY == linux.O_WRONLY
	readWrite := flag&linux.O_RDWR == linux.O_RDWR

	create := flag&linux.O_CREAT == linux.O_CREAT
	exclusive := flag&linux.O_EXCL == linux.O_EXCL
	block := flag&linux.O_NONBLOCK != linux.O_NONBLOCK

	var attr linux.MqAttr
	var attrPtr *linux.MqAttr
	if attrAddr != 0 {
		if _, err := attr.CopyIn(t, attrAddr); err != nil {
			return 0, nil, err
		}
		attrPtr = &attr
	}

	opts := openOpts(name, rOnly, wOnly, readWrite, create, exclusive, block)

	r := t.IPCNamespace().PosixQueues()
	queue, err := r.FindOrCreate(t, opts, linux.FileMode(mode), attrPtr)
	if err != nil {
		return 0, nil, err
	}
	defer queue.DecRef(t)

	fd, err := t.NewFDFrom(0, queue, kernel.FDFlags{
		CloseOnExec: flag&linux.O_CLOEXEC != 0,
	})
	if err != nil {
		return 0, nil, err
	}
	return uintptr(fd), nil, nil
}

// MqUnlink implements mq_unlink(2).
func MqUnlink(t *kernel.Task, sysno uintptr, args arch.SyscallArguments) (uintptr, *kernel.SyscallControl, error) {
	nameAddr := args[0].Pointer()
	name, err := t.CopyInString(nameAddr, mq.MaxName)
	if err != nil {
		return 0, nil, err
	}
	return 0, nil, t.IPCNamespace().PosixQueues().Remove(t, name)
}

func openOpts(name string, rOnly, wOnly, readWrite, create, exclusive, block bool) mq.OpenOpts {
	var access mq.AccessType
	switch {
	case readWrite:
		access = mq.ReadWrite
	case wOnly:
		access = mq.WriteOnly
	case rOnly:
		access = mq.ReadOnly
	}

	return mq.OpenOpts{
		Name:      name,
		Access:    access,
		Create:    create,
		Exclusive: exclusive,
		Block:     block,
	}
}

// MqTimedsend implements mq_timedsend(2).
func MqTimedsend(t *kernel.Task, sysno uintptr, args arch.SyscallArguments) (uintptr, *kernel.SyscallControl, error) {
	mqdes := args[0].Int()
	msgAddr := args[1].Pointer()
	msgLen := args[2].SizeT()
	priority := args[3].Uint()
	timeoutAddr := args[4].Pointer()

	haveDeadline, deadline, err := mqDeadline(t, timeoutAddr)
	if err != nil {
		return 0, nil, err
	}
	if priority >= linux.MQ_PRIO_MAX {
		return 0, nil, linuxerr.EINVAL
	}
	file, q, err := mqFile(t, mqdes)
	if err != nil {
		return 0, nil, err
	}
	defer file.DecRef(t)
	if !file.IsWritable() {
		return 0, nil, linuxerr.EBADF
	}
	if uint64(msgLen) > q.MaxMessageSize() {
		return 0, nil, linuxerr.EMSGSIZE
	}
	text := make([]byte, msgLen)
	if _, err := t.CopyInBytes(msgAddr, text); err != nil {
		return 0, nil, err
	}

	var notify *mq.Subscriber
	if err := mqWait(t, file, q, waiter.WritableEvents, haveDeadline, deadline, nil, func() (err error) {
		notify, err = q.Send(string(text), priority)
		return err
	}); err != nil {
		return 0, nil, err
	}
	if notify != nil {
		mqNotify(t, notify)
	}
	return 0, nil, nil
}

// MqTimedreceive implements mq_timedreceive(2).
func MqTimedreceive(t *kernel.Task, sysno uintptr, args arch.SyscallArguments) (uintptr, *kernel.SyscallControl, error) {
	mqdes := args[0].Int()
	msgAddr := args[1].Pointer()
	msgLen := args[2].SizeT()
	priorityAddr := args[3].Pointer()
	timeoutAddr := args[4].Pointer()

	haveDeadline, deadline, err := mqDeadline(t, timeoutAddr)
	if err != nil {
		return 0, nil, err
	}
	file, q, err := mqFile(t, mqdes)
	if err != nil {
		return 0, nil, err
	}
	defer file.DecRef(t)
	if !file.IsReadable() {
		return 0, nil, linuxerr.EBADF
	}
	if uint64(msgLen) < q.MaxMessageSize() {
		return 0, nil, linuxerr.EMSGSIZE
	}

	var msg *mq.Message
	if err := mqWait(t, file, q, waiter.ReadableEvents, haveDeadline, deadline, q.WaitToReceive, func() (err error) {
		msg, err = q.Receive()
		return err
	}); err != nil {
		return 0, nil, err
	}
	// Like Linux, the message is gone even if it cannot be copied out.
	if priorityAddr != 0 {
		priority := primitive.Uint32(msg.Priority)
		if _, err := priority.CopyOut(t, priorityAddr); err != nil {
			return 0, nil, err
		}
	}
	if _, err := t.CopyOutBytes(msgAddr, []byte(msg.Text)); err != nil {
		return 0, nil, err
	}
	return uintptr(msg.Size), nil, nil
}

// MqNotify implements mq_notify(2). Of its notifications, it supports
// SIGEV_NONE and SIGEV_SIGNAL. SIGEV_THREAD, which glibc implements with a
// netlink socket that receives a cookie, fails with EINVAL.
func MqNotify(t *kernel.Task, sysno uintptr, args arch.SyscallArguments) (uintptr, *kernel.SyscallControl, error) {
	mqdes := args[0].Int()
	sevAddr := args[1].Pointer()

	var sev linux.Sigevent
	if sevAddr != 0 {
		if _, err := sev.CopyIn(t, sevAddr); err != nil {
			return 0, nil, err
		}
		switch sev.Notify {
		case linux.SIGEV_NONE:
		case linux.SIGEV_SIGNAL:
			// Like Linux, accept signal 0, which then sends none.
			if sev.Signo < 0 || sev.Signo > linux.SignalMaximum {
				return 0, nil, linuxerr.EINVAL
			}
		default:
			return 0, nil, linuxerr.EINVAL
		}
	}
	file, q, err := mqFile(t, mqdes)
	if err != nil {
		return 0, nil, err
	}
	defer file.DecRef(t)

	tg := t.ThreadGroup()
	if sevAddr == 0 {
		q.Unsubscribe(int32(tg.ID()))
		return 0, nil, nil
	}
	return 0, nil, q.Subscribe(mq.NewSubscriber(int32(tg.ID()), tg, t.UserNamespace(), sev.Notify, sev.Signo, sev.Value))
}

// MqGetsetattr implements mq_getsetattr(2).
func MqGetsetattr(t *kernel.Task, sysno uintptr, args arch.SyscallArguments) (uintptr, *kernel.SyscallControl, error) {
	mqdes := args[0].Int()
	newAddr := args[1].Pointer()
	oldAddr := args[2].Pointer()

	var newAttr linux.MqAttr
	if newAddr != 0 {
		if _, err := newAttr.CopyIn(t, newAddr); err != nil {
			return 0, nil, err
		}
		if newAttr.MqFlags&^linux.O_NONBLOCK != 0 {
			return 0, nil, linuxerr.EINVAL
		}
	}
	file, q, err := mqFile(t, mqdes)
	if err != nil {
		return 0, nil, err
	}
	defer file.DecRef(t)

	oldAttr := q.Attr()
	oldAttr.MqFlags = int64(file.StatusFlags() & linux.O_NONBLOCK)
	if newAddr != 0 {
		flags := file.StatusFlags()&^linux.O_NONBLOCK | uint32(newAttr.MqFlags)
		if err := file.SetStatusFlags(t, t.Credentials(), flags); err != nil {
			return 0, nil, err
		}
	}
	if oldAddr != 0 {
		if _, err := oldAttr.CopyOut(t, oldAddr); err != nil {
			return 0, nil, err
		}
	}
	return 0, nil, nil
}

// mqFile returns the file and the queue of the message queue descriptor
// mqdes. The caller must DecRef the file.
func mqFile(t *kernel.Task, mqdes int32) (*vfs.FileDescription, *mq.Queue, error) {
	file := t.GetFile(mqdes)
	if file == nil {
		return nil, nil, linuxerr.EBADF
	}
	q := mqfs.QueueOf(file)
	if q == nil {
		file.DecRef(t)
		return nil, nil, linuxerr.EBADF
	}
	return file, q, nil
}

// mqDeadline reads the absolute CLOCK_REALTIME timeout at addr, if addr is
// not null.
func mqDeadline(t *kernel.Task, addr hostarch.Addr) (bool, ktime.Time, error) {
	if addr == 0 {
		return false, ktime.Time{}, nil
	}
	var ts linux.Timespec
	if _, err := ts.CopyIn(t, addr); err != nil {
		return false, ktime.Time{}, err
	}
	// Like Linux, refuse an invalid timeout even if the call need not wait.
	if !ts.Valid() {
		return false, ktime.Time{}, linuxerr.EINVAL
	}
	return true, ktime.FromTimespec(ts), nil
}

// mqWait calls try until it no longer fails with linuxerr.ErrWouldBlock: if
// file is non-blocking, only once, failing with EAGAIN; otherwise whenever q
// is ready for mask, until deadline, if haveDeadline, when it fails with
// ETIMEDOUT. While it waits, it counts itself as waiting, if waiting is not
// nil. Like Linux, a wait that a signal interrupts restarts, unless a
// handler without SA_RESTART runs.
func mqWait(t *kernel.Task, file *vfs.FileDescription, q *mq.Queue, mask waiter.EventMask, haveDeadline bool, deadline ktime.Time, waiting func() func(), try func() error) error {
	if err := try(); !linuxerr.Equals(linuxerr.ErrWouldBlock, err) {
		return err
	}
	if file.StatusFlags()&linux.O_NONBLOCK != 0 {
		return linuxerr.EAGAIN
	}
	if waiting != nil {
		defer waiting()()
	}
	e, ch := waiter.NewChannelEntry(mask)
	if err := q.EventRegister(&e); err != nil {
		return err
	}
	defer q.EventUnregister(&e)
	for {
		if err := try(); !linuxerr.Equals(linuxerr.ErrWouldBlock, err) {
			return err
		}
		if err := t.BlockWithDeadlineFrom(ch, t.Kernel().RealtimeClock(), haveDeadline, deadline); err != nil {
			if linuxerr.Equals(linuxerr.ErrInterrupted, err) {
				return linuxerr.ERESTARTSYS
			}
			return err
		}
	}
}

// mqNotify sends sub's notification of a message that t sent.
func mqNotify(t *kernel.Task, sub *mq.Subscriber) {
	target, userNS, method, signal, value := sub.Notification()
	tg, ok := target.(*kernel.ThreadGroup)
	if !ok || method != linux.SIGEV_SIGNAL || signal == 0 {
		return
	}
	info := &linux.SignalInfo{Signo: signal, Code: linux.SI_MESGQ}
	info.SetPID(int32(tg.PIDNamespace().IDOfThreadGroup(t.ThreadGroup())))
	info.SetUID(int32(t.Credentials().RealKUID.In(userNS).OrOverflow()))
	info.SetSigval(value)
	// A thread group that has exited meanwhile gets no signal.
	_ = tg.SendSignal(info)
}
