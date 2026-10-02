// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package binder

import (
	"sort"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
)

// allocBuffer allocates a buffer of size bytes in p's mapping, first fit, as
// Linux's binder_alloc_new_buf, with its best fit, does. Oneway
// transactions may take half of the mapping. It fails with ESRCH if p maps
// no buffer, and ENOSPC if the buffer has no room.
//
// Preconditions: p.dev.mu is locked.
func (p *proc) allocBuffer(size uint64, async bool) (*buffer, error) {
	if !p.mapped || p.size == 0 {
		return nil, linuxerr.ESRCH
	}
	// Buffers of no data get addresses of their own.
	size = max(align8(size), 8)
	if async && p.freeAsyncSpace < size {
		return nil, linuxerr.ENOSPC
	}
	var start uint64
	i := 0
	for ; i < len(p.buffers); i++ {
		b := p.buffers[i]
		if b.offset-start >= size {
			break
		}
		start = b.offset + b.size
	}
	if start+size > p.size {
		return nil, linuxerr.ENOSPC
	}
	b := &buffer{offset: start, size: size, async: async}
	p.buffers = append(p.buffers, nil)
	copy(p.buffers[i+1:], p.buffers[i:])
	p.buffers[i] = b
	if async {
		p.freeAsyncSpace -= size
	}
	return b, nil
}

// freeBuffer frees b's memory.
//
// Preconditions: p.dev.mu is locked.
func (p *proc) freeBuffer(b *buffer) {
	i := sort.Search(len(p.buffers), func(i int) bool { return p.buffers[i].offset >= b.offset })
	if i == len(p.buffers) || p.buffers[i] != b {
		return
	}
	p.buffers = append(p.buffers[:i], p.buffers[i+1:]...)
	if b.async {
		p.freeAsyncSpace += b.size
	}
}

// bufferAt returns p's buffer whose data is at addr, a user address.
//
// Preconditions: p.dev.mu is locked.
func (p *proc) bufferAt(addr uint64) *buffer {
	if addr < p.base {
		return nil
	}
	off := addr - p.base
	i := sort.Search(len(p.buffers), func(i int) bool { return p.buffers[i].offset >= off })
	if i == len(p.buffers) || p.buffers[i].offset != off {
		return nil
	}
	return p.buffers[i]
}

// userAddr returns the user address of b's data at off.
func (p *proc) userAddr(b *buffer, off uint64) uint64 {
	return p.base + b.offset + off
}

// writeBuffer writes data to b at off.
//
// Preconditions: p.dev.mu is locked.
func (p *proc) writeBuffer(ctx context.Context, b *buffer, off uint64, data []byte) error {
	return p.accessBuffer(ctx, b, off, data, true)
}

// readBuffer reads b at off into data.
//
// Preconditions: p.dev.mu is locked.
func (p *proc) readBuffer(ctx context.Context, b *buffer, off uint64, data []byte) error {
	return p.accessBuffer(ctx, b, off, data, false)
}

func (p *proc) accessBuffer(ctx context.Context, b *buffer, off uint64, data []byte, write bool) error {
	if len(data) == 0 {
		return nil
	}
	if off+uint64(len(data)) > b.size || off+uint64(len(data)) < off {
		return linuxerr.EFAULT
	}
	p.mapMu.Lock()
	fr, err := p.memoryLocked(ctx)
	mf := p.mf
	p.mapMu.Unlock()
	if err != nil {
		return err
	}
	return copyMemory(mf, fr.Start+b.offset+off, data, write)
}

// copyMemory copies data to mf at start, or with write false, from it.
func copyMemory(mf *pgalloc.MemoryFile, start uint64, data []byte, write bool) error {
	fr := memmap.FileRange{Start: start, End: start + uint64(len(data))}
	at := hostarch.Read
	if write {
		at = hostarch.Write
	}
	ims, lease, err := memmap.MapInternalWithLease(mf, fr, at)
	if err != nil {
		return err
	}
	if lease != nil {
		defer lease.Release()
	}
	local := safemem.BlockSeqOf(safemem.BlockFromSafeSlice(data))
	if write {
		_, err = safemem.CopySeq(ims, local)
	} else {
		_, err = safemem.CopySeq(local, ims)
	}
	return err
}
