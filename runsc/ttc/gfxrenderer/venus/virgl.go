// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package venus

import (
	"log"

	"gvisor.dev/gvisor/pkg/gfxproto"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl"
)

// The server's virgl contexts, which the virgl package executes: contexts of
// virgl's capability sets, of which classic resources are Gallium's
// resources, and whose fences wait for the GPU.

// isVirglCapset reports whether a context of capability set id is a virgl
// context: of virgl's capability sets, or of none, which Linux's driver
// asks for when the guest initializes no context, and virglrenderer takes
// for virgl's.
func isVirglCapset(id uint32) bool {
	return virgl.Available() && (id == 0 || id == virgl.CapsetVirgl || id == virgl.CapsetVirgl2)
}

// virglContext returns virgl context id, or nil.
func (s *server) virglContext(id uint32) *virgl.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.virgl[id]
}

// newVirglResource returns a Gallium resource of the classic resource that
// p creates, whose guest's memory is mem, or nil if Zink refuses it.
func newVirglResource(p *gfxproto.CreateResource, mem *sharedMemory) *virgl.Resource {
	mem.incRef()
	r, err := virgl.NewResource(p.ResID, virgl.ResourceArgs{
		Target:    p.Target,
		Format:    p.Format,
		Bind:      p.Bind,
		Width:     p.Width,
		Height:    p.Height,
		Depth:     p.Depth,
		ArraySize: p.ArraySize,
		LastLevel: p.LastLevel,
		NrSamples: p.NrSamples,
		Flags:     p.Flags,
	}, mem.data, mem.decRef)
	if err != nil {
		mem.decRef()
		log.Printf("virgl: creating resource %d: %v", p.ResID, err)
		return nil
	}
	return r
}

// transferVirgl executes a transfer of resource r, for context ctxID.
func (s *server) transferVirgl(ctxID uint32, r *resource, p *gfxproto.Transfer) {
	a := virgl.TransferArgs{
		ToHost:      p.ToHost != 0,
		X:           p.X,
		Y:           p.Y,
		Z:           p.Z,
		W:           p.W,
		H:           p.H,
		D:           p.D,
		Level:       p.Level,
		Offset:      uint64(p.Offset),
		Stride:      p.Stride,
		LayerStride: p.LayerStride,
	}
	var err error
	if c := s.virglContext(ctxID); c != nil {
		err = c.Transfer(r.virgl, a)
	} else {
		err = virgl.Transfer(r.virgl, a)
	}
	if err != nil {
		log.Printf("virgl: context %d: %v", ctxID, err)
	}
}

// globalFence signals fence id of the global timeline once what came
// before it is done: Venus's work, which the renderer executed as it came,
// and virgl's, which the GPU executes after.
func (s *server) globalFence(id uint64) {
	s.mu.Lock()
	contexts := make([]*virgl.Context, 0, len(s.virgl))
	for _, c := range s.virgl {
		contexts = append(contexts, c)
	}
	s.mu.Unlock()
	fences := make([]*virgl.Fence, 0, len(contexts))
	for _, c := range contexts {
		fences = append(fences, c.Flush())
	}
	s.queueFence(id, 0, false, 0, fences)
}

// queueFence signals a fence once fences are done, and the fences queued
// before it signaled: a fence signals those before it on its timeline.
//
// Preconditions: the server's goroutine calls it.
func (s *server) queueFence(id uint64, ctxID uint32, hasRing bool, ring uint32, fences []*virgl.Fence) {
	prev := s.lastFence
	if len(fences) == 0 && signaled(prev) {
		s.fenceSignaled(id, ctxID, hasRing, ring)
		return
	}
	done := make(chan struct{})
	s.lastFence = done
	go func() {
		defer close(done)
		for _, f := range fences {
			f.Wait()
		}
		if prev != nil {
			<-prev
		}
		s.fenceSignaled(id, ctxID, hasRing, ring)
	}()
}

// signaled reports whether a queued fence, done, signaled.
func signaled(done chan struct{}) bool {
	if done == nil {
		return true
	}
	select {
	case <-done:
		return true
	default:
		return false
	}
}
