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

// Package mq provides an implementation for POSIX message queues.
package mq

import (
	"bytes"
	"fmt"
	"strings"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/waiter"
)

// AccessType is the access type passed to mq_open.
type AccessType int

// Possible access types.
const (
	ReadOnly AccessType = iota
	WriteOnly
	ReadWrite
)

// MaxName is the maximum size for a queue name.
const MaxName = 255

const (
	maxPriority = linux.MQ_PRIO_MAX - 1 // Highest possible message priority.

	maxQueuesDefault = linux.DFLT_QUEUESMAX // Default max number of queues.

	maxMsgDefault   = linux.DFLT_MSG    // Default max number of messages per queue.
	maxMsgMin       = linux.MIN_MSGMAX  // Min value for max number of messages per queue.
	maxMsgLimit     = linux.DFLT_MSGMAX // Limit for max number of messages per queue.
	maxMsgHardLimit = linux.HARD_MSGMAX // Hard limit for max number of messages per queue.

	msgSizeDefault   = linux.DFLT_MSGSIZE    // Default max message size.
	msgSizeMin       = linux.MIN_MSGSIZEMAX  // Min value for max message size.
	msgSizeLimit     = linux.DFLT_MSGSIZEMAX // Limit for max message size.
	msgSizeHardLimit = linux.HARD_MSGSIZEMAX // Hard limit for max message size.
)

// Registry is a POSIX message queue registry.
//
// Unlike SysV utilities, Registry is not map-based. It uses a provided
// RegistryImpl backed by a virtual filesystem to implement registry operations.
//
// +stateify savable
type Registry struct {
	// userNS is the user namespace containing this registry. Immutable.
	userNS *auth.UserNamespace

	// mu protects all fields below.
	mu sync.Mutex `state:"nosave"`

	// impl is an implementation of several message queue utilities needed by
	// the registry. impl should be provided by mqfs.
	impl RegistryImpl
}

// RegistryImpl defines utilities needed by a Registry to provide actual
// registry implementation. It works mainly as an abstraction layer used by
// Registry to avoid dealing directly with the filesystem. RegistryImpl should
// be implemented by mqfs and provided to Registry at initialization.
type RegistryImpl interface {
	// Get searches for a queue with the given name, if it exists, the queue is
	// used to create a new FD, return it and return true. If the queue  doesn't
	// exist, return false and no error. An error is returned if creation fails.
	Get(ctx context.Context, name string, access AccessType, block bool, flags uint32) (*vfs.FileDescription, bool, error)

	// New creates a new inode and file description using the given queue,
	// inserts the inode into the filesystem tree using the given name, and
	// returns the file description. An error is returned if creation fails, or
	// if the name already exists.
	New(ctx context.Context, name string, q *Queue, access AccessType, block bool, perm linux.FileMode, flags uint32) (*vfs.FileDescription, error)

	// Unlink removes the queue with given name from the registry, and returns
	// an error if the name doesn't exist.
	Unlink(ctx context.Context, name string) error

	// Destroy destroys the registry.
	Destroy(context.Context)
}

// NewRegistry returns a new, initialized message queue registry. NewRegistry
// should be called when a new message queue filesystem is created, once per
// IPCNamespace.
func NewRegistry(userNS *auth.UserNamespace, impl RegistryImpl) *Registry {
	return &Registry{
		userNS: userNS,
		impl:   impl,
	}
}

// OpenOpts holds the options passed to FindOrCreate.
type OpenOpts struct {
	Name      string
	Access    AccessType
	Create    bool
	Exclusive bool
	Block     bool
}

