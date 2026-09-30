// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rendererclient

import (
	"os"
	"os/exec"
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

// TestRealRenderer drives ttc-gfx-renderer with gfxstream, whose host library
// TTC_GFXSTREAM_LIBRARY names, on the host's GPU.
func TestRealRenderer(t *testing.T) {
	library := os.Getenv("TTC_GFXSTREAM_LIBRARY")
	if library == "" {
		t.Skip("TTC_GFXSTREAM_LIBRARY does not name gfxstream's host library")
	}
	renderer, err := testutil.FindFile("runsc/ttcgfx/ttc-gfx-renderer")
	if err != nil {
		t.Fatalf("finding the renderer: %v", err)
	}
	sentrySock, rendererSock, err := unet.SocketPair(false /* packet */)
	if err != nil {
		t.Fatal(err)
	}
	rendererFD, err := rendererSock.Release()
	if err != nil {
		t.Fatal(err)
	}
	rendererFile := os.NewFile(uintptr(rendererFD), "renderer")
	cmd := exec.Command(renderer, "serve", "--library", library)
	cmd.ExtraFiles = []*os.File{rendererFile}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the renderer: %v", err)
	}
	rendererFile.Close()
	defer cmd.Wait()

	sentryFD, err := sentrySock.Release()
	if err != nil {
		t.Fatal(err)
	}
	ev := &events{lost: make(chan error, 1)}
	c, err := New(sentryFD, ev)
	if err != nil {
		t.Fatalf("connecting to the renderer: %v", err)
	}
	defer c.Close()

	if err := c.CreateContext(1, linux.VIRTGPU_DRM_CAPSET_GFXSTREAM_VULKAN, "renderer-test"); err != nil {
		t.Fatalf("CreateContext: %v", err)
	}
	// The guest driver's command ring: a blob without a host allocation.
	const ringSize = 1<<20 + 12<<10
	if err := c.CreateBlob(1, 1, virtgpu.BlobArgs{
		Mem:   linux.VIRTGPU_BLOB_MEM_HOST3D,
		Flags: linux.VIRTGPU_BLOB_FLAG_USE_MAPPABLE | linux.VIRTGPU_BLOB_FLAG_USE_SHAREABLE,
		Size:  ringSize,
	}); err != nil {
		t.Fatalf("CreateBlob: %v", err)
	}
	c.AttachResource(1, 1)
	mem, err := c.MapResource(1)
	if err != nil {
		t.Fatalf("MapResource: %v", err)
	}
	fr := memmap.FileRange{Start: mem.Offset(), End: mem.Offset() + hostarch.PageSize}
	mem.File().IncRef(fr, 0)
	ims, err := mem.File().MapInternal(fr, hostarch.ReadWrite)
	if err != nil {
		t.Fatalf("MapInternal: %v", err)
	}
	if _, err := safemem.ZeroSeq(ims); err != nil {
		t.Fatalf("writing the ring: %v", err)
	}

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
	mem.File().DecRef(fr)
	mem.Release()
	c.DestroyContext(1)
	select {
	case err := <-ev.lost:
		t.Fatalf("lost the renderer: %v", err)
	default:
	}
}

// TestBroker starts a renderer through the broker, as the Sentry does.
func TestBroker(t *testing.T) {
	library := os.Getenv("TTC_GFXSTREAM_LIBRARY")
	if library == "" {
		t.Skip("TTC_GFXSTREAM_LIBRARY does not name gfxstream's host library")
	}
	renderer, err := testutil.FindFile("runsc/ttcgfx/ttc-gfx-renderer")
	if err != nil {
		t.Fatalf("finding the renderer: %v", err)
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
		served <- gfxrenderer.ServeBroker(brokerFD, gfxrenderer.BrokerOptions{
			Renderer: renderer,
			Options:  gfxrenderer.Options{Library: library},
			Stderr:   os.Stderr,
		})
	}()
	sentryFD, err := sentrySock.Release()
	if err != nil {
		t.Fatal(err)
	}
	broker, err := NewBroker(sentryFD)
	if err != nil {
		t.Fatal(err)
	}
	// Two renderers, as two branches would have.
	for i := 0; i < 2; i++ {
		ev := &events{lost: make(chan error, 1)}
		r, err := broker.NewRenderer(ev)
		if err != nil {
			t.Fatalf("NewRenderer: %v", err)
		}
		if err := r.CreateContext(1, linux.VIRTGPU_DRM_CAPSET_GFXSTREAM_VULKAN, "broker-test"); err != nil {
			t.Errorf("CreateContext: %v", err)
		}
		r.Close()
	}
	broker.Close()
	if err := <-served; err != nil {
		t.Errorf("ServeBroker: %v", err)
	}
}
