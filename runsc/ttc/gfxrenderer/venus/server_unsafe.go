// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package venus

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/gfxproto"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vk"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vn"
)

// Blob memory and flags, as VIRTGPU_BLOB_MEM_* and VIRTGPU_BLOB_FLAG_*.
const (
	blobMemGuest        = 1
	blobMemHost3D       = 2
	blobFlagMappable    = 1
	blobFlagShareable   = 2
	blobFlagCrossDevice = 4
)

// server renders for one Sentry.
type server struct {
	conn *gfxproto.Conn
	lib  *vk.Library

	mu        sync.Mutex
	contexts  map[uint32]*Context
	resources map[uint32]*resource
}

// Serve renders for the Sentry connected to the Unix stream socket fd, which
// it takes, until the Sentry disconnects.
func Serve(fd int) error {
	conn, err := gfxproto.NewConn(fd)
	if err != nil {
		unix.Close(fd)
		return err
	}
	defer conn.Close()
	lib, err := loadLibrary()
	if err != nil {
		conn.Send(gfxproto.MsgHello, 0, &gfxproto.Hello{Version: gfxproto.Version, Errno: uint32(unix.ENODEV)}, []byte(err.Error()))
		return err
	}
	s := &server{
		conn:      conn,
		lib:       lib,
		contexts:  make(map[uint32]*Context),
		resources: make(map[uint32]*resource),
	}
	defer s.release()
	if err := conn.Send(gfxproto.MsgHello, 0, &gfxproto.Hello{Version: gfxproto.Version}, nil); err != nil {
		return err
	}
	for {
		m, err := conn.Recv()
		if errors.Is(err, gfxproto.ErrClosed) {
			return nil
		}
		if err != nil {
			return err
		}
		err = s.handle(m)
		m.CloseFDs()
		if err != nil {
			return err
		}
	}
}

// Probe checks that the host's Vulkan can render for Venus, and returns
// Venus's capability set.
func Probe() ([]byte, error) {
	lib, err := loadLibrary()
	if err != nil {
		return nil, err
	}
	var pool vn.Pool
	defer pool.Release()
	a := (*vn.Args_vkEnumerateInstanceVersion)(pool.Alloc(unsafe.Sizeof(vn.Args_vkEnumerateInstanceVersion{})))
	version := (*uint32)(pool.Alloc(4))
	a.PApiVersion = unsafe.Pointer(version)
	vk.Call(vn.VK_COMMAND_TYPE_vkEnumerateInstanceVersion_EXT, lib.Global(), unsafe.Pointer(a))
	if a.Ret != vn.VK_SUCCESS || *version < apiVersion(1, 1) {
		return nil, fmt.Errorf("the host's Vulkan instance version is %#x, not 1.1 or later", *version)
	}
	return Capset(), nil
}

// fenceSignaled tells the Sentry that a fence is signaled.
func (s *server) fenceSignaled(fenceID uint64, ctxID uint32, hasRing bool, ring uint32) {
	fs := gfxproto.FenceSignaled{FenceID: fenceID}
	if hasRing {
		fs.CtxID = ctxID
		fs.HasRing = 1
		fs.Ring = ring
	}
	if err := s.conn.Send(gfxproto.MsgFenceSignaled, 0, &fs, nil); err != nil {
		log.Printf("venus: signaling fence %d: %v", fenceID, err)
	}
}

func (s *server) reply(m *gfxproto.Message, reply gfxproto.Reply, fds ...int) error {
	return s.conn.Send(gfxproto.MsgReply, m.Seq, &reply, nil, fds...)
}

func (s *server) context(id uint32) *Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.contexts[id]
}

