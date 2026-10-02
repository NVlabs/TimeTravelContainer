// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package vn

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// Pool is memory for decoded commands, which a command's arguments use until
// it is executed. It is mapped memory, outside Go's heap, so that what
// decoders store in it passes to Vulkan as it is: its pointers point into it
// or into other memory that is not Go's.
type Pool struct {
	// chunks are the pool's memory; allocations come from the last.
	chunks [][]byte
	// off is the offset of the next allocation in the last chunk.
	off uintptr
	// total is the size of chunks.
	total uintptr
	// mark is where reset frees from.
	mark poolMark
}

// poolMark is a position in a pool.
type poolMark struct {
	// chunk is the index of a chunk, and off an offset in it.
	chunk int
	off   uintptr
}

const (
	// minPoolChunk is the size of a pool's first chunk.
	minPoolChunk = 64 << 10
	// maxPoolSize is the most memory that a pool has, as virglrenderer
	// allows.
	maxPoolSize = 1 << 30
	// poolAlign is the alignment of allocations.
	poolAlign = 8
)

// alloc returns size zeroed bytes, or nil if the pool would grow too large.
func (p *Pool) alloc(size uintptr) unsafe.Pointer {
	if size > maxPoolSize {
		return nil
	}
	aligned := (size + poolAlign - 1) &^ (poolAlign - 1)
	if len(p.chunks) == 0 || aligned > uintptr(len(p.chunks[len(p.chunks)-1]))-p.off {
		if !p.grow(aligned) {
			return nil
		}
	}
	chunk := p.chunks[len(p.chunks)-1]
	mem := chunk[p.off : p.off+aligned]
	// Memory comes zeroed: what a decoder skips stays zero.
	clear(mem)
	p.off += aligned
	return unsafe.Pointer(unsafe.SliceData(mem))
}

// grow adds a chunk of at least need bytes.
func (p *Pool) grow(need uintptr) bool {
	size := uintptr(minPoolChunk)
	if n := len(p.chunks); n != 0 {
		size = uintptr(len(p.chunks[n-1])) * 2
	}
	for size < need {
		size *= 2
	}
	if size > maxPoolSize-p.total {
		return false
	}
	chunk, err := unix.Mmap(-1, 0, int(size), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANON|unix.MAP_PRIVATE)
	if err != nil {
		return false
	}
	p.chunks = append(p.chunks, chunk)
	p.total += size
	p.off = 0
	return true
}

// reset frees what was allocated since the mark. Without a mark, the pool
// keeps only its largest chunk.
func (p *Pool) reset() {
	n := len(p.chunks)
	if n == 0 {
		return
	}
	keep := p.mark.chunk
	if keep == 0 && p.mark.off == 0 {
		// Keep the last chunk, the largest.
		for _, c := range p.chunks[:n-1] {
			unix.Munmap(c)
			p.total -= uintptr(len(c))
		}
		p.chunks = append(p.chunks[:0], p.chunks[n-1])
		p.off = 0
		return
	}
	for _, c := range p.chunks[keep+1:] {
		unix.Munmap(c)
		p.total -= uintptr(len(c))
	}
	p.chunks = p.chunks[:keep+1]
	p.off = p.mark.off
}

// setMark makes reset keep what is allocated so far, and returns the
// previous mark.
func (p *Pool) setMark() poolMark {
	prev := p.mark
	if len(p.chunks) == 0 {
		p.grow(minPoolChunk)
	}
	p.mark = poolMark{chunk: len(p.chunks) - 1, off: p.off}
	return prev
}

// restoreMark restores a mark that setMark returned.
func (p *Pool) restoreMark(m poolMark) {
	p.mark = m
}

// Release unmaps the pool's memory.
func (p *Pool) Release() {
	for _, c := range p.chunks {
		unix.Munmap(c)
	}
	p.chunks = nil
	p.total = 0
	p.off = 0
	p.mark = poolMark{}
}

// Alloc returns size zeroed bytes of the pool, which stay until Reset, or nil
// if the pool would grow too large.
func (p *Pool) Alloc(size uintptr) unsafe.Pointer {
	return p.alloc(size)
}

// Reset frees what Alloc allocated.
func (p *Pool) Reset() {
	p.reset()
}
