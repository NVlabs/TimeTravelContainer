// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package binder implements Android's binder IPC, as Linux's
// drivers/android/binder.c does: a device through which processes send each
// other transactions, which carry data and references to objects of the
// processes' (nodes), and files. Binder devices live in binderfs
// (pkg/sentry/fsimpl/binderfs).
//
// A Device is a binder context, whose context manager, the process that
// handle 0 refers to, is Android's servicemanager. A process that opens the
// device gets a proc; each of its threads that calls into the driver, a
// thread. A proc owns the nodes of its objects, and holds refs to other
// procs' nodes by handle. A transaction's data is copied into a buffer of
// its target proc, in memory that the target maps read-only, and whose
// objects are translated for the target.
//
// The state of a Device is protected by its mutex; waiting threads block
// outside of it. Lists of work are slices, and maps are keyed by numbers,
// so that the state can be saved.
package binder

import (
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/waiter"
)

// Device is a binder device, a context of its own.
//
// +stateify savable
type Device struct {
	// name is the device's name, such as "binder".
	name string

	mu sync.Mutex `state:"nosave"`

	// mgr is the context manager's node, which handle 0 refers to, or nil.
	mgr *node
	// mgrUID is the EUID of the first context manager, the only one that may
	// be context manager again, or nil.
	mgrUID *auth.KUID

	// nextID numbers nodes, refs, and transactions, for debugging and keys.
	nextID uint64
}

// NewDevice returns a binder device named name.
func NewDevice(name string) *Device {
	return &Device{name: name}
}

// Name returns the device's name.
func (d *Device) Name() string {
	return d.name
}

// Preconditions: d.mu is locked.
func (d *Device) newID() uint64 {
	d.nextID++
	return d.nextID
}

// workType is the kind of a work item.
type workType uint8

const (
	// workTransaction delivers a transaction.
	workTransaction workType = iota + 1
	// workTransactionComplete tells a sender that its transaction is sent.
	workTransactionComplete
	// workReturnError tells a sender that its transaction failed.
	workReturnError
	// workNode tells a node's owner that the node's references changed.
	workNode
	// workDeadBinder tells a proc that a node it refers to died.
	workDeadBinder
	// workDeadBinderAndClear is workDeadBinder, whose notification the proc
	// cleared since.
	workDeadBinderAndClear
	// workClearDeathNotification tells a proc that it cleared a death
	// notification.
	workClearDeathNotification
)

// work is an item of work for a thread or proc.
//
// +stateify savable
type work struct {
	typ workType

	// list is the list the item is on, or nil.
	list *workList

	// The item's subject, according to typ.
	transaction *transaction
	node        *node
	death       *death
	// cmd is the BR_* of workReturnError.
	cmd uint32
}

// workList is a FIFO list of work.
//
// +stateify savable
type workList struct {
	items []*work
}

func (l *workList) empty() bool {
	return len(l.items) == 0
}

// push adds w to the end of l.
func (l *workList) push(w *work) {
	if w.list != nil {
		panic("binder: work queued twice")
	}
	w.list = l
	l.items = append(l.items, w)
}

// pop removes and returns the first item of l.
func (l *workList) pop() *work {
	w := l.items[0]
	l.items[0] = nil
	l.items = l.items[1:]
	w.list = nil
	return w
}

// remove removes w from the list it is on, if any.
func (w *work) remove() {
	l := w.list
	if l == nil {
		return
	}
	for i, item := range l.items {
		if item == w {
			l.items = append(l.items[:i], l.items[i+1:]...)
			break
		}
	}
	w.list = nil
}

// proc is a process' open binder file.
//
// +stateify savable
type proc struct {
	dev *Device

	// tg is the process that opened the device.
	tg *kernel.ThreadGroup
	// creds are its credentials when it opened the device.
	creds *auth.Credentials

	// dead is whether the file is released.
	dead bool

	threads map[kernel.ThreadID]*thread
	// nodes are the proc's nodes, by their pointers.
	nodes map[uint64]*node
	// refs are the proc's refs, by their handles; refsByNode, by their
	// nodes' IDs.
	refs       map[uint32]*ref
	refsByNode map[uint64]*ref

	// todo is the proc's work, for any of its looper threads.
	todo workList
	// waiting are the proc's looper threads that wait for its work.
	waiting []*thread

	maxThreads              uint32
	requestedThreads        uint32
	requestedThreadsStarted uint32

	// delivered are the death notifications delivered and not yet done.
	delivered workList

	// pollQueue is notified of work for pollers.
	pollQueue waiter.Queue

	// The buffer mapping. mapMu protects size, fr, and mappings, which
	// Translate reads under memory manager locks; they change under both
	// the device's mutex and mapMu, which nests inside it. The driver
	// touches no user memory under the device's mutex.
	mapMu sync.Mutex `state:"nosave"`
	// mapped is whether the proc maps its buffer.
	mapped bool
	// base is the mapping's address.
	base uint64
	// size is the mapping's size.
	size uint64
	// fr is the mapping's memory, and mf its MemoryFile. They are not saved:
	// each branch of a live fork writes its own transactions, so a restored
	// proc allocates new memory on first use (see memoryLocked).
	fr memmap.FileRange    `state:"nosave"`
	mf *pgalloc.MemoryFile `state:"nosave"`
	// mappings tracks the proc's mappings of its buffer.
	mappings memmap.MappingSet
	// buffers are the allocated buffers, by offset in the mapping.
	buffers []*buffer
	// freeAsyncSpace is how much the proc's oneway transactions may still
	// take.
	freeAsyncSpace uint64
}

