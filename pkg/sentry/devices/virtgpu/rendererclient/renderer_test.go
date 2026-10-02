// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rendererclient

import (
	"os"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/devices/virtgpu"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/test/testutil"
	"gvisor.dev/gvisor/pkg/unet"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer"
)

// startBroker serves ttc-gfx-renderer's broker, as ttcd does, and returns
// the Sentry's Broker of it, and a function that closes the Broker and
// returns what serving it returned. It skips the test if the renderer cannot
// render on this host.
func startBroker(t *testing.T) (*Broker, func() error) {
	t.Helper()
	renderer, err := testutil.FindFile("runsc/ttcgfx/ttc-gfx-renderer")
	if err != nil {
		t.Fatalf("finding the renderer: %v", err)
	}
	opts := gfxrenderer.BrokerOptions{Renderer: renderer, Stderr: os.Stderr}
	if _, err := gfxrenderer.ProbeRenderer(opts); err != nil {
		t.Skipf("the renderer cannot render here: %v", err)
	}
	sentrySock, brokerSock, err := unet.SocketPair(false /* packet */)
	if err != nil {
		t.Fatal(err)
	}
	brokerFD, err := brokerSock.Release()
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() {
		served <- gfxrenderer.ServeBroker(brokerFD, opts)
	}()
	sentryFD, err := sentrySock.Release()
	if err != nil {
		t.Fatal(err)
	}
	broker, err := NewBroker(sentryFD)
	if err != nil {
		t.Fatal(err)
	}
	return broker, func() error {
		broker.Close()
		return <-served
	}
}

// ringSize is the size of a blob that holds a Venus command ring.
const ringSize = 1<<20 + 12<<10

// newRing creates context 1 of the Venus capability set, and blob resource 1,
// shared memory as Venus's driver creates for its rings, which it maps.
func newRing(t *testing.T, r virtgpu.Renderer, name string) virtgpu.ResourceMemory {
	t.Helper()
	if err := r.CreateContext(1, linux.VIRTGPU_DRM_CAPSET_VENUS, name); err != nil {
		t.Fatalf("CreateContext: %v", err)
	}
	if err := r.CreateBlob(1, 1, virtgpu.BlobArgs{
		Mem:   linux.VIRTGPU_BLOB_MEM_HOST3D,
		Flags: linux.VIRTGPU_BLOB_FLAG_USE_MAPPABLE,
		Size:  ringSize,
	}); err != nil {
		t.Fatalf("CreateBlob: %v", err)
	}
	r.AttachResource(1, 1)
	mem, err := r.MapResource(1)
	if err != nil {
		t.Fatalf("MapResource: %v", err)
	}
	return mem
}

// TestRealRenderer drives ttc-gfx-renderer, on the host's GPU.
func TestRealRenderer(t *testing.T) {
	broker, closeBroker := startBroker(t)
	defer func() {
		if err := closeBroker(); err != nil {
			t.Errorf("ServeBroker: %v", err)
		}
	}()
	ev := &events{lost: make(chan error, 1)}
	c, err := broker.NewRenderer(ev)
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	defer c.Close()
	mem := newRing(t, c, "renderer-test")
	copyMemory(t, mem, 0, make([]byte, hostarch.PageSize), true)

	// With nothing submitted, the renderer signals fences at once.
	for _, f := range []virtgpu.Fence{
		{ID: 1, CtxID: 1, HasRing: true, Ring: 0},
		{ID: 2, CtxID: 1},
	} {
		if err := c.CreateFence(f); err != nil {
			t.Fatalf("CreateFence(%+v): %v", f, err)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		ev.mu.Lock()
		n := len(ev.fences)
		ev.mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of 2 fences signaled", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	ev.mu.Lock()
	for _, f := range ev.fences {
		if f.ID == 2 && (f.HasRing || f.CtxID != 0) {
			t.Errorf("the global fence was signaled as %+v", f)
		}
	}
	ev.mu.Unlock()

	c.DetachResource(1, 1)
	c.DestroyResource(1)
	mem.Release()
	c.DestroyContext(1)
	select {
	case err := <-ev.lost:
		t.Fatalf("lost the renderer: %v", err)
	default:
	}
}

// TestBroker connects branches to their family's renderer through the
// broker, as the Sentry does.
func TestBroker(t *testing.T) {
	broker, closeBroker := startBroker(t)
	// Two renderers, as two branches would have.
	for i := 0; i < 2; i++ {
		ev := &events{lost: make(chan error, 1)}
		r, err := broker.NewRenderer(ev)
		if err != nil {
			t.Fatalf("NewRenderer: %v", err)
		}
		if err := r.CreateContext(1, linux.VIRTGPU_DRM_CAPSET_VENUS, "broker-test"); err != nil {
			t.Errorf("CreateContext: %v", err)
		}
		r.Close()
	}
	if err := closeBroker(); err != nil {
		t.Errorf("ServeBroker: %v", err)
	}
}

// TestRealSnapshot takes a snapshot of a branch's renderer, restores it in
// another branch's, and checks that the other's blob holds what the first's
// did.
func TestRealSnapshot(t *testing.T) {
	broker, closeBroker := startBroker(t)
	defer func() {
		if err := closeBroker(); err != nil {
			t.Errorf("ServeBroker: %v", err)
		}
	}()

	// A command ring's blob, whose last page holds a message.
	const offset = ringSize - hostarch.PageSize
	message := []byte("taken with a snapshot")
	source, err := broker.NewRenderer(&events{lost: make(chan error, 1)})
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	defer source.Close()
	sourceMem := newRing(t, source, "snapshot-test")
	defer sourceMem.Release()
	copyMemory(t, sourceMem, offset, message, true)

	snapshot, err := source.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !snapshot.Reference {
		t.Errorf("the renderer of the family took a snapshot of %d bytes, not a reference to a clone", len(snapshot.Data))
	}
	// The source renders on after its snapshot, and its writes are its own.
	if err := source.CreateContext(2, linux.VIRTGPU_DRM_CAPSET_VENUS, "after-snapshot"); err != nil {
		t.Errorf("CreateContext after a snapshot: %v", err)
	}
	copyMemory(t, sourceMem, offset, []byte("changed"), true)

	child, err := broker.NewRenderer(&events{lost: make(chan error, 1)})
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	defer child.Close()
	if err := child.Restore(snapshot.Data, map[uint32]uint64{1: ringSize}, true); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	childMem, err := child.MapResource(1)
	if err != nil {
		t.Fatalf("MapResource after Restore: %v", err)
	}
	defer childMem.Release()
	got := make([]byte, len(message))
	copyMemory(t, childMem, offset, got, false)
	if string(got) != string(message) {
		t.Errorf("the restored blob holds %q, want %q", got, message)
	}
}

// copyMemory copies between buf and the memory of a resource at offset.
func copyMemory(t *testing.T, mem virtgpu.ResourceMemory, offset uint64, buf []byte, toMemory bool) {
	t.Helper()
	fr := memmap.FileRange{Start: mem.Offset() + offset, End: mem.Offset() + offset + uint64(len(buf))}
	ims, err := mem.File().MapInternal(fr, hostarch.ReadWrite)
	if err != nil {
		t.Fatalf("MapInternal: %v", err)
	}
	blocks := safemem.BlockSeqOf(safemem.BlockFromSafeSlice(buf))
	if toMemory {
		_, err = safemem.CopySeq(ims, blocks)
	} else {
		_, err = safemem.CopySeq(blocks, ims)
	}
	if err != nil {
		t.Fatalf("copying: %v", err)
	}
}
