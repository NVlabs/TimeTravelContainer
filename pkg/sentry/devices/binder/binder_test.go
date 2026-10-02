// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package binder

import (
	"bytes"
	"testing"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/contexttest"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
)

// The UAPI's numbers, from a Linux build's include/uapi/linux/android/binder.h.
func TestUAPINumbers(t *testing.T) {
	for _, test := range []struct {
		name      string
		got, want uint32
	}{
		{"BINDER_WRITE_READ", ioctlWriteRead, 0xc0306201},
		{"BINDER_SET_MAX_THREADS", ioctlSetMaxThreads, 0x40046205},
		{"BINDER_SET_CONTEXT_MGR", ioctlSetContextMgr, 0x40046207},
		{"BINDER_THREAD_EXIT", ioctlThreadExit, 0x40046208},
		{"BINDER_VERSION", ioctlVersion, 0xc0046209},
		{"BINDER_SET_CONTEXT_MGR_EXT", ioctlSetContextMgrExt, 0x4018620d},
		{"BINDER_CTL_ADD", ioctlCtlAdd, 0xc1086201},
		{"BC_TRANSACTION", bcTransaction, 0x40406300},
		{"BC_REPLY", bcReply, 0x40406301},
		{"BC_FREE_BUFFER", bcFreeBuffer, 0x40086303},
		{"BC_ACQUIRE", bcAcquire, 0x40046305},
		{"BC_INCREFS_DONE", bcIncrefsDone, 0x40106308},
		{"BC_ENTER_LOOPER", bcEnterLooper, 0x630c},
		{"BC_REQUEST_DEATH_NOTIFICATION", bcRequestDeathNotification, 0x400c630e},
		{"BC_TRANSACTION_SG", bcTransactionSG, 0x40486311},
		{"BR_TRANSACTION", brTransaction, 0x80407202},
		{"BR_TRANSACTION_SEC_CTX", brTransactionSecCtx, 0x80487202},
		{"BR_REPLY", brReply, 0x80407203},
		{"BR_TRANSACTION_COMPLETE", brTransactionComplete, 0x7206},
		{"BR_INCREFS", brIncrefs, 0x80107207},
		{"BR_NOOP", brNoop, 0x720c},
		{"BR_SPAWN_LOOPER", brSpawnLooper, 0x720d},
		{"BR_DEAD_BINDER", brDeadBinder, 0x8008720f},
		{"BR_FAILED_REPLY", brFailedReply, 0x7211},
		{"BINDER_TYPE_BINDER", typeBinder, 0x73622a85},
		{"BINDER_TYPE_HANDLE", typeHandle, 0x73682a85},
		{"BINDER_TYPE_FD", typeFD, 0x66642a85},
		{"BINDER_TYPE_PTR", typePtr, 0x70742a85},
	} {
		if test.got != test.want {
			t.Errorf("%s = %#x, want %#x", test.name, test.got, test.want)
		}
	}
}

func TestAllocBuffer(t *testing.T) {
	p := &proc{mapped: true, size: 4096, freeAsyncSpace: 2048}
	a, err := p.allocBuffer(100, false)
	if err != nil || a.offset != 0 || a.size != 104 {
		t.Fatalf("allocBuffer(100) = %+v, %v", a, err)
	}
	b, err := p.allocBuffer(0, true)
	if err != nil || b.offset != 104 || b.size != 8 || p.freeAsyncSpace != 2040 {
		t.Fatalf("allocBuffer(0, async) = %+v, %v; async space %d", b, err, p.freeAsyncSpace)
	}
	c, err := p.allocBuffer(1000, false)
	if err != nil || c.offset != 112 {
		t.Fatalf("allocBuffer(1000) = %+v, %v", c, err)
	}
	p.freeBuffer(a)
	// The first gap fits.
	d, err := p.allocBuffer(64, false)
	if err != nil || d.offset != 0 {
		t.Fatalf("allocBuffer(64) after a free = %+v, %v", d, err)
	}
	if _, err := p.allocBuffer(4096, false); err == nil {
		t.Errorf("allocBuffer beyond the mapping succeeded")
	}
	if _, err := p.allocBuffer(4000, true); err == nil {
		t.Errorf("an async allocBuffer beyond the async space succeeded")
	}
	p.freeBuffer(b)
	if p.freeAsyncSpace != 2048 {
		t.Errorf("async space after freeing = %d, want 2048", p.freeAsyncSpace)
	}
	p.base = 0x7000
	if got := p.bufferAt(0x7000 + c.offset); got != c {
		t.Errorf("bufferAt(c) = %+v", got)
	}
	if got := p.bufferAt(0x7000 + c.offset + 8); got != nil {
		t.Errorf("bufferAt(inside c) = %+v, want nil", got)
	}
}

