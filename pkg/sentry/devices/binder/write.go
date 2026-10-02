// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package binder

import (
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/waiter"
)

// write processes the commands of bwr's write buffer, from bwr.writeConsumed
// on, as Linux's binder_thread_write does. It stops early while th has a
// failure to report.
func (th *thread) write(t *kernel.Task, bwr *writeRead) error {
	d := th.proc.dev
	for bwr.writeConsumed < bwr.writeSize {
		d.mu.Lock()
		pending := th.returnError.cmd != 0
		d.mu.Unlock()
		if pending {
			return nil
		}
		addr := hostarch.Addr(bwr.writeBuffer + bwr.writeConsumed)
		var c [4]byte
		if _, err := t.CopyInBytes(addr, c[:]); err != nil {
			return err
		}
		cmd := le.Uint32(c[:])
		payload := make([]byte, iocSize(cmd))
		if len(payload) != 0 {
			if _, err := t.CopyInBytes(addr+4, payload); err != nil {
				return err
			}
		}
		if err := th.command(t, cmd, payload); err != nil {
			return err
		}
		bwr.writeConsumed += 4 + uint64(len(payload))
	}
	return nil
}

// command processes a command.
func (th *thread) command(t *kernel.Task, cmd uint32, payload []byte) error {
	p := th.proc
	d := p.dev
	switch cmd {
	case bcTransaction, bcReply:
		var tr transactionData
		tr.decode(payload)
		th.transaction(t, &tr, cmd == bcReply, 0)
		return nil

	case bcTransactionSG, bcReplySG:
		var tr transactionData
		tr.decode(payload)
		th.transaction(t, &tr, cmd == bcReplySG, le.Uint64(payload[sizeofTransactionData:]))
		return nil
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	switch cmd {
	case bcIncrefs, bcAcquire, bcRelease, bcDecrefs:
		handle := le.Uint32(payload)
		strong := cmd == bcAcquire || cmd == bcRelease
		increment := cmd == bcIncrefs || cmd == bcAcquire
		if increment && handle == 0 && d.mgr != nil {
			if d.mgr.proc == p {
				p.logf("the context manager refers to handle 0")
				return linuxerr.EINVAL
			}
			if r, err := p.incRefForNode(d.mgr, strong, nil); err == nil {
				if r.handle != 0 {
					p.logf("the context manager's handle is %d, not 0", r.handle)
				}
				return nil
			}
		}
		r := p.getRef(handle, strong)
		if r == nil {
			p.logf("refcount change on invalid handle %d", handle)
			return nil
		}
		var err error
		if increment {
			err = r.inc(strong, nil)
		} else {
			err = r.dec(strong)
		}
		if err != nil {
			p.logf("refcount change on handle %d: %v", handle, err)
		}

	case bcIncrefsDone, bcAcquireDone:
		ptr, cookie := le.Uint64(payload[0:]), le.Uint64(payload[8:])
		n := p.nodes[ptr]
		if n == nil || n.cookie != cookie {
			p.logf("refcount acknowledgment of invalid node %#x", ptr)
			return nil
		}
		strong := cmd == bcAcquireDone
		if strong {
			if !n.pendingStrongRef {
				return nil
			}
			n.pendingStrongRef = false
		} else {
			if !n.pendingWeakRef {
				return nil
			}
			n.pendingWeakRef = false
		}
		n.dec(strong, false)

	case bcFreeBuffer:
		b := p.bufferAt(le.Uint64(payload))
		if b == nil || !b.userFree {
			p.logf("free of invalid buffer %#x", le.Uint64(payload))
			return nil
		}
		p.freeBuf(t, b, false)

	case bcRegisterLooper:
		if th.looper&looperEntered != 0 {
			th.looper |= looperInvalid
		} else if p.requestedThreads == 0 {
			th.looper |= looperInvalid
		} else {
			p.requestedThreads--
			p.requestedThreadsStarted++
		}
		th.looper |= looperRegistered

	case bcEnterLooper:
		if th.looper&looperRegistered != 0 {
			th.looper |= looperInvalid
		}
		th.looper |= looperEntered

	case bcExitLooper:
		th.looper |= looperExited

	case bcRequestDeathNotification, bcClearDeathNotification:
		handle, cookie := le.Uint32(payload[0:]), le.Uint64(payload[4:])
		r := p.getRef(handle, false)
		if r == nil {
			p.logf("death notification of invalid handle %d", handle)
			return nil
		}
		if cmd == bcRequestDeathNotification {
			if r.death != nil {
				return nil
			}
			r.death = &death{cookie: cookie, proc: p}
			r.death.work.death = r.death
			if r.node.proc == nil {
				r.death.work.typ = workDeadBinder
				p.todo.push(&r.death.work)
				p.wakeup()
			}
			return nil
		}
		dth := r.death
		if dth == nil || dth.cookie != cookie {
			return nil
		}
		r.death = nil
		if dth.work.list == nil {
			dth.work.typ = workClearDeathNotification
			th.enqueueDeath(&dth.work)
		} else {
			dth.work.typ = workDeadBinderAndClear
		}

	case bcDeadBinderDone:
		cookie := le.Uint64(payload)
		var dth *death
		for _, w := range p.delivered.items {
			if w.death.cookie == cookie {
				dth = w.death
				break
			}
		}
		if dth == nil {
			return nil
		}
		dth.work.remove()
		if dth.work.typ == workDeadBinderAndClear {
			dth.work.typ = workClearDeathNotification
			th.enqueueDeath(&dth.work)
		}

	default:
		// Includes BC_ATTEMPT_ACQUIRE and BC_ACQUIRE_RESULT, which Linux
		// does not support either, and freeze notifications.
		p.logf("unknown command %#x", cmd)
		return linuxerr.EINVAL
	}
	return nil
}

// enqueueDeath queues a death notification's work for th, or, if th is no
// looper, for its proc.
//
// Preconditions: the device's mutex is locked.
func (th *thread) enqueueDeath(w *work) {
	if th.looper&(looperRegistered|looperEntered) != 0 {
		th.enqueue(w)
		return
	}
	th.proc.todo.push(w)
	th.proc.wakeup()
}

// object is an object of a transaction's data.
type object struct {
	// offset is the object's offset in the data.
	offset uint64
	typ    uint32
	// For binder_buffer_objects: the length of their buffers, and where
	// they go in the transaction's buffer.
	length uint64
	sgOff  uint64
}

// objectSize returns the size of an object of type typ, or 0 for an invalid
// type.
func objectSize(typ uint32) uint64 {
	switch typ {
	case typeBinder, typeWeakBinder, typeHandle, typeWeakHandle:
		return sizeofFlatObject
	case typeFD:
		return sizeofFDObject
	case typePtr:
		return sizeofBufferObject
	case typeFDA:
		return sizeofFDArrayObject
	}
	return 0
}

// parseObjects returns the objects of data at offsets, validated as Linux's
// binder_get_object does, and the bytes of their buffers, which it copies
// in from t.
func parseObjects(t *kernel.Task, data, offsets []byte, extraSize uint64) ([]object, [][]byte, bool) {
	var objs []object
	var sg [][]byte
	var minOff, sgUsed uint64
	for i := 0; i+8 <= len(offsets); i += 8 {
		off := le.Uint64(offsets[i:])
		if off%4 != 0 || off < minOff || off+4 > uint64(len(data)) || off+4 < off {
			return nil, nil, false
		}
		typ := le.Uint32(data[off:])
		size := objectSize(typ)
		if size == 0 || off+size > uint64(len(data)) {
			return nil, nil, false
		}
		o := object{offset: off, typ: typ}
		if typ == typePtr {
			length := le.Uint64(data[off+16:])
			if align8(length) < length || sgUsed+align8(length) > extraSize {
				return nil, nil, false
			}
			buf := make([]byte, length)
			if length != 0 {
				if _, err := t.CopyInBytes(hostarch.Addr(le.Uint64(data[off+8:])), buf); err != nil {
					return nil, nil, false
				}
			}
			o.length = length
			sg = append(sg, buf)
			sgUsed += align8(length)
		}
		objs = append(objs, o)
		minOff = off + size
	}
	return objs, sg, true
}

// transaction sends a transaction or reply, as Linux's binder_transaction
// does. Failures are reported to th by its next read.
func (th *thread) transaction(t *kernel.Task, tr *transactionData, reply bool, extraSize uint64) {
	p := th.proc
	d := p.dev

	// Copy the data in before taking the device's mutex, under which the
	// driver touches no user memory.
	valid := tr.offsetsSize%8 == 0 && extraSize%8 == 0 &&
		tr.dataSize <= maxBufferSize && tr.offsetsSize <= maxBufferSize && extraSize <= maxBufferSize
	var data, offsets []byte
	var objs []object
	var sg [][]byte
	if valid {
		data = make([]byte, tr.dataSize)
		offsets = make([]byte, tr.offsetsSize)
		if _, err := t.CopyInBytes(hostarch.Addr(tr.buffer), data); err != nil && tr.dataSize != 0 {
			valid = false
		} else if _, err := t.CopyInBytes(hostarch.Addr(tr.offsets), offsets); err != nil && tr.offsetsSize != 0 {
			valid = false
		} else {
			objs, sg, valid = parseObjects(t, data, offsets, extraSize)
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	var inReplyTo *transaction
	var targetThread *thread
	var targetProc *proc
	var targetNode *node

	// fail reports a failure, of a transaction before it has a buffer.
	fail := func(cmd uint32) {
		if targetNode != nil {
			targetNode.dec(true, false)
		}
		if inReplyTo != nil {
			th.returnError.cmd = brTransactionComplete
			th.enqueue(&th.returnError)
			sendFailedReply(t, inReplyTo, cmd)
			return
		}
		th.returnError.cmd = cmd
		th.enqueue(&th.returnError)
	}

	if reply {
		inReplyTo = th.stack
		if inReplyTo == nil || inReplyTo.toThread != th {
			p.logf("reply without a transaction to reply to")
			inReplyTo = nil
			fail(brFailedReply)
			return
		}
		th.stack = inReplyTo.toParent
		targetThread = inReplyTo.from
		if targetThread == nil {
			fail(brDeadReply)
			return
		}
		if targetThread.stack != inReplyTo {
			fail(brFailedReply)
			return
		}
		targetProc = targetThread.proc
	} else {
		var n *node
		if handle := uint32(tr.target); handle != 0 {
			r := p.getRef(handle, true)
			if r == nil {
				p.logf("transaction to invalid handle %d", handle)
				fail(brFailedReply)
				return
			}
			n = r.node
		} else if n = d.mgr; n == nil {
			fail(brDeadReply)
			return
		}
		if n.proc == nil {
			fail(brDeadReply)
			return
		}
		// The buffer holds the node strongly.
		n.localStrongRefs++
		targetNode = n
		targetProc = n.proc
		if uint32(tr.target) == 0 && targetProc.tg == p.tg {
			p.logf("transaction to the context manager from its own process")
			fail(brFailedReply)
			return
		}
		if tr.flags&tfOneWay == 0 && th.stack != nil {
			tmp := th.stack
			if tmp.toThread != th {
				fail(brFailedReply)
				return
			}
			for ; tmp != nil; tmp = tmp.fromParent {
				if tmp.from != nil && tmp.from.proc == targetProc {
					targetThread = tmp.from
					break
				}
			}
		}
	}
	if !valid {
		fail(brFailedReply)
		return
	}
	if targetProc.dead {
		fail(brDeadReply)
		return
	}

	txn := &transaction{
		id:         d.newID(),
		fromTG:     p.tg,
		senderEUID: p.creds.EffectiveKUID,
		toProc:     targetProc,
		toThread:   targetThread,
		isReply:    reply,
		code:       tr.code,
		flags:      tr.flags,
	}
	txn.work.typ = workTransaction
	txn.work.transaction = txn
	if !reply && tr.flags&tfOneWay == 0 {
		txn.from = th
	}

	offsetsOff := align8(tr.dataSize)
	sgBase := offsetsOff + align8(tr.offsetsSize)
	b, err := targetProc.allocBuffer(sgBase+extraSize, !reply && tr.flags&tfOneWay != 0)
	if err != nil {
		if linuxerr.Equals(linuxerr.ESRCH, err) {
			fail(brDeadReply)
		} else {
			fail(brFailedReply)
		}
		return
	}
	b.dataSize = tr.dataSize
	b.offsetsSize = tr.offsetsSize
	b.extraSize = extraSize
	b.transaction = txn
	b.targetNode = targetNode
	txn.buffer = b
	// The buffer holds the target node's reference from now on.
	targetNode = nil

	image := make([]byte, b.size)
	copy(image, data)
	copy(image[offsetsOff:], offsets)
	translated, ok := th.translate(t, txn, inReplyTo, image, objs, sg, sgBase)

	// failBuffer reports a failure of a transaction with a buffer.
	failBuffer := func(cmd uint32) {
		_ = targetProc.writeBuffer(t, b, 0, image)
		targetProc.releaseObjects(t, b, image, translated, true)
		b.transaction = nil
		targetProc.freeBuffer(b)
		releaseFDs(t, txn)
		fail(cmd)
	}
	if !ok {
		failBuffer(brFailedReply)
		return
	}
	if err := targetProc.writeBuffer(t, b, 0, image); err != nil {
		failBuffer(brFailedReply)
		return
	}

	tcomplete := &work{typ: workTransactionComplete}
	switch {
	case reply:
		th.enqueue(tcomplete)
		if targetThread.dead {
			tcomplete.remove()
			failBuffer(brDeadReply)
			return
		}
		targetThread.stack = inReplyTo.fromParent
		inReplyTo.from = nil
		targetThread.enqueue(&txn.work)
		freeTransaction(t, inReplyTo)

	case tr.flags&tfOneWay == 0:
		// The sender learns that its transaction is sent with the reply.
		th.enqueueDeferred(tcomplete)
		txn.needReply = true
		txn.fromParent = th.stack
		th.stack = txn
		if cmd := procTransaction(txn, targetProc, targetThread); cmd != 0 {
			th.stack = txn.fromParent
			txn.from = nil
			tcomplete.remove()
			failBuffer(cmd)
		}

	default:
		cmd := procTransaction(txn, targetProc, nil)
		th.enqueue(tcomplete)
		if cmd != 0 {
			tcomplete.remove()
			failBuffer(cmd)
		}
	}
}

// translate translates the objects of txn's data, in image, for its target:
// references to nodes, files, and buffers. It returns how many objects it
// translated, and whether it translated all.
//
// Preconditions: the device's mutex is locked.
func (th *thread) translate(t *kernel.Task, txn *transaction, inReplyTo *transaction, image []byte, objs []object, sg [][]byte, sgBase uint64) (int, bool) {
	p := th.proc
	target := txn.toProc
	b := txn.buffer
	acceptFDs := func() bool {
		if inReplyTo != nil {
			return inReplyTo.flags&tfAcceptFDs != 0
		}
		return b.targetNode.acceptFDs
	}
	addFD := func(fd int32, pos uint64) bool {
		if !acceptFDs() {
			return false
		}
		file := t.GetFile(fd)
		if file == nil {
			return false
		}
		txn.fds = append(txn.fds, fdFixup{file: file, offset: pos})
		return true
	}
	sgOff := sgBase
	nsg := 0
	for i := range objs {
		o := &objs[i]
		off := o.offset
		obj := image[off : off+objectSize(o.typ)]
		switch o.typ {
		case typeBinder, typeWeakBinder:
			flags, ptr, cookie := le.Uint32(obj[4:]), le.Uint64(obj[8:]), le.Uint64(obj[16:])
			n := p.newNode(ptr, cookie, flags)
			if n.cookie != cookie {
				return i, false
			}
			strong := o.typ == typeBinder
			r, err := target.incRefForNode(n, strong, th)
			if err != nil {
				return i, false
			}
			typ := typeHandle
			if !strong {
				typ = typeWeakHandle
			}
			le.PutUint32(obj[0:], typ)
			le.PutUint64(obj[8:], 0)
			le.PutUint32(obj[8:], r.handle)
			le.PutUint64(obj[16:], 0)

		case typeHandle, typeWeakHandle:
			strong := o.typ == typeHandle
			r := p.getRef(le.Uint32(obj[8:]), strong)
			if r == nil {
				return i, false
			}
			n := r.node
			if n.proc == target {
				typ := typeBinder
				if !strong {
					typ = typeWeakBinder
				}
				le.PutUint32(obj[0:], typ)
				le.PutUint64(obj[8:], n.ptr)
				le.PutUint64(obj[16:], n.cookie)
				n.inc(strong, false, nil)
			} else {
				r2, err := target.incRefForNode(n, strong, nil)
				if err != nil {
					return i, false
				}
				le.PutUint64(obj[8:], 0)
				le.PutUint32(obj[8:], r2.handle)
				le.PutUint64(obj[16:], 0)
			}

		case typeFD:
			if !addFD(int32(le.Uint32(obj[8:])), off+8) {
				return i, false
			}

		case typePtr:
			o.sgOff = sgOff
			copy(image[sgOff:], sg[nsg])
			nsg++
			le.PutUint64(obj[8:], target.userAddr(b, sgOff))
			if le.Uint32(obj[4:])&bufferFlagHasParent != 0 {
				parent, ok := ptrParent(objs, i, le.Uint64(obj[24:]))
				pos := le.Uint64(obj[32:])
				if !ok || pos%8 != 0 || pos+8 > parent.length {
					return i, false
				}
				le.PutUint64(image[parent.sgOff+pos:], target.userAddr(b, sgOff))
			}
			sgOff += align8(o.length)

		case typeFDA:
			num, pos := le.Uint64(obj[8:]), le.Uint64(obj[24:])
			parent, ok := ptrParent(objs, i, le.Uint64(obj[16:]))
			if !ok || pos%4 != 0 || num > parent.length/4 || pos+4*num > parent.length {
				return i, false
			}
			for j := uint64(0); j < num; j++ {
				at := parent.sgOff + pos + 4*j
				if !addFD(int32(le.Uint32(image[at:])), at) {
					return i, false
				}
			}
		}
	}
	return len(objs), true
}

// ptrParent returns the object that is objs[i]'s parent, which must be an
// earlier binder_buffer_object.
func ptrParent(objs []object, i int, parent uint64) (*object, bool) {
	if parent >= uint64(i) || objs[parent].typ != typePtr {
		return nil, false
	}
	return &objs[parent], true
}

// procTransaction queues txn, a transaction, for its target: thread, a
// thread of its sender's call chain, or a waiting thread of proc's, or
// proc. It returns the BR_* of its failure, or 0.
//
// Preconditions: the device's mutex is locked.
func procTransaction(txn *transaction, proc *proc, thread *thread) uint32 {
	n := txn.buffer.targetNode
	oneway := txn.flags&tfOneWay != 0
	pendingAsync := false
	if oneway {
		if n.hasAsyncTransaction {
			pendingAsync = true
		} else {
			n.hasAsyncTransaction = true
		}
	}
	if proc.dead || (thread != nil && thread.dead) {
		return brDeadReply
	}
	if thread == nil && !pendingAsync && len(proc.waiting) != 0 {
		thread = proc.waiting[0]
		proc.waiting = proc.waiting[1:]
		thread.waitingForProc = false
	}
	switch {
	case thread != nil:
		thread.enqueue(&txn.work)
	case !pendingAsync:
		proc.todo.push(&txn.work)
		proc.pollQueue.Notify(waiter.ReadableEvents)
	default:
		n.asyncTodo.push(&txn.work)
	}
	return 0
}

// releaseFDs releases the files of txn that it did not deliver.
func releaseFDs(ctx context.Context, txn *transaction) {
	for _, f := range txn.fds {
		f.file.DecRef(ctx)
	}
	txn.fds = nil
}

// freeTransaction frees txn.
//
// Preconditions: the device's mutex is locked.
func freeTransaction(ctx context.Context, txn *transaction) {
	if txn.buffer != nil {
		txn.buffer.transaction = nil
	}
	releaseFDs(ctx, txn)
	txn.work.remove()
}

// sendFailedReply tells the sender of txn, or of the first transaction up
// its call chain whose sender lives, that it failed with cmd.
//
// Preconditions: the device's mutex is locked.
func sendFailedReply(ctx context.Context, txn *transaction, cmd uint32) {
	for {
		if target := txn.from; target != nil {
			if target.stack == txn {
				target.stack = txn.fromParent
			}
			txn.from = nil
			if target.replyError.cmd == 0 {
				target.replyError.cmd = cmd
				target.enqueue(&target.replyError)
			}
			freeTransaction(ctx, txn)
			return
		}
		next := txn.fromParent
		freeTransaction(ctx, txn)
		if next == nil {
			return
		}
		txn = next
	}
}

// freeBuf frees b, of p's mapping: it delivers the next oneway transaction
// to b's node, releases b's objects, and frees its memory.
//
// Preconditions: the device's mutex is locked.
func (p *proc) freeBuf(ctx context.Context, b *buffer, failure bool) {
	if b.transaction != nil {
		b.transaction.buffer = nil
		b.transaction = nil
	}
	if b.async && b.targetNode != nil {
		n := b.targetNode
		if n.asyncTodo.empty() {
			n.hasAsyncTransaction = false
		} else {
			p.todo.push(n.asyncTodo.pop())
			p.wakeup()
		}
	}
	image := make([]byte, b.size)
	if err := p.readBuffer(ctx, b, 0, image); err == nil {
		p.releaseObjects(ctx, b, image, -1, failure)
	} else if b.targetNode != nil {
		b.targetNode.dec(true, false)
	}
	p.freeBuffer(b)
}

// releaseObjects releases the references of the first n objects of b's
// data, in image, or all of them if n is negative, as Linux's
// binder_transaction_buffer_release does: its hold on its target node, and
// its references to nodes. Unless failure, which is of a transaction not
// delivered, it closes the files of its file arrays in t, their receiver.
//
// Preconditions: the device's mutex is locked.
func (p *proc) releaseObjects(ctx context.Context, b *buffer, image []byte, n int, failure bool) {
	t := kernel.TaskFromContext(ctx)
	if b.targetNode != nil {
		b.targetNode.dec(true, false)
		b.targetNode = nil
	}
	offsetsOff := align8(b.dataSize)
	count := int(b.offsetsSize / 8)
	if n >= 0 && n < count {
		count = n
	}
	var ptrs []object
	for i := 0; i < count; i++ {
		off := le.Uint64(image[offsetsOff+8*uint64(i):])
		if off+4 > b.dataSize || off+4 < off {
			return
		}
		typ := le.Uint32(image[off:])
		size := objectSize(typ)
		if size == 0 || off+size > b.dataSize {
			return
		}
		obj := image[off : off+size]
		o := object{offset: off, typ: typ}
		switch typ {
		case typeBinder, typeWeakBinder:
			if n := p.nodes[le.Uint64(obj[8:])]; n != nil {
				n.dec(typ == typeBinder, false)
			}
		case typeHandle, typeWeakHandle:
			strong := typ == typeHandle
			if r := p.getRef(le.Uint32(obj[8:]), strong); r != nil {
				r.dec(strong)
			}
		case typePtr:
			if addr := le.Uint64(obj[8:]); addr >= p.userAddr(b, 0) {
				o.sgOff = addr - p.userAddr(b, 0)
				o.length = le.Uint64(obj[16:])
			}
		case typeFDA:
			if failure || t == nil {
				break
			}
			num, parent, pos := le.Uint64(obj[8:]), le.Uint64(obj[16:]), le.Uint64(obj[24:])
			if parent >= uint64(len(ptrs)) {
				break
			}
			par := ptrs[parent]
			if par.typ != typePtr || pos+4*num > par.length || par.sgOff+pos+4*num > uint64(len(image)) {
				break
			}
			for j := uint64(0); j < num; j++ {
				fd := int32(le.Uint32(image[par.sgOff+pos+4*j:]))
				if file := t.FDTable().Remove(t, fd); file != nil {
					file.DecRef(t)
				}
			}
		}
		ptrs = append(ptrs, o)
	}
}
