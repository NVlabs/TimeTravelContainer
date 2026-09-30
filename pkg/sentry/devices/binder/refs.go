// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package binder

import (
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
)

// The reference counting of nodes and refs, as Linux's binder_inc_node,
// binder_dec_node, binder_inc_ref, and binder_dec_ref have it. Callers hold
// the device's mutex.
//
// A node's owner learns of references to the node by work of the node's
// (BR_INCREFS, BR_ACQUIRE, BR_RELEASE, BR_DECREFS), and holds a reference of
// its own on the object while the driver does, which it acknowledges
// (BC_INCREFS_DONE, BC_ACQUIRE_DONE).

// newNode returns p's node of ptr: its existing one, or a new one with
// cookie and flags, those of a flat_binder_object.
func (p *proc) newNode(ptr, cookie uint64, flags uint32) *node {
	if n, ok := p.nodes[ptr]; ok {
		return n
	}
	n := &node{
		id:             p.dev.newID(),
		proc:           p,
		ptr:            ptr,
		cookie:         cookie,
		refs:           make(map[uint64]*ref),
		acceptFDs:      flags&flatFlagAcceptsFDs != 0,
		txnSecurityCtx: flags&flatFlagTxnSecurityCtx != 0,
	}
	n.work.typ = workNode
	n.work.node = n
	p.nodes[ptr] = n
	return n
}

// inc takes a reference on n, strong or weak, internal (of a ref) or local
// (of the driver's). If the owner must hear of it, n's work goes to target,
// the todo list of the owner's thread that caused it, or, without one, fails.
func (n *node) inc(strong, internal bool, target *thread) error {
	if strong {
		if internal {
			if target == nil && n.internalStrongRefs == 0 &&
				!(n.proc != nil && n == n.proc.dev.mgr && n.hasStrongRef) {
				return linuxerr.EINVAL
			}
			n.internalStrongRefs++
		} else {
			n.localStrongRefs++
		}
		if !n.hasStrongRef && target != nil {
			n.work.remove()
			target.enqueueDeferred(&n.work)
		}
		return nil
	}
	if !internal {
		n.localWeakRefs++
	}
	if !n.hasWeakRef && n.work.list == nil {
		if target == nil {
			return linuxerr.EINVAL
		}
		target.enqueue(&n.work)
	}
	return nil
}

// dec drops a reference on n, and frees n if nothing refers to it any more.
func (n *node) dec(strong, internal bool) {
	if strong {
		if internal {
			n.internalStrongRefs--
		} else {
			n.localStrongRefs--
		}
		if n.localStrongRefs != 0 || n.internalStrongRefs != 0 {
			return
		}
	} else {
		if !internal {
			n.localWeakRefs--
		}
		if n.localWeakRefs != 0 || len(n.refs) != 0 {
			return
		}
	}
	if p := n.proc; p != nil && (n.hasStrongRef || n.hasWeakRef) {
		// The owner must release its references.
		if n.work.list == nil {
			p.todo.push(&n.work)
			p.wakeup()
		}
		return
	}
	n.maybeFree()
}

// maybeFree frees n if nothing refers to it.
func (n *node) maybeFree() {
	if len(n.refs) != 0 || n.localStrongRefs != 0 || n.localWeakRefs != 0 {
		return
	}
	if p := n.proc; p != nil {
		n.work.remove()
		if p.nodes[n.ptr] == n {
			delete(p.nodes, n.ptr)
		}
	}
}

// refForNode returns p's ref to n, which it creates if p has none.
func (p *proc) refForNode(n *node) *ref {
	if r, ok := p.refsByNode[n.id]; ok {
		return r
	}
	r := &ref{id: p.dev.newID(), proc: p, node: n}
	// Handle 0 is the context manager's; others take the lowest free
	// handle.
	if n != p.dev.mgr {
		r.handle = 1
		for {
			if _, taken := p.refs[r.handle]; !taken {
				break
			}
			r.handle++
		}
	} else if other, taken := p.refs[0]; taken && other.node != n {
		// Another node was context manager.
		r.handle = 1
		for {
			if _, taken := p.refs[r.handle]; !taken {
				break
			}
			r.handle++
		}
	}
	p.refs[r.handle] = r
	p.refsByNode[n.id] = r
	n.refs[r.id] = r
	return r
}

// inc takes a reference on r, strong or weak.
func (r *ref) inc(strong bool, target *thread) error {
	if strong {
		if r.strong == 0 {
			if err := r.node.inc(true, true, target); err != nil {
				return err
			}
		}
		r.strong++
		return nil
	}
	if r.weak == 0 {
		if err := r.node.inc(false, true, target); err != nil {
			return err
		}
	}
	r.weak++
	return nil
}

// dec drops a reference on r, and deletes r once it has none.
func (r *ref) dec(strong bool) error {
	if strong {
		if r.strong == 0 {
			return linuxerr.EINVAL
		}
		r.strong--
		if r.strong == 0 {
			r.node.dec(true, true)
		}
	} else {
		if r.weak == 0 {
			return linuxerr.EINVAL
		}
		r.weak--
	}
	if r.strong == 0 && r.weak == 0 {
		r.cleanup()
	}
	return nil
}

// cleanup deletes r.
func (r *ref) cleanup() {
	p := r.proc
	delete(p.refs, r.handle)
	delete(p.refsByNode, r.node.id)
	n := r.node
	if r.strong != 0 {
		r.strong = 0
		n.dec(true, true)
	}
	delete(n.refs, r.id)
	n.dec(false, true)
	if r.death != nil {
		r.death.work.remove()
		r.death = nil
	}
}

// incRefForNode takes a reference, strong or weak, on p's ref to n.
func (p *proc) incRefForNode(n *node, strong bool, target *thread) (*ref, error) {
	r := p.refForNode(n)
	if err := r.inc(strong, target); err != nil {
		if r.strong == 0 && r.weak == 0 {
			r.cleanup()
		}
		return nil, err
	}
	return r, nil
}

// getRef returns p's ref of handle, which must be strong if needStrong.
func (p *proc) getRef(handle uint32, needStrong bool) *ref {
	r, ok := p.refs[handle]
	if !ok || (needStrong && r.strong == 0) {
		return nil
	}
	return r
}
