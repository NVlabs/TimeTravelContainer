// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package binder

import (
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/waiter"
)

// read fills bwr's read buffer, from bwr.readConsumed on, with th's work, as
// Linux's binder_thread_read does: it waits for work unless nonblock, and
// returns at most one transaction.
func (th *thread) read(t *kernel.Task, bwr *writeRead, nonblock bool) error {
	p := th.proc
	d := p.dev
	limit := bwr.readSize - bwr.readConsumed
	var out []byte
	put32 := func(v uint32) {
		out = le.AppendUint32(out, v)
	}
	if bwr.readConsumed == 0 {
		put32(brNoop)
	}

	d.mu.Lock()
	var procWork bool
retry:
	procWork = th.availableForProcWork()
	th.looper |= looperWaiting
	var err error
	if nonblock {
		if !th.hasWork(procWork) {
			err = linuxerr.EAGAIN
		}
	} else {
		err = th.wait(t, procWork)
	}
	th.looper &^= looperWaiting
	if err != nil {
		d.mu.Unlock()
		return err
	}

	for {
		var list *workList
		switch {
		case !th.todo.empty():
			list = &th.todo
		case procWork && !p.todo.empty():
			list = &p.todo
		default:
			if bwr.readConsumed+uint64(len(out)) == 4 && !th.needReturn {
				goto retry
			}
			goto done
		}
		if limit-uint64(len(out)) < sizeofTransactionDataSecctx+4 {
			goto done
		}
		w := list.pop()
		if th.todo.empty() {
			th.processTodo = false
		}

		switch w.typ {
		case workReturnError:
			put32(w.cmd)
			w.cmd = 0

		case workTransactionComplete:
			put32(brTransactionComplete)

		case workNode:
			n := w.node
			strong := n.internalStrongRefs != 0 || n.localStrongRefs != 0
			weak := len(n.refs) != 0 || n.localWeakRefs != 0 || strong
			hasStrong, hasWeak := n.hasStrongRef, n.hasWeakRef
			if weak && !hasWeak {
				n.hasWeakRef = true
				n.pendingWeakRef = true
				n.localWeakRefs++
			}
			if strong && !hasStrong {
				n.hasStrongRef = true
				n.pendingStrongRef = true
				n.localStrongRefs++
			}
			if !strong && hasStrong {
				n.hasStrongRef = false
			}
			if !weak && hasWeak {
				n.hasWeakRef = false
			}
			if !weak && !strong && p.nodes[n.ptr] == n {
				delete(p.nodes, n.ptr)
			}
			putNode := func(cmd uint32) {
				put32(cmd)
				out = le.AppendUint64(out, n.ptr)
				out = le.AppendUint64(out, n.cookie)
			}
			if weak && !hasWeak {
				putNode(brIncrefs)
			}
			if strong && !hasStrong {
				putNode(brAcquire)
			}
			if !strong && hasStrong {
				putNode(brRelease)
			}
			if !weak && hasWeak {
				putNode(brDecrefs)
			}

		case workDeadBinder, workDeadBinderAndClear, workClearDeathNotification:
			dth := w.death
			cmd := brDeadBinder
			if w.typ == workClearDeathNotification {
				cmd = brClearDeathNotificationDone
			} else {
				p.delivered.push(w)
			}
			put32(cmd)
			out = le.AppendUint64(out, dth.cookie)
			if cmd == brDeadBinder {
				// Notifications of death may cause transactions.
				goto done
			}

		case workTransaction:
			if th.deliver(t, w.transaction, put32, &out) {
				goto done
			}
		}
	}

done:
	spawn := p.requestedThreads == 0 && len(p.waiting) == 0 &&
		p.requestedThreadsStarted < p.maxThreads &&
		th.looper&(looperRegistered|looperEntered) != 0
	if spawn {
		p.requestedThreads++
	}
	d.mu.Unlock()

	if spawn {
		if bwr.readConsumed == 0 {
			le.PutUint32(out, brSpawnLooper)
		} else {
			var b [4]byte
			le.PutUint32(b[:], brSpawnLooper)
			if _, err := t.CopyOutBytes(hostarch.Addr(bwr.readBuffer), b[:]); err != nil {
				return err
			}
		}
	}
	if _, err := t.CopyOutBytes(hostarch.Addr(bwr.readBuffer+bwr.readConsumed), out); err != nil {
		return err
	}
	bwr.readConsumed += uint64(len(out))
	return nil
}

// wait waits for th's work, or, if procWork, its proc's, as Linux's
// binder_wait_for_work does.
//
// Preconditions: the device's mutex is locked. It unlocks it while it
// waits.
func (th *thread) wait(t *kernel.Task, procWork bool) error {
	p := th.proc
	d := p.dev
	for !th.hasWork(procWork) {
		if procWork && !th.waitingForProc {
			p.waiting = append(p.waiting, th)
			th.waitingForProc = true
		}
		e, ch := waiter.NewChannelEntry(waiter.ReadableEvents)
		th.queue.EventRegister(&e)
		d.mu.Unlock()
		err := t.Block(ch)
		d.mu.Lock()
		th.queue.EventUnregister(&e)
		th.removeWaiting()
		if err != nil {
			return linuxerr.EINTR
		}
	}
	return nil
}

// deliver delivers txn to th, as BR_TRANSACTION, or BR_REPLY. It returns
// whether th's read is done.
//
// Preconditions: the device's mutex is locked.
func (th *thread) deliver(t *kernel.Task, txn *transaction, put32 func(uint32), out *[]byte) bool {
	p := th.proc
	b := txn.buffer
	var tr transactionData
	cmd := brReply
	if n := b.targetNode; n != nil {
		tr.target = n.ptr
		tr.cookie = n.cookie
		cmd = brTransaction
	}
	tr.code = txn.code
	tr.flags = txn.flags
	tr.senderEUID = uint32(txn.senderEUID.In(t.UserNamespace()).OrOverflow())
	if txn.from != nil {
		tr.senderPID = int32(t.PIDNamespace().IDOfThreadGroup(txn.from.proc.tg))
	}

	// Install the transaction's files in the receiver, and put their
	// numbers in the buffer.
	var installed []int32
	for _, f := range txn.fds {
		fd, err := t.NewFDFrom(0, f.file, kernel.FDFlags{CloseOnExec: true})
		if err == nil {
			var num [4]byte
			le.PutUint32(num[:], uint32(fd))
			err = p.writeBuffer(t, b, f.offset, num[:])
			installed = append(installed, fd)
		}
		if err != nil {
			for _, fd := range installed {
				if file := t.FDTable().Remove(t, fd); file != nil {
					file.DecRef(t)
				}
			}
			if txn.buffer.targetNode != nil && txn.flags&tfOneWay == 0 {
				sendFailedReply(t, txn, brFailedReply)
			} else {
				freeTransaction(t, txn)
			}
			b.transaction = nil
			p.freeBuf(t, b, true)
			if cmd == brReply {
				put32(brFailedReply)
				return true
			}
			return false
		}
	}
	releaseFDs(t, txn)

	tr.dataSize = b.dataSize
	tr.offsetsSize = b.offsetsSize
	tr.buffer = p.userAddr(b, 0)
	tr.offsets = tr.buffer + align8(b.dataSize)
	put32(cmd)
	var enc [sizeofTransactionData]byte
	tr.encode(enc[:])
	*out = append(*out, enc[:]...)

	b.userFree = true
	if cmd == brTransaction && txn.flags&tfOneWay == 0 {
		txn.toParent = th.stack
		txn.toThread = th
		th.stack = txn
	} else {
		freeTransaction(t, txn)
	}
	return true
}
