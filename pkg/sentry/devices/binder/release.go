// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package binder

import (
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/waiter"
)

// release releases p, whose file is closed, as Linux's
// binder_deferred_release does: the transactions it owed replies fail, its
// nodes die, and those who asked hear of it.
//
// Preconditions: p.dev.mu is locked.
func (p *proc) release(ctx context.Context) {
	d := p.dev
	if d.mgr != nil && d.mgr.proc == p {
		d.mgr = nil
	}
	p.dead = true
	for _, th := range p.threads {
		th.release(ctx)
	}
	for _, n := range p.nodes {
		n.release(ctx)
	}
	for _, r := range p.refs {
		r.cleanup()
	}
	p.releaseWork(ctx, &p.todo)
	p.releaseWork(ctx, &p.delivered)
	for _, b := range p.buffers {
		if b.transaction != nil {
			b.transaction.buffer = nil
			b.transaction = nil
		}
	}
	p.buffers = nil
	p.waiting = nil
}

// release releases th, which exited, or whose proc died, as Linux's
// binder_thread_release does.
//
// Preconditions: the device's mutex is locked.
func (th *thread) release(ctx context.Context) {
	p := th.proc
	delete(p.threads, th.tid)
	th.removeWaiting()
	th.dead = true
	var sendReply *transaction
	txn := th.stack
	if txn != nil && txn.toThread == th {
		sendReply = txn
	}
	for txn != nil {
		switch {
		case txn.toThread == th:
			txn.toProc = nil
			txn.toThread = nil
			if txn.buffer != nil {
				txn.buffer.transaction = nil
				txn.buffer = nil
			}
			txn = txn.toParent
		case txn.from == th:
			txn.from = nil
			txn = txn.fromParent
		default:
			txn = nil
		}
	}
	th.stack = nil
	if sendReply != nil {
		sendFailedReply(ctx, sendReply, brDeadReply)
	}
	p.releaseWork(ctx, &th.todo)
	th.queue.Notify(waiter.ReadableEvents)
}

// release releases n, whose owner died, as Linux's binder_node_release
// does: n lives on while other procs refer to it, and those that asked hear
// of its death.
//
// Preconditions: the device's mutex is locked.
func (n *node) release(ctx context.Context) {
	p := n.proc
	p.releaseWork(ctx, &n.asyncTodo)
	n.work.remove()
	delete(p.nodes, n.ptr)
	if len(n.refs) == 0 {
		return
	}
	n.proc = nil
	n.localStrongRefs = 0
	n.localWeakRefs = 0
	for _, r := range n.refs {
		if r.death == nil || r.proc.dead || r.death.work.list != nil {
			continue
		}
		r.death.work.typ = workDeadBinder
		r.proc.todo.push(&r.death.work)
		r.proc.wakeup()
	}
}

// releaseWork discards the work of list: transactions fail back to their
// senders.
//
// Preconditions: the device's mutex is locked.
func (p *proc) releaseWork(ctx context.Context, list *workList) {
	for !list.empty() {
		w := list.pop()
		switch w.typ {
		case workTransaction:
			txn := w.transaction
			if txn.buffer != nil && txn.buffer.targetNode != nil && txn.flags&tfOneWay == 0 {
				sendFailedReply(ctx, txn, brDeadReply)
			} else {
				freeTransaction(ctx, txn)
			}
		case workReturnError:
			w.cmd = 0
		}
	}
}
