// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package binder

import (
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/usermem"
	"gvisor.dev/gvisor/pkg/waiter"
)

// Open returns a file of the device for the calling process: a proc.
func (d *Device) Open(ctx context.Context, mnt *vfs.Mount, vfsd *vfs.Dentry, opts vfs.OpenOptions) (*vfs.FileDescription, error) {
	t := kernel.TaskFromContext(ctx)
	if t == nil {
		return nil, linuxerr.EINVAL
	}
	p := &proc{
		dev:        d,
		tg:         t.ThreadGroup(),
		creds:      t.Credentials(),
		threads:    make(map[kernel.ThreadID]*thread),
		nodes:      make(map[uint64]*node),
		refs:       make(map[uint32]*ref),
		refsByNode: make(map[uint64]*ref),
	}
	fd := &procFD{proc: p}
	if err := fd.vfsfd.Init(fd, opts.Flags, auth.CredentialsFromContext(ctx), mnt, vfsd, &vfs.FileDescriptionOptions{
		UseDentryMetadata: true,
		DenyPRead:         true,
		DenyPWrite:        true,
	}); err != nil {
		return nil, err
	}
	return &fd.vfsfd, nil
}

// procFD is a proc's file.
//
// +stateify savable
type procFD struct {
	vfsfd vfs.FileDescription
	vfs.FileDescriptionDefaultImpl
	vfs.DentryMetadataFileDescriptionImpl
	vfs.NoLockFD

	proc *proc
}

// Release implements vfs.FileDescriptionImpl.Release.
func (fd *procFD) Release(ctx context.Context) {
	p := fd.proc
	d := p.dev
	d.mu.Lock()
	p.release(ctx)
	d.mu.Unlock()
	p.freeMapping(ctx)
}

// Readiness implements waiter.Waitable.Readiness. Linux's binder_poll
// answers for the polling thread: the sentry's file does not know it, so it
// is readable if the proc has work, or a thread of its that is not reading
// has.
func (fd *procFD) Readiness(mask waiter.EventMask) waiter.EventMask {
	p := fd.proc
	p.dev.mu.Lock()
	defer p.dev.mu.Unlock()
	if !p.todo.empty() {
		return mask & waiter.ReadableEvents
	}
	for _, th := range p.threads {
		if th.processTodo && th.looper&looperWaiting == 0 {
			return mask & waiter.ReadableEvents
		}
	}
	return 0
}

// EventRegister implements waiter.Waitable.EventRegister.
func (fd *procFD) EventRegister(e *waiter.Entry) error {
	fd.proc.pollQueue.EventRegister(e)
	return nil
}

// EventUnregister implements waiter.Waitable.EventUnregister.
func (fd *procFD) EventUnregister(e *waiter.Entry) {
	fd.proc.pollQueue.EventUnregister(e)
}

// Epollable implements vfs.FileDescriptionImpl.Epollable.
func (fd *procFD) Epollable() bool {
	return true
}