// FindOrCreate creates a new POSIX message queue or opens an existing queue.
// See mq_open(2).
func (r *Registry) FindOrCreate(ctx context.Context, opts OpenOpts, mode linux.FileMode, attr *linux.MqAttr) (*vfs.FileDescription, error) {
	// mq_overview(7) mentions that: "Each message queue is identified by a name
	// of the form '/somename'", but the mq_open(3) man pages mention:
	//   "The mq_open() library function is implemented on top of a system call
	//    of the same name.  The library function performs the check that the
	//    name starts with a slash (/), giving the EINVAL error if it does not.
	//    The kernel system call expects name to contain no preceding slash, so
	//    the C library function passes name without the preceding slash (i.e.,
	//    name+1) to the system call."
	// So we don't need to check it.

	if len(opts.Name) == 0 {
		return nil, linuxerr.ENOENT
	}
	if len(opts.Name) > MaxName {
		return nil, linuxerr.ENAMETOOLONG
	}
	if strings.ContainsRune(opts.Name, '/') {
		return nil, linuxerr.EACCES
	}
	if opts.Name == "." || opts.Name == ".." {
		return nil, linuxerr.EINVAL
	}

	// Construct status flags.
	var flags uint32
	if !opts.Block {
		flags = linux.O_NONBLOCK
	}
	switch opts.Access {
	case ReadOnly:
		flags = flags | linux.O_RDONLY
	case WriteOnly:
		flags = flags | linux.O_WRONLY
	case ReadWrite:
		flags = flags | linux.O_RDWR
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	fd, ok, err := r.impl.Get(ctx, opts.Name, opts.Access, opts.Block, flags)
	if err != nil {
		return nil, err
	}

	if ok {
		if opts.Create && opts.Exclusive {
			// "Both O_CREAT and O_EXCL were specified in oflag, but a queue
			//  with this name already exists."
			fd.DecRef(ctx)
			return nil, linuxerr.EEXIST
		}
		return fd, nil
	}

	if !opts.Create {
		// "The O_CREAT flag was not specified in oflag, and no queue with this name
		//  exists."
		return nil, linuxerr.ENOENT
	}

	q, err := r.newQueueLocked(auth.CredentialsFromContext(ctx), attr)
	if err != nil {
		return nil, err
	}
	return r.impl.New(ctx, opts.Name, q, opts.Access, opts.Block, mode.Permissions(), flags)
}

// newQueueLocked creates a new queue using the given attributes. If attr is nil
// return a queue with default values, otherwise use attr to create a new queue,
// and return an error if attributes are invalid.
func (r *Registry) newQueueLocked(creds *auth.Credentials, attr *linux.MqAttr) (*Queue, error) {
	if attr == nil {
		return &Queue{
			maxMessageCount: int64(maxMsgDefault),
			maxMessageSize:  uint64(msgSizeDefault),
		}, nil
	}

	// "O_CREAT was specified in oflag, and attr was not NULL, but
	//  attr->mq_maxmsg or attr->mq_msqsize was invalid.  Both of these fields
	//  these fields must be greater than zero.  In a process that is
	//  unprivileged (does not have the CAP_SYS_RESOURCE capability),
	//  attr->mq_maxmsg must be less than or equal to the msg_max limit, and
	//  attr->mq_msgsize must be less than or equal to the msgsize_max limit.
	//  In addition, even in a privileged process, attr->mq_maxmsg cannot
	//  exceed the HARD_MAX limit." - man mq_open(3).
	if attr.MqMaxmsg <= 0 || attr.MqMsgsize <= 0 {
		return nil, linuxerr.EINVAL
	}

	if attr.MqMaxmsg > maxMsgHardLimit || (!creds.HasCapabilityIn(linux.CAP_SYS_RESOURCE, r.userNS) && (attr.MqMaxmsg > maxMsgLimit || attr.MqMsgsize > msgSizeLimit)) {
		return nil, linuxerr.EINVAL
	}

	return &Queue{
		maxMessageCount: attr.MqMaxmsg,
		maxMessageSize:  uint64(attr.MqMsgsize),
	}, nil
}

// Remove removes the queue with the given name from the registry. See
// mq_unlink(2).
func (r *Registry) Remove(ctx context.Context, name string) error {
	if len(name) > MaxName {
		return linuxerr.ENAMETOOLONG
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	return r.impl.Unlink(ctx, name)
}

// Destroy destroys the registry and releases all held references.
func (r *Registry) Destroy(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.impl.Destroy(ctx)
}

// Impl returns RegistryImpl inside r.
func (r *Registry) Impl() RegistryImpl {
	return r.impl
}

// Queue represents a POSIX message queue.
//
// +stateify savable
type Queue struct {
	// mu protects all the fields below.
	mu sync.Mutex `state:"nosave"`

	// queue is the queue of waiters.
	queue waiter.Queue

	// messages is a list of messages currently in the queue.
	messages msgList

	// subscriber represents a task registered to receive async notification
	// from this queue.
	subscriber *Subscriber

	// messageCount is the number of messages currently in the queue.
	messageCount int64

	// maxMessageCount is the maximum number of messages that the queue can
	// hold.
	maxMessageCount int64

	// maxMessageSize is the maximum size of a message held by the queue.
	maxMessageSize uint64

	// byteCount is the number of bytes of data in all messages in the queue.
	byteCount uint64

	// receivers is the number of receivers waiting for a message. A task
	// waiting in a checkpoint restarts its wait, and counts itself again.
	receivers int64 `state:"nosave"`
}

// View is a view into a message queue. Views should only be used in file
// descriptions, but not inodes, because we use inodes to retrieve the actual
// queue, and only FDs are responsible for providing user functionality.
type View interface {
	// TODO: Add Send and Receive when mq_timedsend(2) and mq_timedreceive(2)
	// are implemented.

	// Flush checks if the calling process has attached a notification request
	// to this queue, if yes, then the request is removed, and another process
	// can attach a request.
	Flush(ctx context.Context)

	waiter.Waitable
}

// ReaderWriter provides a send and receive view into a queue.
//
// +stateify savable
type ReaderWriter struct {
	*Queue

	block bool
}

// Reader provides a send-only view into a queue.
//
// +stateify savable
type Reader struct {
	*Queue

	block bool
}

// Writer provides a receive-only view into a queue.
//
// +stateify savable
type Writer struct {
	*Queue

	block bool
}

// NewView creates a new view into a queue and returns it.
func NewView(q *Queue, access AccessType, block bool) (View, error) {
	switch access {
	case ReadWrite:
		return ReaderWriter{Queue: q, block: block}, nil
	case WriteOnly:
		return Writer{Queue: q, block: block}, nil
	case ReadOnly:
		return Reader{Queue: q, block: block}, nil
	default:
		// This case can't happen, due to O_RDONLY flag being 0 and O_WRONLY
		// being 1, so one of them must be true.
		return nil, linuxerr.EINVAL
	}
}

// Message holds a message exchanged through a Queue via mq_timedsend(2) and
// mq_timedreceive(2), and additional info relating to the message.
//
// +stateify savable
type Message struct {
	msgEntry

	// Text is the message's sent content.
	Text string

	// Size is the message's size in bytes.
	Size uint64

	// Priority is the message's priority.
	Priority uint32
}

// Subscriber represents a task registered for async notification from a Queue.
//
// +stateify savable
type Subscriber struct {
	// pid is the PID of the registered task.
	pid int32

	// target is the thread group that the notification signals.
	target NotificationTarget

	// userNS is the user namespace of the registered task.
	userNS *auth.UserNamespace

	// method is how the subscriber is notified: linux.SIGEV_NONE or
	// linux.SIGEV_SIGNAL.
	method int32

	// signal and value are the number and the value of the signal of a
	// linux.SIGEV_SIGNAL notification.
	signal int32
	value  uint64
}

// NotificationTarget is the thread group that a Subscriber notifies, a
// *kernel.ThreadGroup.
type NotificationTarget interface {
	SendSignal(info *linux.SignalInfo) error
}

// NewSubscriber returns a Subscriber for the thread group with ID pid,
// target, in user namespace userNS, to be notified by method; for
// linux.SIGEV_SIGNAL, with the given signal and value.
func NewSubscriber(pid int32, target NotificationTarget, userNS *auth.UserNamespace, method, signal int32, value uint64) *Subscriber {
	return &Subscriber{pid: pid, target: target, userNS: userNS, method: method, signal: signal, value: value}
}

// Notification returns how s is notified: by method, and for
// linux.SIGEV_SIGNAL, with a signal of the given number and value sent to
// target, in user namespace userNS.
func (s *Subscriber) Notification() (target NotificationTarget, userNS *auth.UserNamespace, method, signal int32, value uint64) {
	return s.target, s.userNS, s.method, s.signal, s.value
}

// Generate implements vfs.DynamicBytesSource.Generate. Queue is used as a
// DynamicBytesSource for mqfs's queueInode.
func (q *Queue) Generate(ctx context.Context, buf *bytes.Buffer) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	var (
		pid       int32
		method    int
		sigNumber int
	)
	if q.subscriber != nil {
		pid = q.subscriber.pid
		method = int(q.subscriber.method)
		if q.subscriber.method == linux.SIGEV_SIGNAL {
			sigNumber = int(q.subscriber.signal)
		}
	}

	fmt.Fprintf(buf, "QSIZE:%-10d NOTIFY:%-5d SIGNO:%-5d NOTIFY_PID:%-6d\n",
		q.byteCount, method, sigNumber, pid)
	return nil
}

// Flush implements View.Flush.
func (q *Queue) Flush(ctx context.Context) {
	q.mu.Lock()
	defer q.mu.Unlock()

	pid, ok := auth.ThreadGroupIDFromContext(ctx)
	if ok {
		if q.subscriber != nil && pid == q.subscriber.pid {
			q.subscriber = nil
		}
	}
}

// Readiness implements Waitable.Readiness.
func (q *Queue) Readiness(mask waiter.EventMask) waiter.EventMask {
	q.mu.Lock()
	defer q.mu.Unlock()

	events := waiter.EventMask(0)
	if q.messageCount > 0 {
		events |= waiter.ReadableEvents
	}
	if q.messageCount < q.maxMessageCount {
		events |= waiter.WritableEvents
	}
	return events & mask
}

// EventRegister implements Waitable.EventRegister.
func (q *Queue) EventRegister(e *waiter.Entry) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.queue.EventRegister(e)
	return nil
}