// thread is a thread of a proc.
//
// +stateify savable
type thread struct {
	proc *proc
	tid  kernel.ThreadID

	looper uint32
	// needReturn is whether the thread's read returns at once, as it does
	// on the thread's first call.
	needReturn bool
	// stack is the thread's transaction stack: the transactions it sent and
	// waits for replies to, and those it received and owes replies to.
	stack *transaction
	todo  workList
	// processTodo is whether todo has work that th processes: work queued
	// deferred waits for other work.
	processTodo bool
	// returnError is the thread's pending failure of its transaction.
	returnError work
	// replyError is the thread's pending failure of a transaction it sent.
	replyError work
	// waitingForProc is whether the thread is on its proc's waiting list.
	waitingForProc bool
	// queue is notified of the thread's work.
	queue waiter.Queue

	dead bool
}

// node is an object of a proc's, which other procs refer to.
//
// +stateify savable
type node struct {
	id uint64
	// proc is the node's owner, or nil once it died.
	proc   *proc
	ptr    uint64
	cookie uint64

	// internalStrongRefs is how many refs hold the node strongly.
	internalStrongRefs int
	// localStrongRefs and localWeakRefs are the driver's references, from
	// transactions and buffers.
	localStrongRefs int
	localWeakRefs   int
	// refs are the node's refs, by their IDs.
	refs map[uint64]*ref

	// hasStrongRef and hasWeakRef are whether the owner holds references
	// for the driver; pendingStrongRef and pendingWeakRef, whether the
	// owner has yet to acknowledge them.
	hasStrongRef     bool
	hasWeakRef       bool
	pendingStrongRef bool
	pendingWeakRef   bool

	// hasAsyncTransaction is whether a oneway transaction to the node is
	// delivered and its buffer not yet freed; asyncTodo are those that wait
	// for it.
	hasAsyncTransaction bool
	asyncTodo           workList

	acceptFDs      bool
	txnSecurityCtx bool

	// work is the node's workNode item.
	work work
}

// ref is a proc's reference to a node, by handle.
//
// +stateify savable
type ref struct {
	id     uint64
	proc   *proc
	node   *node
	handle uint32
	strong int
	weak   int
	death  *death
}

// death is a death notification of a ref's node.
//
// +stateify savable
type death struct {
	work   work
	cookie uint64
	// proc is the proc to notify.
	proc *proc
}

// transaction is a transaction or reply in flight.
//
// +stateify savable
type transaction struct {
	id   uint64
	work work

	// from is the sending thread, for transactions that expect replies, or
	// nil.
	from *thread
	// fromTG is the sending process.
	fromTG *kernel.ThreadGroup
	// senderEUID is the sending proc's EUID.
	senderEUID auth.KUID
	// fromParent is the transaction that the sender was handling.
	fromParent *transaction
	toProc     *proc
	toThread   *thread
	// toParent is the transaction that the receiver was handling.
	toParent *transaction

	needReply bool
	isReply   bool
	buffer    *buffer
	code      uint32
	flags     uint32

	// fds are the files to install in the receiver, and where their
	// numbers go in the buffer.
	fds []fdFixup
	// secctx is the offset of the security context in the buffer, if
	// secctxLen is not 0.
	secctx    uint64
	secctxLen uint64
}

// fdFixup is a file that a transaction carries.
//
// +stateify savable
type fdFixup struct {
	file *vfs.FileDescription
	// offset is where in the buffer the file's number goes.
	offset uint64
}

// buffer is a transaction's data in its target proc's mapping.
//
// +stateify savable
type buffer struct {
	// offset is the buffer's offset in the mapping, and size its size.
	offset uint64
	size   uint64

	dataSize    uint64
	offsetsSize uint64
	extraSize   uint64

	transaction *transaction
	// targetNode is the node that the transaction was sent to, which the
	// buffer holds strongly, or nil for replies.
	targetNode *node
	async      bool
	// userFree is whether the proc may free the buffer.
	userFree bool
	// contents is the buffer's data while the proc's state is saved, until
	// the restored proc allocates its memory.
	contents []byte
}