// Ioctl implements vfs.FileDescriptionImpl.Ioctl.
func (fd *procFD) Ioctl(ctx context.Context, uio usermem.IO, sysno uintptr, args arch.SyscallArguments) (uintptr, error) {
	t := kernel.TaskFromContext(ctx)
	if t == nil {
		return 0, linuxerr.EINVAL
	}
	cmd := args[1].Uint()
	arg := args[2].Pointer()
	p := fd.proc
	d := p.dev

	switch cmd {
	case ioctlWriteRead:
		return 0, fd.writeRead(t, arg)

	case ioctlSetMaxThreads:
		var b [4]byte
		if _, err := t.CopyInBytes(arg, b[:]); err != nil {
			return 0, err
		}
		d.mu.Lock()
		p.maxThreads = le.Uint32(b[:])
		d.mu.Unlock()
		return 0, nil

	case ioctlSetContextMgr, ioctlSetContextMgrExt:
		var ptr, cookie uint64
		var flags uint32
		if cmd == ioctlSetContextMgrExt {
			var b [sizeofFlatObject]byte
			if _, err := t.CopyInBytes(arg, b[:]); err != nil {
				return 0, err
			}
			flags = le.Uint32(b[4:])
			ptr = le.Uint64(b[8:])
			cookie = le.Uint64(b[16:])
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		return 0, p.setContextManager(t, ptr, cookie, flags)

	case ioctlThreadExit:
		tid := p.tid(t)
		d.mu.Lock()
		defer d.mu.Unlock()
		if th, ok := p.threads[tid]; ok {
			th.release(t)
		}
		return 0, nil

	case ioctlVersion:
		var b [4]byte
		le.PutUint32(b[:], ProtocolVersion)
		_, err := t.CopyOutBytes(arg, b[:])
		return 0, err

	case ioctlGetNodeDebugInfo:
		var b [sizeofNodeDebugInfo]byte
		if _, err := t.CopyInBytes(arg, b[:]); err != nil {
			return 0, err
		}
		after := le.Uint64(b[0:])
		d.mu.Lock()
		var found *node
		for _, n := range p.nodes {
			if n.ptr > after && (found == nil || n.ptr < found.ptr) {
				found = n
			}
		}
		var out [sizeofNodeDebugInfo]byte
		if found != nil {
			le.PutUint64(out[0:], found.ptr)
			le.PutUint64(out[8:], found.cookie)
			if found.hasStrongRef {
				le.PutUint32(out[16:], 1)
			}
			if found.hasWeakRef {
				le.PutUint32(out[20:], 1)
			}
		}
		d.mu.Unlock()
		_, err := t.CopyOutBytes(arg, out[:])
		return 0, err

	case ioctlGetNodeInfoForRef:
		var b [sizeofNodeInfoForRef]byte
		if _, err := t.CopyInBytes(arg, b[:]); err != nil {
			return 0, err
		}
		for i := 4; i < len(b); i++ {
			if b[i] != 0 {
				return 0, linuxerr.EINVAL
			}
		}
		d.mu.Lock()
		if d.mgr == nil || d.mgr.proc != p {
			d.mu.Unlock()
			return 0, linuxerr.EPERM
		}
		r := p.getRef(le.Uint32(b[0:]), false)
		if r == nil {
			d.mu.Unlock()
			return 0, linuxerr.EINVAL
		}
		le.PutUint32(b[4:], uint32(r.node.localStrongRefs+r.node.internalStrongRefs))
		le.PutUint32(b[8:], uint32(r.node.localWeakRefs))
		d.mu.Unlock()
		_, err := t.CopyOutBytes(arg, b[:])
		return 0, err

	case ioctlEnableOnewaySpamDetection:
		return 0, nil

	case ioctlGetExtendedError:
		var out [sizeofExtendedError]byte
		le.PutUint32(out[4:], brOK)
		_, err := t.CopyOutBytes(arg, out[:])
		return 0, err

	default:
		// Freezing processes is not supported: Android's freezer, which
		// checks for it with BINDER_GET_FROZEN_INFO, turns itself off.
		return 0, linuxerr.EINVAL
	}
}

// tid returns t's thread ID, as the proc keys its threads.
//
// Preconditions: p.dev.mu is unlocked: it takes the TaskSet's lock.
func (p *proc) tid(t *kernel.Task) kernel.ThreadID {
	return t.Kernel().TaskSet().Root.IDOfTask(t)
}

// thread returns the thread of p of thread ID tid, which it creates if
// needed.
//
// Preconditions: p.dev.mu is locked.
func (p *proc) thread(tid kernel.ThreadID) *thread {
	if th, ok := p.threads[tid]; ok {
		return th
	}
	th := &thread{proc: p, tid: tid, needReturn: true}
	th.returnError.typ = workReturnError
	th.replyError.typ = workReturnError
	p.threads[tid] = th
	return th
}

// setContextManager makes p the context manager, with a node of ptr and
// cookie.
//
// Preconditions: p.dev.mu is locked.
func (p *proc) setContextManager(t *kernel.Task, ptr, cookie uint64, flags uint32) error {
	d := p.dev
	if d.mgr != nil {
		return linuxerr.EBUSY
	}
	euid := t.Credentials().EffectiveKUID
	if d.mgrUID != nil {
		if *d.mgrUID != euid {
			return linuxerr.EPERM
		}
	} else {
		d.mgrUID = &euid
	}
	n := p.newNode(ptr, cookie, flags)
	n.localWeakRefs++
	n.localStrongRefs++
	n.hasStrongRef = true
	n.hasWeakRef = true
	d.mgr = n
	return nil
}

// enqueue queues w on th's todo list, which th processes.
//
// Preconditions: the device's mutex is locked.
func (th *thread) enqueue(w *work) {
	th.todo.push(w)
	th.processTodo = true
	th.queue.Notify(waiter.ReadableEvents)
	th.proc.pollQueue.Notify(waiter.ReadableEvents)
}

// enqueueDeferred queues w on th's todo list, without waking th for it: th
// gets it with other work, as Linux's deferred thread work.
//
// Preconditions: the device's mutex is locked.
func (th *thread) enqueueDeferred(w *work) {
	th.todo.push(w)
}

// wakeup wakes a thread of p's for its proc work: one that waits, or those
// that poll.
//
// Preconditions: the device's mutex is locked.
func (p *proc) wakeup() {
	if len(p.waiting) != 0 {
		th := p.waiting[0]
		p.waiting = p.waiting[1:]
		th.waitingForProc = false
		th.queue.Notify(waiter.ReadableEvents)
		return
	}
	p.pollQueue.Notify(waiter.ReadableEvents)
}

// removeWaiting removes th from its proc's waiting threads.
//
// Preconditions: the device's mutex is locked.
func (th *thread) removeWaiting() {
	if !th.waitingForProc {
		return
	}
	p := th.proc
	for i, w := range p.waiting {
		if w == th {
			p.waiting = append(p.waiting[:i], p.waiting[i+1:]...)
			break
		}
	}
	th.waitingForProc = false
}

// availableForProcWork returns whether th may take its proc's work.
//
// Preconditions: the device's mutex is locked.
func (th *thread) availableForProcWork() bool {
	return th.stack == nil && th.todo.empty() && th.looper&(looperEntered|looperRegistered) != 0
}

// hasWork returns whether th has work to return.
//
// Preconditions: the device's mutex is locked.
func (th *thread) hasWork(procWork bool) bool {
	return th.processTodo || th.needReturn || (procWork && !th.proc.todo.empty())
}

// writeRead implements BINDER_WRITE_READ.
func (fd *procFD) writeRead(t *kernel.Task, addr hostarch.Addr) error {
	var b [sizeofWriteRead]byte
	if _, err := t.CopyInBytes(addr, b[:]); err != nil {
		return err
	}
	var bwr writeRead
	bwr.decode(b[:])
	p := fd.proc
	d := p.dev

	tid := p.tid(t)
	d.mu.Lock()
	th := p.thread(tid)
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		th.needReturn = false
		d.mu.Unlock()
	}()

	if bwr.writeSize > 0 {
		if err := th.write(t, &bwr); err != nil {
			bwr.readConsumed = 0
			bwr.encode(b[:])
			t.CopyOutBytes(addr, b[:])
			return err
		}
	}
	if bwr.readSize > 0 {
		err := th.read(t, &bwr, fd.vfsfd.StatusFlags()&linux.O_NONBLOCK != 0)
		d.mu.Lock()
		if !p.todo.empty() {
			p.wakeup()
		}
		d.mu.Unlock()
		if err != nil {
			bwr.encode(b[:])
			t.CopyOutBytes(addr, b[:])
			return err
		}
	}
	bwr.encode(b[:])
	_, err := t.CopyOutBytes(addr, b[:])
	return err
}

