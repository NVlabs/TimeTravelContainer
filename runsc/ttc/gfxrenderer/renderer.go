// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package gfxrenderer

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/gfxproto"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus"
)

// start loads and starts gfxstream, which writes the fences it signals to
// fenceFD.
func start(opts Options, fenceFD int) (*gfxstream, error) {
	setVulkanLoader()
	g, err := openGfxstream(opts.library())
	if err != nil {
		return nil, err
	}
	features := opts.Features
	if opts.Snapshots {
		features = strings.Trim(features+","+snapshotFeatures, ",")
	}
	if err := g.init(features, fenceFD); err != nil {
		return nil, err
	}
	return g, nil
}

// maxCapsetID is the largest capability set ID that Probe asks about.
const maxCapsetID = 8

// Probe starts the renderer's backend, and returns its capability sets.
func Probe(opts Options) ([]Capset, error) {
	if opts.Backend == BackendVenus {
		data, err := venus.Probe()
		if err != nil {
			return nil, err
		}
		return []Capset{{ID: venus.CapsetVenus, Data: data}}, nil
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	defer w.Close()
	g, err := start(opts, int(w.Fd()))
	if err != nil {
		return nil, err
	}
	defer g.teardown()
	var capsets []Capset
	for id := uint32(1); id <= maxCapsetID; id++ {
		version, data := g.capset(id)
		if len(data) != 0 {
			capsets = append(capsets, Capset{ID: id, MaxVersion: version, Data: data})
		}
	}
	return capsets, nil
}

// resource is a resource of the renderer.
type resource struct {
	// fd is the descriptor of the resource's memory once the renderer has
	// it: a classic resource's backing, or an exported blob's memory.
	fd int
	// backing is the renderer's mapping of a classic resource's backing.
	backing []byte
	// blob is whether the resource is a blob.
	blob bool
}

// server renders for one Sentry.
type server struct {
	conn      *gfxproto.Conn
	g         *gfxstream
	resources map[uint32]*resource
	opts      Options
	// restoring is the snapshot that MsgRestoreData sent, for MsgRestore.
	restoring []byte
}

// Serve renders for the Sentry connected to the Unix stream socket fd, which
// it takes, until the Sentry disconnects.
func Serve(fd int, opts Options) error {
	if opts.Backend == BackendVenus {
		return venus.Serve(fd)
	}
	conn, err := gfxproto.NewConn(fd)
	if err != nil {
		unix.Close(fd)
		return err
	}
	defer conn.Close()
	fenceR, fenceW, err := os.Pipe()
	if err != nil {
		return err
	}
	defer fenceW.Close()
	g, err := start(opts, int(fenceW.Fd()))
	if err != nil {
		conn.Send(gfxproto.MsgHello, 0, &gfxproto.Hello{Version: gfxproto.Version, Errno: uint32(unix.ENODEV)}, []byte(err.Error()))
		fenceR.Close()
		return err
	}
	s := &server{
		conn:      conn,
		g:         g,
		resources: make(map[uint32]*resource),
		opts:      opts,
	}
	// Free resources' memory once gfxstream no longer uses it.
	defer func() {
		g.teardown()
		s.releaseAll()
	}()
	if err := conn.Send(gfxproto.MsgHello, 0, &gfxproto.Hello{Version: gfxproto.Version}, nil); err != nil {
		fenceR.Close()
		return err
	}
	go forwardFences(conn, fenceR)
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

// forwardFences sends the Sentry the fences that gfxstream signals.
func forwardFences(conn *gfxproto.Conn, r *os.File) {
	defer r.Close()
	var rec [fenceRecordSize]byte
	for {
		if _, err := io.ReadFull(r, rec[:]); err != nil {
			return
		}
		flags := binary.NativeEndian.Uint32(rec[16:])
		fs := gfxproto.FenceSignaled{
			FenceID: binary.NativeEndian.Uint64(rec[0:]),
			CtxID:   binary.NativeEndian.Uint32(rec[8:]),
			Ring:    binary.NativeEndian.Uint32(rec[12:]),
		}
		// STREAM_RENDERER_FLAG_FENCE_RING_IDX.
		if flags&(1<<1) != 0 {
			fs.HasRing = 1
		} else {
			fs.CtxID = 0
			fs.Ring = 0
		}
		if err := conn.Send(gfxproto.MsgFenceSignaled, 0, &fs, nil); err != nil {
			return
		}
	}
}

// reply answers the request m.
func (s *server) reply(m *gfxproto.Message, reply gfxproto.Reply, fds ...int) error {
	return s.conn.Send(gfxproto.MsgReply, m.Seq, &reply, nil, fds...)
}

// errno converts gfxstream's negative errno, or other failure, to an errno.
func errno(ret int) uint32 {
	switch {
	case ret == 0:
		return 0
	case ret < 0 && ret > -4096:
		return uint32(-ret)
	default:
		return uint32(unix.EINVAL)
	}
}

// handle handles a message. It returns an error only if the connection is
// unusable: the Sentry learns of a failed request from its reply, and of
// other failures from their consequences, as a virtual machine's guest does.
func (s *server) handle(m *gfxproto.Message) error {
	switch m.Type {
	case gfxproto.MsgCreateContext:
		var p gfxproto.CreateContext
		name, err := m.Decode(&p)
		if err != nil {
			return err
		}
		ret := s.g.contextCreate(p.CtxID, p.CapsetID, string(name))
		if ret != 0 {
			log.Printf("creating context %d of capability set %d: %d", p.CtxID, p.CapsetID, ret)
		}
		return s.reply(m, gfxproto.Reply{Errno: errno(ret)})

	case gfxproto.MsgDestroyContext:
		var p gfxproto.DestroyContext
		if _, err := m.Decode(&p); err != nil {
			return err
		}
		s.g.contextDestroy(p.CtxID)

	case gfxproto.MsgCreateResource:
		var p gfxproto.CreateResource
		if _, err := m.Decode(&p); err != nil {
			return err
		}
		s.createResource(&p)

	case gfxproto.MsgCreateBlob:
		var p gfxproto.CreateBlob
		if _, err := m.Decode(&p); err != nil {
			return err
		}
		if ret := s.g.createBlob(p.CtxID, p.ResID, p.Mem, p.Flags, p.BlobID, p.Size); ret != 0 {
			log.Printf("creating blob %d (memory %d, flags %#x, ID %d, %d bytes): %d", p.ResID, p.Mem, p.Flags, p.BlobID, p.Size, ret)
			return nil
		}
		s.resources[p.ResID] = &resource{fd: -1, blob: true}

	case gfxproto.MsgMapResource:
		var p gfxproto.MapResource
		if _, err := m.Decode(&p); err != nil {
			return err
		}
		fd, size, err := s.mapResource(p.ResID)
		if err != nil {
			log.Printf("mapping resource %d: %v", p.ResID, err)
			return s.reply(m, gfxproto.Reply{Errno: uint32(unix.EINVAL)})
		}
		return s.reply(m, gfxproto.Reply{Size: size, MapInfo: s.g.mapInfo(p.ResID)}, fd)

	case gfxproto.MsgAttachResource:
		var p gfxproto.AttachResource
		if _, err := m.Decode(&p); err != nil {
			return err
		}
		s.g.attachResource(p.CtxID, p.ResID)

	case gfxproto.MsgDetachResource:
		var p gfxproto.DetachResource
		if _, err := m.Decode(&p); err != nil {
			return err
		}
		s.g.detachResource(p.CtxID, p.ResID)

	case gfxproto.MsgDestroyResource:
		var p gfxproto.DestroyResource
		if _, err := m.Decode(&p); err != nil {
			return err
		}
		s.g.resourceUnref(p.ResID)
		if r, ok := s.resources[p.ResID]; ok {
			r.release()
			delete(s.resources, p.ResID)
		}

	case gfxproto.MsgSubmitCommand:
		var p gfxproto.SubmitCommand
		cmd, err := m.Decode(&p)
		if err != nil {
			return err
		}
		if ret := s.g.submitCommand(p.CtxID, cmd); ret != 0 {
			log.Printf("submitting %d bytes to context %d: %d", len(cmd), p.CtxID, ret)
		}

	case gfxproto.MsgTransfer:
		var p gfxproto.Transfer
		if _, err := m.Decode(&p); err != nil {
			return err
		}
		if ret := s.g.transfer(p.ResID, p.CtxID, p.ToHost != 0, p.Level, p.Stride, p.LayerStride, p.X, p.Y, p.Z, p.W, p.H, p.D, uint64(p.Offset)); ret != 0 {
			log.Printf("transferring resource %d: %d", p.ResID, ret)
		}

	case gfxproto.MsgCreateFence:
		var p gfxproto.CreateFence
		if _, err := m.Decode(&p); err != nil {
			return err
		}
		if ret := s.g.createFence(p.FenceID, p.CtxID, p.Ring, p.HasRing != 0); ret != 0 {
			// The fence will never be signaled.
			return fmt.Errorf("creating fence %d: %d", p.FenceID, ret)
		}

	case gfxproto.MsgSnapshot:
		fd, size, err := s.snapshot()
		if err != nil {
			log.Printf("taking a snapshot: %v", err)
			return s.reply(m, gfxproto.Reply{Errno: uint32(unix.EIO)})
		}
		err = s.reply(m, gfxproto.Reply{Size: size}, fd)
		unix.Close(fd)
		return err

	case gfxproto.MsgRestoreData:
		var p gfxproto.RestoreData
		data, err := m.Decode(&p)
		if err != nil {
			return err
		}
		if uint64(len(s.restoring))+uint64(len(data)) > maxSnapshotSize {
			return fmt.Errorf("a snapshot of more than %d bytes", maxSnapshotSize)
		}
		s.restoring = append(s.restoring, data...)

	case gfxproto.MsgRestore:
		var p gfxproto.Restore
		if _, err := m.Decode(&p); err != nil {
			return err
		}
		snapshot := s.restoring
		s.restoring = nil
		if uint64(len(snapshot)) != p.Size {
			log.Printf("restoring a snapshot of %d bytes, of which %d came", p.Size, len(snapshot))
			return s.reply(m, gfxproto.Reply{Errno: uint32(unix.EINVAL)})
		}
		if err := s.restore(snapshot); err != nil {
			log.Printf("restoring a snapshot: %v", err)
			return s.reply(m, gfxproto.Reply{Errno: uint32(unix.EIO)})
		}
		return s.reply(m, gfxproto.Reply{})

	default:
		return fmt.Errorf("unexpected %v", m.Type)
	}
	return nil
}

// createResource creates a classic resource, with a backing in shared memory
// that the Sentry maps as the guest's.
func (s *server) createResource(p *gfxproto.CreateResource) {
	fd, err := createSharedMemory(p.Size)
	if err != nil {
		log.Printf("allocating %d bytes for resource %d: %v", p.Size, p.ResID, err)
		return
	}
	backing, err := unix.Mmap(fd, 0, int(p.Size), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		log.Printf("mapping %d bytes for resource %d: %v", p.Size, p.ResID, err)
		unix.Close(fd)
		return
	}
	r := &resource{fd: fd, backing: backing}
	if ret := s.g.resourceCreate(p.ResID, p.Target, p.Format, p.Bind, p.Width, p.Height, p.Depth, p.ArraySize, p.LastLevel, p.NrSamples, p.Flags, unsafe.Pointer(&backing[0]), p.Size); ret != 0 {
		log.Printf("creating resource %d: %d", p.ResID, ret)
		r.release()
		return
	}
	s.resources[p.ResID] = r
}

// mapResource returns the descriptor and size of a resource's memory.
func (s *server) mapResource(resID uint32) (int, uint64, error) {
	r, ok := s.resources[resID]
	if !ok {
		return -1, 0, fmt.Errorf("no resource")
	}
	if r.fd < 0 {
		fd, typ, ret := s.g.exportBlob(resID)
		if ret != 0 {
			return -1, 0, fmt.Errorf("exporting: %d", ret)
		}
		if typ != handleTypeSHM && typ != handleTypeDMABuf {
			unix.Close(fd)
			return -1, 0, fmt.Errorf("the blob's memory is of type %#x, which the Sentry cannot map", typ)
		}
		r.fd = fd
	}
	var st unix.Stat_t
	if err := unix.Fstat(r.fd, &st); err != nil {
		return -1, 0, err
	}
	return r.fd, uint64(st.Size), nil
}

func (r *resource) release() {
	if r.backing != nil {
		unix.Munmap(r.backing)
		r.backing = nil
	}
	if r.fd >= 0 {
		unix.Close(r.fd)
		r.fd = -1
	}
}

func (s *server) releaseAll() {
	for id, r := range s.resources {
		r.release()
		delete(s.resources, id)
	}
}

// snapshot saves the renderer's state, and returns a pipe from which it
// reads, and its size. The Sentry has quiesced the renderer: no work is
// pending.
func (s *server) snapshot() (int, uint64, error) {
	dir := s.opts.ScratchDir
	if dir == "" {
		return -1, 0, errors.New("the renderer was not started for snapshots")
	}
	if ret := s.g.snapshot(dir); ret != 0 {
		removeFiles(dir)
		return -1, 0, fmt.Errorf("stream_renderer_snapshot: %d", ret)
	}
	entries, err := takeFiles(dir)
	if err != nil {
		return -1, 0, err
	}
	var resources []archivedResource
	for id, r := range s.resources {
		resources = append(resources, archivedResource{id: id, blob: r.blob})
		if !r.blob && r.backing != nil {
			entries = append(entries, archiveEntry{name: archiveBackingPrefix + strconv.FormatUint(uint64(id), 10), data: r.backing})
		}
	}
	entries = append(entries, archiveEntry{name: archiveResources, data: encodeResources(resources)})
	data := encodeArchive(entries)
	// The Sentry reads the snapshot from a pipe: it neither creates nor
	// maps memory of the renderer's for it.
	r, w, err := os.Pipe()
	if err != nil {
		return -1, 0, err
	}
	go func() {
		w.Write(data)
		w.Close()
	}()
	fd, err := unix.Dup(int(r.Fd()))
	r.Close()
	if err != nil {
		return -1, 0, err
	}
	return fd, uint64(len(data)), nil
}

// maxSnapshotSize is the size of the largest snapshot that a renderer
// restores.
const maxSnapshotSize = 16 << 30

// restore restores the renderer, which has no resources, from data.
func (s *server) restore(data []byte) error {
	dir := s.opts.ScratchDir
	if dir == "" {
		return errors.New("the renderer was not started for snapshots")
	}
	if len(s.resources) != 0 {
		return errors.New("the renderer has resources")
	}
	entries, err := decodeArchive(data)
	if err != nil {
		return err
	}
	var resources []archivedResource
	backings := make(map[uint32][]byte)
	defer removeFiles(dir)
	for _, e := range entries {
		switch {
		case e.name == archiveResources:
			if resources, err = decodeResources(e.data); err != nil {
				return err
			}
		case strings.HasPrefix(e.name, archiveBackingPrefix):
			id, err := strconv.ParseUint(strings.TrimPrefix(e.name, archiveBackingPrefix), 10, 32)
			if err != nil {
				return fmt.Errorf("snapshot entry %q", e.name)
			}
			backings[uint32(id)] = e.data
		default:
			if err := os.WriteFile(filepath.Join(dir, e.name), e.data, 0600); err != nil {
				return err
			}
		}
	}
	if ret := s.g.restore(dir); ret != 0 {
		return fmt.Errorf("stream_renderer_restore: %d", ret)
	}
	for _, ar := range resources {
		if ar.blob {
			s.resources[ar.id] = &resource{fd: -1, blob: true}
			continue
		}
		// gfxstream saves a classic resource, but not its backing.
		saved := backings[ar.id]
		if len(saved) == 0 {
			return fmt.Errorf("classic resource %d has no backing", ar.id)
		}
		fd, err := createSharedMemory(uint64(len(saved)))
		if err != nil {
			return err
		}
		backing, err := unix.Mmap(fd, 0, len(saved), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if err != nil {
			unix.Close(fd)
			return err
		}
		copy(backing, saved)
		r := &resource{fd: fd, backing: backing}
		if ret := s.g.attachBacking(ar.id, backing); ret != 0 {
			r.release()
			return fmt.Errorf("attaching the backing of resource %d: %d", ar.id, ret)
		}
		s.resources[ar.id] = r
	}
	return nil
}

// takeFiles returns the files of dir as archive entries, and removes them.
func takeFiles(dir string) ([]archiveEntry, error) {
	defer removeFiles(dir)
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var entries []archiveEntry
	for _, de := range des {
		if !de.Type().IsRegular() {
			return nil, fmt.Errorf("gfxstream's snapshot has %q, not a regular file", de.Name())
		}
		data, err := os.ReadFile(filepath.Join(dir, de.Name()))
		if err != nil {
			return nil, err
		}
		entries = append(entries, archiveEntry{name: de.Name(), data: data})
	}
	return entries, nil
}

// removeFiles removes the files of dir.
func removeFiles(dir string) {
	des, _ := os.ReadDir(dir)
	for _, de := range des {
		os.RemoveAll(filepath.Join(dir, de.Name()))
	}
}
