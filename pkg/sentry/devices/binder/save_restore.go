// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package binder

import (
	"fmt"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

// A proc's buffer memory is not saved: a live fork's branches each write
// their own transactions, so each needs memory of its own. The proc saves the
// data of its allocated buffers instead, all that its memory holds of use,
// and a restored proc allocates new memory when it first needs it, for a
// fault or a transaction, and writes the data back in.

// beforeSave is invoked by stateify.
func (p *proc) beforeSave() {
	p.mapMu.Lock()
	defer p.mapMu.Unlock()
	if p.fr.Length() == 0 {
		// A restored proc that has not allocated its memory keeps its
		// buffers' saved data.
		return
	}
	for _, b := range p.buffers {
		b.contents = make([]byte, b.size)
		if err := copyMemory(p.mf, p.fr.Start+b.offset, b.contents, false /* write */); err != nil {
			panic(fmt.Sprintf("saving binder buffer at %#x: %v", b.offset, err))
		}
	}
}

// memoryLocked returns p's buffer memory, which a restored proc allocates on
// first use.
//
// Preconditions: p.mapMu is locked.
func (p *proc) memoryLocked(ctx context.Context) (memmap.FileRange, error) {
	if p.fr.Length() != 0 || p.size == 0 || p.dead {
		return p.fr, nil
	}
	mf := pgalloc.MemoryFileFromContext(ctx)
	fr, err := mf.Allocate(p.size, pgalloc.AllocOpts{Kind: usage.Anonymous})
	if err != nil {
		return memmap.FileRange{}, err
	}
	for _, b := range p.buffers {
		if len(b.contents) == 0 {
			continue
		}
		if err := copyMemory(mf, fr.Start+b.offset, b.contents, true /* write */); err != nil {
			mf.DecRef(fr)
			return memmap.FileRange{}, err
		}
	}
	for _, b := range p.buffers {
		b.contents = nil
	}
	p.fr, p.mf = fr, mf
	return fr, nil
}