// handle handles a message. It returns an error only if the connection is
// unusable: the Sentry learns of failed requests from their replies, and
// of other failures from their consequences, as a virtual machine's guest
// does.
func (s *server) handle(m *gfxproto.Message) error {
	switch m.Type {
	case gfxproto.MsgCreateContext:
		var p gfxproto.CreateContext
		name, err := m.Decode(&p)
		if err != nil {
			return err
		}
		errno := uint32(0)
		s.mu.Lock()
		switch {
		case p.CapsetID != CapsetVenus:
			log.Printf("venus: context %d of capability set %d", p.CtxID, p.CapsetID)
			errno = uint32(unix.EINVAL)
		case s.contexts[p.CtxID] != nil:
			errno = uint32(unix.EEXIST)
		default:
			s.contexts[p.CtxID] = newContext(s, p.CtxID, string(name))
		}
		s.mu.Unlock()
		return s.reply(m, gfxproto.Reply{Errno: errno})

	case gfxproto.MsgDestroyContext:
		var p gfxproto.DestroyContext
		if _, err := m.Decode(&p); err != nil {
			return err
		}
		s.mu.Lock()
		c := s.contexts[p.CtxID]
		delete(s.contexts, p.CtxID)
		s.mu.Unlock()
		if c != nil {
			c.destroy()
		}

	case gfxproto.MsgCreateResource:
		var p gfxproto.CreateResource
		if _, err := m.Decode(&p); err != nil {
			return err
		}
		// A classic resource is shared memory that the guest writes and the
		// renderer reads as it is: transfers move nothing.
		mem, err := newSharedMemory(p.Size)
		if err != nil {
			log.Printf("venus: creating resource %d of %d bytes: %v", p.ResID, p.Size, err)
			return nil
		}
		s.addResource(p.CtxID, &resource{id: p.ResID, size: p.Size, mapInfo: mapCacheCached, mem: mem})

	case gfxproto.MsgCreateBlob:
		var p gfxproto.CreateBlob
		if _, err := m.Decode(&p); err != nil {
			return err
		}
		if err := s.createBlob(&p); err != nil {
			log.Printf("venus: creating blob %d (memory %d, flags %#x, ID %d, %d bytes): %v", p.ResID, p.Mem, p.Flags, p.BlobID, p.Size, err)
		}

	case gfxproto.MsgMapResource:
		var p gfxproto.MapResource
		if _, err := m.Decode(&p); err != nil {
			return err
		}
		s.mu.Lock()
		r := s.resources[p.ResID]
		s.mu.Unlock()
		if r == nil {
			log.Printf("venus: mapping resource %d: no resource", p.ResID)
			return s.reply(m, gfxproto.Reply{Errno: uint32(unix.EINVAL)})
		}
		return s.reply(m, gfxproto.Reply{Size: uint64(len(r.mem.data)), MapInfo: r.mapInfo}, r.mem.fd)

	case gfxproto.MsgAttachResource:
		var p gfxproto.AttachResource
		if _, err := m.Decode(&p); err != nil {
			return err
		}
		s.mu.Lock()
		r := s.resources[p.ResID]
		c := s.contexts[p.CtxID]
		s.mu.Unlock()
		if r != nil && c != nil {
			c.attach(r)
		}

	case gfxproto.MsgDetachResource:
		var p gfxproto.DetachResource
		if _, err := m.Decode(&p); err != nil {
			return err
		}
		if c := s.context(p.CtxID); c != nil {
			c.detach(p.ResID)
		}

	case gfxproto.MsgDestroyResource:
		var p gfxproto.DestroyResource
		if _, err := m.Decode(&p); err != nil {
			return err
		}
		s.mu.Lock()
		r := s.resources[p.ResID]
		delete(s.resources, p.ResID)
		contexts := make([]*Context, 0, len(s.contexts))
		for _, c := range s.contexts {
			contexts = append(contexts, c)
		}
		s.mu.Unlock()
		if r == nil {
			return nil
		}
		for _, c := range contexts {
			c.detach(p.ResID)
		}
		r.mem.decRef()

	case gfxproto.MsgSubmitCommand:
		var p gfxproto.SubmitCommand
		cmd, err := m.Decode(&p)
		if err != nil {
			return err
		}
		c := s.context(p.CtxID)
		if c == nil {
			log.Printf("venus: submitting to context %d: no context", p.CtxID)
			return nil
		}
		if err := c.submit(cmd); err != nil {
			c.logf("submitting %d bytes: %v", len(cmd), err)
		}

	case gfxproto.MsgTransfer:
		var p gfxproto.Transfer
		if _, err := m.Decode(&p); err != nil {
			return err
		}
		// The renderer reads classic resources where the guest writes them.

	case gfxproto.MsgCreateFence:
		var p gfxproto.CreateFence
		if _, err := m.Decode(&p); err != nil {
			return err
		}
		if p.HasRing == 0 {
			// The global timeline: what came before the fence is executed.
			s.fenceSignaled(p.FenceID, 0, false, 0)
			return nil
		}
		c := s.context(p.CtxID)
		if c == nil {
			return fmt.Errorf("fence %d of context %d: no context", p.FenceID, p.CtxID)
		}
		if err := c.createFence(p.FenceID, p.Ring); err != nil {
			// The fence would never be signaled.
			return fmt.Errorf("creating fence %d: %w", p.FenceID, err)
		}

	case gfxproto.MsgSnapshot:
		return s.reply(m, gfxproto.Reply{Errno: uint32(unix.ENOTSUP)})

	case gfxproto.MsgRestoreData:

	case gfxproto.MsgRestore:
		return s.reply(m, gfxproto.Reply{Errno: uint32(unix.ENOTSUP)})

	default:
		return fmt.Errorf("unexpected %v", m.Type)
	}
	return nil
}

// addResource adds r, which ctxID created and may use.
func (s *server) addResource(ctxID uint32, r *resource) {
	s.mu.Lock()
	old := s.resources[r.id]
	s.resources[r.id] = r
	c := s.contexts[ctxID]
	s.mu.Unlock()
	if old != nil {
		old.mem.decRef()
	}
	if c != nil {
		c.attach(r)
	}
}

// createBlob creates a blob resource: shared memory that the guest asks for
// with ID 0, as rings and reply streams, or a VkDeviceMemory's.
func (s *server) createBlob(p *gfxproto.CreateBlob) error {
	if p.Mem != blobMemHost3D {
		return fmt.Errorf("blob memory %d", p.Mem)
	}
	if p.BlobID == 0 {
		if p.Flags != blobFlagMappable {
			return fmt.Errorf("a blob of ID 0 with flags %#x", p.Flags)
		}
		mem, err := newSharedMemory(p.Size)
		if err != nil {
			return err
		}
		s.addResource(p.CtxID, &resource{id: p.ResID, size: p.Size, mapInfo: mapCacheCached, mem: mem})
		return nil
	}
	c := s.context(p.CtxID)
	if c == nil {
		return fmt.Errorf("no context %d", p.CtxID)
	}
	mem, mapInfo, err := c.exportMemory(p.BlobID, p.Size, p.Flags&blobFlagMappable != 0)
	if err != nil {
		return err
	}
	s.addResource(p.CtxID, &resource{id: p.ResID, size: p.Size, mapInfo: mapInfo, mem: mem})
	return nil
}

// release destroys the server's contexts and resources.
func (s *server) release() {
	s.mu.Lock()
	contexts := s.contexts
	resources := s.resources
	s.contexts = nil
	s.resources = nil
	s.mu.Unlock()
	for _, c := range contexts {
		c.destroy()
	}
	for _, r := range resources {
		r.mem.decRef()
	}
}