func TestRefs(t *testing.T) {
	d := NewDevice("binder")
	owner := &proc{dev: d, nodes: map[uint64]*node{}, refs: map[uint32]*ref{}, refsByNode: map[uint64]*ref{}, threads: map[kernel.ThreadID]*thread{}}
	user := &proc{dev: d, nodes: map[uint64]*node{}, refs: map[uint32]*ref{}, refsByNode: map[uint64]*ref{}, threads: map[kernel.ThreadID]*thread{}}
	th := &thread{proc: owner}
	n := owner.newNode(0x1000, 0x2000, flatFlagAcceptsFDs)
	if !n.acceptFDs {
		t.Errorf("the node does not accept files")
	}
	// A strong reference from another proc makes the owner acquire the
	// object, by the owner's thread.
	r, err := user.incRefForNode(n, true, th)
	if err != nil || r.handle != 1 || n.internalStrongRefs != 1 {
		t.Fatalf("incRefForNode = %+v, %v; node %+v", r, err, n)
	}
	if th.todo.empty() || th.todo.items[0] != &n.work {
		t.Fatalf("the node's work is not queued for the owner's thread")
	}
	// A second ref takes the next handle.
	n2 := owner.newNode(0x3000, 0, 0)
	r2, err := user.incRefForNode(n2, false, th)
	if err != nil || r2.handle != 2 {
		t.Fatalf("second ref = %+v, %v", r2, err)
	}
	// Dropping the strong reference drops the node's internal strong
	// reference; the ref goes with its last reference.
	if err := r.dec(true); err != nil {
		t.Fatal(err)
	}
	if n.internalStrongRefs != 0 || user.refs[1] != nil || len(n.refs) != 0 {
		t.Errorf("after the last reference: node %+v, refs %v", n, user.refs)
	}
	if err := r.dec(true); err == nil {
		t.Errorf("dec of a dead ref succeeded")
	}
}

// TestRestoredProcMemory checks that a restored proc writes its buffers'
// saved data into memory of its own, rather than into the saved proc's: the
// two branches of a live fork each write their own transactions.
func TestRestoredProcMemory(t *testing.T) {
	ctx := contexttest.Context(t)
	p := &proc{dev: &Device{}}
	if err := p.allocMapping(ctx, 2*hostarch.PageSize); err != nil {
		t.Fatalf("allocMapping: %v", err)
	}
	defer p.freeMapping(ctx)
	p.mapped = true
	b, err := p.allocBuffer(64, false)
	if err != nil {
		t.Fatalf("allocBuffer: %v", err)
	}
	data := []byte("a transaction in flight at the save")
	if err := p.writeBuffer(ctx, b, 0, data); err != nil {
		t.Fatalf("writeBuffer: %v", err)
	}

	p.beforeSave()
	// Model the restored proc: its memory is not saved.
	source, sourceMF := p.fr, p.mf
	defer sourceMF.DecRef(source)
	p.fr, p.mf = memmap.FileRange{}, nil

	got := make([]byte, len(data))
	if err := p.readBuffer(ctx, b, 0, got); err != nil {
		t.Fatalf("readBuffer: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("restored buffer holds %q, want %q", got, data)
	}
	if p.fr.Length() == 0 || p.fr.Overlaps(source) {
		t.Errorf("restored proc's memory %v, want memory of its own, apart from %v", p.fr, source)
	}
	if b.contents != nil {
		t.Errorf("restored buffer keeps its saved data")
	}
}
