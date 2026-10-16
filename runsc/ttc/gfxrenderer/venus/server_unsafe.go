// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package venus

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/gfxproto"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vk"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vn"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl"
)

// Blob memory and flags, as VIRTGPU_BLOB_MEM_* and VIRTGPU_BLOB_FLAG_*.
const (
	blobMemGuest        = 1
	blobMemHost3D       = 2
	blobFlagMappable    = 1
	blobFlagShareable   = 2
	blobFlagCrossDevice = 4
)

// server renders for one branch of a Sentry's family.
type server struct {
	conn *gfxproto.Conn
	lib  *vk.Library

	mu        sync.Mutex
	contexts  map[uint32]*Context
	resources map[uint32]*resource
	// virgl are the server's virgl contexts (see virgl.go).
	virgl map[uint32]*virgl.Context

	// lastFence is closed once the last fence that queueFence queued
	// signaled. Only the server's goroutine uses it.
	lastFence chan struct{}

	// restoring is the snapshot that MsgRestoreData sent, for MsgRestore.
	restoring []byte
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
		virgl:     make(map[uint32]*virgl.Context),
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
		createVirgl := false
		s.mu.Lock()
		switch {
		case p.CapsetID != CapsetVenus && p.CapsetID != CapsetCrossDomain && !isVirglCapset(p.CapsetID):
			log.Printf("venus: context %d of capability set %d", p.CtxID, p.CapsetID)
			errno = uint32(unix.EINVAL)
		case s.contexts[p.CtxID] != nil || s.virgl[p.CtxID] != nil:
			errno = uint32(unix.EEXIST)
		case isVirglCapset(p.CapsetID):
			// Only the server's goroutine creates contexts.
			createVirgl = true
		default:
			c := newContext(s, p.CtxID, string(name))
			if p.CapsetID == CapsetCrossDomain {
				c.crossDomain = newCrossDomain()
			}
			s.contexts[p.CtxID] = c
		}
		s.mu.Unlock()
		if createVirgl {
			if c, err := virgl.NewContext(p.CtxID); err != nil {
				log.Printf("virgl: creating context %d (%s): %v", p.CtxID, name, err)
				errno = uint32(unix.EINVAL)
			} else {
				s.mu.Lock()
				s.virgl[p.CtxID] = c
				s.mu.Unlock()
			}
		}
		return s.reply(m, gfxproto.Reply{Errno: errno})

	case gfxproto.MsgDestroyContext:
		var p gfxproto.DestroyContext
		if _, err := m.Decode(&p); err != nil {
			return err
		}
		s.mu.Lock()
		c := s.contexts[p.CtxID]
		delete(s.contexts, p.CtxID)
		vc := s.virgl[p.CtxID]
		delete(s.virgl, p.CtxID)
		s.mu.Unlock()
		if c != nil {
			c.destroy()
		}
		if vc != nil {
			vc.Destroy()
		}

	case gfxproto.MsgCreateResource:
		var p gfxproto.CreateResource
		if _, err := m.Decode(&p); err != nil {
			return err
		}
		// A classic resource is shared memory that the guest writes and the
		// renderer reads as it is: for Venus, transfers move nothing. A virgl
		// context's is also a Gallium resource, which transfers copy to and
		// from the memory.
		mem, err := newSharedMemory(p.Size)
		if err != nil {
			log.Printf("venus: creating resource %d of %d bytes: %v", p.ResID, p.Size, err)
			return nil
		}
		r := &resource{id: p.ResID, size: p.Size, mapInfo: mapCacheCached, mem: mem}
		if s.virglContext(p.CtxID) != nil {
			r.virgl = newVirglResource(&p, mem)
		}
		s.addResource(p.CtxID, r)

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
		vc := s.virgl[p.CtxID]
		s.mu.Unlock()
		if r != nil && c != nil {
			c.attach(r)
		}
		if r != nil && r.virgl != nil && vc != nil {
			vc.Attach(r.virgl)
		}

	case gfxproto.MsgDetachResource:
		var p gfxproto.DetachResource
		if _, err := m.Decode(&p); err != nil {
			return err
		}
		if c := s.context(p.CtxID); c != nil {
			c.detach(p.ResID)
		}
		if vc := s.virglContext(p.CtxID); vc != nil {
			vc.Detach(p.ResID)
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
		virglContexts := make([]*virgl.Context, 0, len(s.virgl))
		for _, c := range s.virgl {
			virglContexts = append(virglContexts, c)
		}
		s.mu.Unlock()
		if r == nil {
			return nil
		}
		for _, c := range contexts {
			c.detach(p.ResID)
		}
		for _, c := range virglContexts {
			c.Detach(p.ResID)
		}
		r.release()

	case gfxproto.MsgSubmitCommand:
		var p gfxproto.SubmitCommand
		cmd, err := m.Decode(&p)
		if err != nil {
			return err
		}
		if vc := s.virglContext(p.CtxID); vc != nil {
			if err := vc.Submit(cmd); err != nil {
				log.Printf("virgl: context %d: submitting %d bytes: %v", p.CtxID, len(cmd), err)
			}
			return nil
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
		// Venus's renderer reads classic resources where the guest writes
		// them; virgl's copies them to and from Gallium's resources.
		s.mu.Lock()
		r := s.resources[p.ResID]
		s.mu.Unlock()
		if r != nil && r.virgl != nil {
			s.transferVirgl(p.CtxID, r, &p)
		}

	case gfxproto.MsgCreateFence:
		var p gfxproto.CreateFence
		if _, err := m.Decode(&p); err != nil {
			return err
		}
		if p.HasRing == 0 {
			s.globalFence(p.FenceID)
			return nil
		}
		if vc := s.virglContext(p.CtxID); vc != nil {
			s.queueFence(p.FenceID, p.CtxID, true, p.Ring, []*virgl.Fence{vc.Flush()})
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
		token, err := s.snapshot()
		if err != nil {
			log.Printf("venus: taking a snapshot for a fork: %v", err)
			return s.reply(m, gfxproto.Reply{Errno: uint32(unix.EIO)})
		}
		fd, err := pipeOf(token)
		if err != nil {
			return s.reply(m, gfxproto.Reply{Errno: uint32(unix.EIO)})
		}
		err = s.reply(m, gfxproto.Reply{Size: uint64(len(token)), Flags: gfxproto.SnapshotFlagReference}, fd)
		unix.Close(fd)
		return err

	case gfxproto.MsgRestoreData:
		var p gfxproto.RestoreData
		data, err := m.Decode(&p)
		if err != nil {
			return err
		}
		if len(s.restoring)+len(data) > maxTokenSize {
			return fmt.Errorf("a snapshot of more than %d bytes", maxTokenSize)
		}
		s.restoring = append(s.restoring, data...)

	case gfxproto.MsgRestore:
		var p gfxproto.Restore
		if _, err := m.Decode(&p); err != nil {
			return err
		}
		token := s.restoring
		s.restoring = nil
		if err := s.restore(token, p.Flags&gfxproto.RestoreFlagLast != 0); err != nil {
			log.Printf("venus: restoring a snapshot: %v", err)
			return s.reply(m, gfxproto.Reply{Errno: uint32(unix.EINVAL)})
		}
		return s.reply(m, gfxproto.Reply{})

	case gfxproto.MsgForgetSnapshot:
		var p gfxproto.ForgetSnapshot
		token, err := m.Decode(&p)
		if err != nil {
			return err
		}
		forgetClone(string(token))

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
	vc := s.virgl[ctxID]
	s.mu.Unlock()
	if old != nil {
		old.release()
	}
	if c != nil {
		c.attach(r)
	}
	if vc != nil && r.virgl != nil {
		vc.Attach(r.virgl)
	}
}

// createBlob creates a blob resource: shared memory that the guest asks for
// with ID 0, as rings and reply streams, or of the guest's memory, a
// VkDeviceMemory's, or an image's that a cross-domain context allocated.
func (s *server) createBlob(p *gfxproto.CreateBlob) error {
	c := s.context(p.CtxID)
	if c != nil && c.crossDomain != nil && p.BlobID != 0 {
		// An image that the context allocated, as the host's memory or,
		// for minigbm built against ChromeOS's headers, the guest's with a
		// host handle, which are alike here.
		mem, err := c.crossDomain.newBlob(uint32(p.BlobID), p.Size)
		if err != nil {
			return err
		}
		s.addResource(p.CtxID, &resource{id: p.ResID, size: p.Size, mapInfo: mapCacheCached, mem: mem})
		return nil
	}
	if p.Mem == blobMemGuest {
		// The renderer's shared memory is the guest's memory, which the
		// Sentry maps as it maps the host's.
		mem, err := newSharedMemory(p.Size)
		if err != nil {
			return err
		}
		s.addResource(p.CtxID, &resource{id: p.ResID, size: p.Size, mapInfo: mapCacheCached, mem: mem})
		return nil
	}
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
	if c == nil {
		return fmt.Errorf("no context %d", p.CtxID)
	}
	mem, mapInfo, err := c.exportMemory(p.BlobID, p.Size, p.Flags&blobFlagMappable != 0)
	if err != nil {
		return err
	}
	s.addResource(p.CtxID, &resource{id: p.ResID, size: p.Size, mapInfo: mapInfo, mem: mem, fromCtx: c, fromMemory: p.BlobID, seq: journalSeq.Add(1)})
	return nil
}

// release destroys the server's contexts and resources.
func (s *server) release() {
	s.mu.Lock()
	contexts := s.contexts
	resources := s.resources
	virglContexts := s.virgl
	s.contexts = nil
	s.resources = nil
	s.virgl = nil
	s.mu.Unlock()
	for _, c := range contexts {
		c.destroy()
	}
	for _, c := range virglContexts {
		c.Destroy()
	}
	for _, r := range resources {
		r.release()
	}
}

// Snapshots of the Venus renderer, for forks, are references to clones that
// the renderer of the family keeps: a token.
const (
	tokenPrefix  = "TTC-VENUS-CLONE:"
	maxTokenSize = 64
)

// clones are the clones that forks' children have yet to adopt, by token.
var clones = struct {
	mu sync.Mutex
	m  map[string]*pendingClone
}{m: make(map[string]*pendingClone)}

// PendingClones returns the number of clones that forks' children have yet
// to adopt, which keep the renderer of a family from exiting.
func PendingClones() int {
	clones.mu.Lock()
	defer clones.mu.Unlock()
	return len(clones.m)
}

// pendingClone is a clone that forks' children adopt. The last takes it;
// the others clone it again.
type pendingClone struct {
	mu sync.Mutex
	st *cloneState
}

// snapshot clones the server's contexts and resources, which the Sentry
// quiesced, for a fork's children, and returns its token.
func (s *server) snapshot() ([]byte, error) {
	s.mu.Lock()
	contexts := make(map[uint32]*Context, len(s.contexts))
	for id, c := range s.contexts {
		contexts[id] = c
	}
	resources := make(map[uint32]*resource, len(s.resources))
	for id, r := range s.resources {
		resources[id] = r
	}
	virglContexts := make(map[uint32]*virgl.Context, len(s.virgl))
	for id, c := range s.virgl {
		virglContexts[id] = c
	}
	s.mu.Unlock()
	resume, err := quiesce(contexts)
	if err != nil {
		return nil, err
	}
	defer resume()
	start := time.Now()
	st, err := cloneContexts(contexts, resources, virglContexts)
	if err != nil {
		return nil, err
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		st.release()
		return nil, err
	}
	token := tokenPrefix + hex.EncodeToString(id[:])
	clones.mu.Lock()
	clones.m[token] = &pendingClone{st: st}
	clones.mu.Unlock()
	log.Printf("venus: cloned %d contexts, %d of virgl's, and %d resources for a fork in %v", len(st.contexts), len(st.virgl), len(st.resources), time.Since(start))
	return []byte(token), nil
}

// restore gives the server, which has no contexts or resources, the clone
// of token: the clone itself if last, or another clone of it.
func (s *server) restore(token []byte, last bool) error {
	s.mu.Lock()
	empty := len(s.contexts) == 0 && len(s.resources) == 0 && len(s.virgl) == 0
	s.mu.Unlock()
	if !empty {
		return fmt.Errorf("the renderer has contexts or resources")
	}
	clones.mu.Lock()
	pc := clones.m[string(token)]
	if pc != nil && last {
		delete(clones.m, string(token))
	}
	clones.mu.Unlock()
	if pc == nil {
		return fmt.Errorf("no clone %q", token)
	}
	pc.mu.Lock()
	defer pc.mu.Unlock()
	st := pc.st
	if !last {
		var err error
		if st, err = cloneContexts(pc.st.contexts, pc.st.resources, pc.st.virgl); err != nil {
			return err
		}
	}
	s.adopt(st)
	return nil
}

// forgetClone drops the clone of token, which no child will adopt.
func forgetClone(token string) {
	clones.mu.Lock()
	pc := clones.m[token]
	delete(clones.m, token)
	clones.mu.Unlock()
	if pc != nil {
		pc.mu.Lock()
		pc.st.release()
		pc.mu.Unlock()
	}
}

// pipeOf returns the read end of a pipe that holds data.
func pipeOf(data []byte) (int, error) {
	var fds [2]int
	if err := unix.Pipe(fds[:]); err != nil {
		return -1, err
	}
	// A token fits in the pipe's buffer.
	_, err := unix.Write(fds[1], data)
	unix.Close(fds[1])
	if err != nil {
		unix.Close(fds[0])
		return -1, err
	}
	return fds[0], nil
}