// EventUnregister implements Waitable.EventUnregister.
func (q *Queue) EventUnregister(e *waiter.Entry) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.queue.EventUnregister(e)
}

// MaxMessageSize returns the size of the largest message that q holds.
func (q *Queue) MaxMessageSize() uint64 {
	return q.maxMessageSize
}

// Attr returns q's attributes, for mq_getsetattr(2), but its flags, which are
// the file description's.
func (q *Queue) Attr() linux.MqAttr {
	q.mu.Lock()
	defer q.mu.Unlock()
	return linux.MqAttr{
		MqMaxmsg:  q.maxMessageCount,
		MqMsgsize: int64(q.maxMessageSize),
		MqCurmsgs: q.messageCount,
	}
}

// Send adds a message of text with priority to q, unless q is full, when it
// fails with linuxerr.ErrWouldBlock. Like Linux, it ends q's notification
// registration and returns it, for the caller to notify, when the message
// makes q nonempty and no receiver is waiting.
//
// Preconditions: len(text) <= q.MaxMessageSize(); priority <
// linux.MQ_PRIO_MAX.
func (q *Queue) Send(text string, priority uint32) (*Subscriber, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.messageCount >= q.maxMessageCount {
		return nil, linuxerr.ErrWouldBlock
	}
	msg := &Message{Text: text, Size: uint64(len(text)), Priority: priority}
	// Messages are ordered by priority, and by their arrival within one.
	at := q.messages.Back()
	for at != nil && at.Priority < priority {
		at = at.Prev()
	}
	if at == nil {
		q.messages.PushFront(msg)
	} else {
		q.messages.InsertAfter(at, msg)
	}
	q.messageCount++
	q.byteCount += msg.Size
	var notify *Subscriber
	if q.messageCount == 1 && q.receivers == 0 {
		notify, q.subscriber = q.subscriber, nil
	}
	q.queue.Notify(waiter.ReadableEvents)
	return notify, nil
}

