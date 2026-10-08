// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rendererclient

import (
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/gfxproto"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/devices/virtgpu"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/unet"
)

// events records what a Client reports.
type events struct {
	mu     sync.Mutex
	fences []virtgpu.Fence
	lost   chan error
}

func (e *events) FenceSignaled(f virtgpu.Fence) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.fences = append(e.fences, f)
}

func (e *events) RendererLost(err error) {
	e.lost <- err
}

// startRenderer returns the Sentry's end of a connection whose renderer end
// serve handles, after it sends hello.
func startRenderer(t *testing.T, hello gfxproto.Hello, serve func(*gfxproto.Conn)) int {
	t.Helper()
	a, b, err := unet.SocketPair(false /* packet */)
	if err != nil {
		t.Fatal(err)
	}
	sentryFD, err := a.Release()
	if err != nil {
		t.Fatal(err)
	}
	rendererFD, err := b.Release()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := gfxproto.NewConn(rendererFD)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		defer conn.Close()
		if err := conn.Send(gfxproto.MsgHello, 0, &hello, []byte("no GPU")); err != nil {
			return
		}
		serve(conn)
	}()
	return sentryFD
}

func TestClient(t *testing.T) {
	shm, err := os.CreateTemp(t.TempDir(), "blob")
	if err != nil {
		t.Fatal(err)
	}
	defer shm.Close()
	const size = 4 * hostarch.PageSize
	// The renderer's memory is whole host pages, which the client maps: 64
	// KiB on some arm64 Linux hosts.
	hostPage := int64(unix.Getpagesize())
	if err := shm.Truncate((size + hostPage - 1) &^ (hostPage - 1)); err != nil {
		t.Fatal(err)
	}
	var received []gfxproto.MsgType
	var receivedMu sync.Mutex
	fd := startRenderer(t, gfxproto.Hello{Version: gfxproto.Version}, func(conn *gfxproto.Conn) {
		for {
			m, err := conn.Recv()
			if err != nil {
				return
			}
			receivedMu.Lock()
			received = append(received, m.Type)
			receivedMu.Unlock()
			switch m.Type {
			case gfxproto.MsgCreateContext:
				var cc gfxproto.CreateContext
				m.Decode(&cc)
				errno := uint32(0)
				if cc.CapsetID != 3 {
					errno = uint32(unix.EINVAL)
				}
				conn.Send(gfxproto.MsgReply, m.Seq, &gfxproto.Reply{Errno: errno}, nil)
			case gfxproto.MsgMapResource:
				conn.Send(gfxproto.MsgReply, m.Seq, &gfxproto.Reply{Size: size}, nil, int(shm.Fd()))
			case gfxproto.MsgCreateFence:
				var cf gfxproto.CreateFence
				m.Decode(&cf)
				conn.Send(gfxproto.MsgFenceSignaled, 0, &gfxproto.FenceSignaled{FenceID: cf.FenceID, CtxID: cf.CtxID, HasRing: cf.HasRing, Ring: cf.Ring}, nil)
			case gfxproto.MsgDestroyContext:
				// The test's last message: hang up.
				return
			}
		}
	})
	ev := &events{lost: make(chan error, 1)}
	c, err := New(fd, ev)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := c.CreateContext(1, 3, "test"); err != nil {
		t.Errorf("CreateContext: %v", err)
	}
	if err := c.CreateContext(2, 4, "venus"); !errors.Is(err, unix.EINVAL) {
		t.Errorf("CreateContext of an unsupported capability set: %v, want EINVAL", err)
	}
	if err := c.CreateBlob(1, 5, virtgpu.BlobArgs{Mem: 2, Flags: 1, Size: size}); err != nil {
		t.Errorf("CreateBlob: %v", err)
	}
	mem, err := c.MapResource(5)
	if err != nil {
		t.Fatalf("MapResource: %v", err)
	}
	// The Sentry's view of the memory is the renderer's.
	fr := memmap.FileRange{Start: mem.Offset() + hostarch.PageSize, End: mem.Offset() + 2*hostarch.PageSize}
	mem.File().IncRef(fr, 0)
	ims, err := mem.File().MapInternal(fr, hostarch.ReadWrite)
	if err != nil {
		t.Fatalf("MapInternal: %v", err)
	}
	if _, err := safemem.CopySeq(ims, safemem.BlockSeqOf(safemem.BlockFromSafeSlice([]byte("from the Sentry")))); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 15)
	if _, err := shm.ReadAt(buf, hostarch.PageSize); err != nil || string(buf) != "from the Sentry" {
		t.Errorf("the renderer reads %q, %v", buf, err)
	}
	if addr, err := mem.File().(memmap.HostMemoryFile).HostAddress(fr); err != nil || addr == 0 {
		t.Errorf("HostAddress: %#x, %v", addr, err)
	}
	mem.Release()
	mem.File().DecRef(fr)
	if _, err := c.MapResource(6); err == nil {
		t.Error("mapped a resource that the client did not create")
	}

	if err := c.CreateFence(virtgpu.Fence{ID: 9, CtxID: 1, HasRing: true, Ring: 1}); err != nil {
		t.Fatalf("CreateFence: %v", err)
	}
	c.DestroyContext(1)
	select {
	case err := <-ev.lost:
		if !errors.Is(err, gfxproto.ErrClosed) {
			t.Errorf("lost the renderer: %v, want ErrClosed", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the renderer's hang up was not reported")
	}
	ev.mu.Lock()
	if len(ev.fences) != 1 || ev.fences[0] != (virtgpu.Fence{ID: 9, CtxID: 1, HasRing: true, Ring: 1}) {
		t.Errorf("fences signaled: %+v", ev.fences)
	}
	ev.mu.Unlock()
	if err := c.CreateContext(3, 3, "after"); err == nil {
		t.Error("CreateContext succeeded after the renderer hung up")
	}
	c.Close()
}

func TestRendererDidNotStart(t *testing.T) {
	fd := startRenderer(t, gfxproto.Hello{Version: gfxproto.Version, Errno: uint32(unix.ENODEV)}, func(*gfxproto.Conn) {})
	_, err := New(fd, &events{lost: make(chan error, 1)})
	if !errors.Is(err, unix.ENODEV) {
		t.Errorf("New: %v, want ENODEV", err)
	}
}

func TestCloseIsNotLoss(t *testing.T) {
	fd := startRenderer(t, gfxproto.Hello{Version: gfxproto.Version}, func(conn *gfxproto.Conn) {
		for {
			if _, err := conn.Recv(); err != nil {
				return
			}
		}
	})
	ev := &events{lost: make(chan error, 1)}
	c, err := New(fd, ev)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	select {
	case err := <-ev.lost:
		t.Errorf("Close reported a loss: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestHostMemoryRelease(t *testing.T) {
	shm, err := os.CreateTemp(t.TempDir(), "blob")
	if err != nil {
		t.Fatal(err)
	}
	defer shm.Close()
	hostPage := int64(unix.Getpagesize())
	if err := shm.Truncate(hostPage); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Dup(int(shm.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	m, err := newHostMemory(fd, uint64(hostPage))
	if err != nil {
		t.Fatalf("newHostMemory: %v", err)
	}
	fr := memmap.FileRange{Start: 0, End: hostarch.PageSize}
	m.IncRef(fr, 0)
	released := 0
	m.OnRelease(func() {
		// The memory is still mapped.
		if _, err := m.HostAddress(fr); err != nil {
			t.Errorf("HostAddress in a release hook: %v", err)
		}
		released++
	})
	m.Release()
	if released != 0 {
		t.Fatal("released memory whose pages are referenced")
	}
	m.DecRef(fr)
	if released != 1 {
		t.Errorf("release hooks ran %d times, want 1", released)
	}
	// Memory that is not whole host pages is refused.
	if hostPage > hostarch.PageSize {
		fd, err := unix.Dup(int(shm.Fd()))
		if err != nil {
			t.Fatal(err)
		}
		if err := shm.Truncate(hostPage - hostarch.PageSize); err != nil {
			t.Fatal(err)
		}
		if _, err := newHostMemory(fd, uint64(hostPage-hostarch.PageSize)); err == nil {
			t.Error("mapped shared memory shorter than a host page")
		}
		unix.Close(fd)
	}
}
