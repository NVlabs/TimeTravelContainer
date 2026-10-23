// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package gfxrenderer

import (
	"errors"
	"fmt"
	"log"
	"sync"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/gfxproto"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl"
)

// Probe returns the renderer's capability sets.
func Probe() ([]Capset, error) {
	data, err := venus.Probe()
	if err != nil {
		return nil, err
	}
	capsets := []Capset{
		{ID: venus.CapsetVenus, Data: data},
		// minigbm, Android's gralloc, allocates buffers with cross-domain
		// contexts.
		{ID: venus.CapsetCrossDomain, Data: venus.CrossDomainCapset()},
	}
	if virgl.Available() {
		// Mesa's virgl OpenGL driver's, which vkgl renders for.
		vcs, err := virgl.Capsets()
		if err != nil {
			log.Printf("rendering for no virgl guest: %v", err)
		}
		for _, c := range vcs {
			capsets = append(capsets, Capset{ID: c.ID, MaxVersion: c.MaxVersion, Data: c.Data})
		}
	}
	return capsets, nil
}

// ServeFamily serves the branches of a family, a connection to each, which
// its broker gives it on the control connection fd, which it takes. It
// returns once it serves no connection and keeps nothing for a fork's
// children, or once the broker disconnects and the connections close.
func ServeFamily(fd int) error {
	control, err := gfxproto.NewConn(fd)
	if err != nil {
		unix.Close(fd)
		return err
	}
	f := &familyServer{control: control}
	defer f.close()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		m, err := control.Recv()
		if errors.Is(err, gfxproto.ErrClosed) || f.closed() {
			return nil
		}
		if err != nil {
			return err
		}
		if m.Type != gfxproto.MsgServeConnection || len(m.FDs) != 1 {
			m.CloseFDs()
			return fmt.Errorf("unexpected %v with %d descriptors", m.Type, len(m.FDs))
		}
		conn := m.FDs[0]
		m.FDs = nil
		if !f.accept() {
			// The renderer exits: the broker starts another.
			unix.Close(conn)
			return nil
		}
		if err := control.Send(gfxproto.MsgReply, m.Seq, &gfxproto.Reply{}, nil); err != nil {
			unix.Close(conn)
			f.done()
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer f.done()
			if err := venus.Serve(conn); err != nil {
				log.Printf("serving a branch: %v", err)
			}
		}()
	}
}

// familyServer counts the connections that the renderer of a family serves,
// and closes its control connection once it serves none and keeps nothing
// for a fork's children.
type familyServer struct {
	control *gfxproto.Conn

	mu      sync.Mutex
	serving int
	closing bool
}

// accept counts a connection to serve, unless the renderer is exiting.
func (f *familyServer) accept() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closing {
		return false
	}
	f.serving++
	return true
}

// done uncounts a connection that the renderer served.
func (f *familyServer) done() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.serving--
	if f.serving == 0 && venus.PendingClones() == 0 && !f.closing {
		f.closing = true
		f.control.Close()
	}
}

func (f *familyServer) closed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closing
}

func (f *familyServer) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closing {
		f.closing = true
		f.control.Close()
	}
}