// Receive removes and returns q's message of the highest priority that came
// first, unless q is empty, when it fails with linuxerr.ErrWouldBlock.
func (q *Queue) Receive() (*Message, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	msg := q.messages.Front()
	if msg == nil {
		return nil, linuxerr.ErrWouldBlock
	}
	q.messages.Remove(msg)
	q.messageCount--
	q.byteCount -= msg.Size
	q.queue.Notify(waiter.WritableEvents)
	return msg, nil
}

// WaitToReceive counts a receiver waiting for a message from q, until the
// function that it returns is called: a message sent meanwhile is for a
// receiver, so it notifies no subscriber.
func (q *Queue) WaitToReceive() func() {
	q.mu.Lock()
	q.receivers++
	q.mu.Unlock()
	return func() {
		q.mu.Lock()
		q.receivers--
		q.mu.Unlock()
	}
}

// Subscribe registers sub for q's notification, unless one is registered
// already, when it fails with EBUSY.
func (q *Queue) Subscribe(sub *Subscriber) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.subscriber != nil {
		return linuxerr.EBUSY
	}
	q.subscriber = sub
	return nil
}

// Unsubscribe ends the notification registration of the thread group with ID
// pid, if it has q's.
func (q *Queue) Unsubscribe(pid int32) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.subscriber != nil && q.subscriber.pid == pid {
		q.subscriber = nil
	}
}