// The proc's buffer mapping.

// ConfigureMMap implements vfs.FileDescriptionImpl.ConfigureMMap. A process
// maps its buffer once, read-only: the driver writes transactions in it.
func (fd *procFD) ConfigureMMap(ctx context.Context, opts *memmap.MMapOpts) error {
	if opts.Perms.Write {
		return linuxerr.EPERM
	}
	if opts.Offset != 0 {
		return linuxerr.EINVAL
	}
	p := fd.proc
	if err := p.allocMapping(ctx, opts.Length); err != nil {
		return err
	}
	opts.MaxPerms = hostarch.Read
	opts.Mappable = p
	opts.MappingIdentity = &fd.vfsfd
	fd.vfsfd.IncRef()
	return nil
}

// allocMapping allocates the memory of p's buffer, of size length, bounded.
func (p *proc) allocMapping(ctx context.Context, length uint64) error {
	d := p.dev
	d.mu.Lock()
	defer d.mu.Unlock()
	p.mapMu.Lock()
	defer p.mapMu.Unlock()
	if p.size != 0 {
		return linuxerr.EBUSY
	}
	size, ok := hostarch.PageRoundUp(min(length, maxBufferSize))
	if !ok || size == 0 {
		return linuxerr.EINVAL
	}
	mf := pgalloc.MemoryFileFromContext(ctx)
	fr, err := mf.Allocate(size, pgalloc.AllocOpts{Kind: usage.Anonymous})
	if err != nil {
		return err
	}
	p.fr, p.mf = fr, mf
	p.size = size
	p.freeAsyncSpace = size / 2
	return nil
}

// freeMapping frees the memory of p's buffer, which nothing maps any more.
func (p *proc) freeMapping(ctx context.Context) {
	p.mapMu.Lock()
	defer p.mapMu.Unlock()
	if p.fr.Length() == 0 {
		return
	}
	p.mf.DecRef(p.fr)
	p.fr, p.mf = memmap.FileRange{}, nil
}

// AddMapping implements memmap.Mappable.AddMapping.
func (p *proc) AddMapping(ctx context.Context, ms memmap.MappingSpace, ar hostarch.AddrRange, offset uint64, writable bool) error {
	d := p.dev
	d.mu.Lock()
	defer d.mu.Unlock()
	p.mapMu.Lock()
	defer p.mapMu.Unlock()
	if !p.mapped && offset == 0 {
		p.base = uint64(ar.Start)
		p.mapped = true
	}
	p.mappings.AddMapping(ms, ar, offset, writable)
	return nil
}

// RemoveMapping implements memmap.Mappable.RemoveMapping.
func (p *proc) RemoveMapping(ctx context.Context, ms memmap.MappingSpace, ar hostarch.AddrRange, offset uint64, writable bool) {
	d := p.dev
	d.mu.Lock()
	defer d.mu.Unlock()
	p.mapMu.Lock()
	defer p.mapMu.Unlock()
	p.mappings.RemoveMapping(ms, ar, offset, writable)
	if p.mappings.IsEmpty() {
		// Transactions to the proc fail from now on, as Linux's do once
		// the proc's buffer's VMA is closed.
		p.mapped = false
	}
}

// CopyMapping implements memmap.Mappable.CopyMapping.
func (p *proc) CopyMapping(ctx context.Context, ms memmap.MappingSpace, srcAR, dstAR hostarch.AddrRange, offset uint64, writable bool) error {
	d := p.dev
	d.mu.Lock()
	defer d.mu.Unlock()
	p.mapMu.Lock()
	defer p.mapMu.Unlock()
	p.mappings.AddMapping(ms, dstAR, offset, writable)
	return nil
}

// Translate implements memmap.Mappable.Translate.
func (p *proc) Translate(ctx context.Context, required, optional memmap.MappableRange, at hostarch.AccessType) ([]memmap.Translation, error) {
	if at.Write || at.Execute {
		return nil, &memmap.BusError{Err: linuxerr.EFAULT}
	}
	p.mapMu.Lock()
	fr, err := p.memoryLocked(ctx)
	size, mf := p.size, p.mf
	p.mapMu.Unlock()
	if err != nil {
		return nil, err
	}
	if required.End > size || fr.Length() == 0 {
		return nil, &memmap.BusError{Err: linuxerr.EFAULT}
	}
	source := optional
	if source.End > size {
		source.End = size
	}
	return []memmap.Translation{{
		Source: source,
		File:   mf,
		Offset: fr.Start + source.Start,
		Perms:  hostarch.Read,
	}}, nil
}

// MayUseHugepages implements memmap.Mappable.MayUseHugepages.
func (*proc) MayUseHugepages() bool {
	return false
}

// InvalidateUnsavable implements memmap.Mappable.InvalidateUnsavable. The
// proc's memory is not saved, so translations to it must not be: the child
// of a live fork would read the source's buffer, which only the source
// writes to.
func (p *proc) InvalidateUnsavable(ctx context.Context) error {
	p.mapMu.Lock()
	defer p.mapMu.Unlock()
	p.mappings.InvalidateAll(memmap.InvalidateOpts{InvalidatePrivate: true})
	return nil
}

// logf logs a user error, as Linux's binder_user_error.
func (p *proc) logf(format string, v ...any) {
	log.Debugf("binder %s: "+format, append([]any{p.dev.name}, v...)...)
}
