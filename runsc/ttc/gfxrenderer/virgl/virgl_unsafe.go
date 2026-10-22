// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build (darwin || linux) && cgo

package virgl

/*
#cgo linux LDFLAGS: -ldl
#include <stdlib.h>
#include "virgl.h"
#include "virgl_hw.h"
#include "virgl_protocol.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
)

// The protocol's numbers in protocol.go are virgl_protocol.h's: an index
// other than 0 fails to compile.
var (
	_ = [1]struct{}{}[cmdSetTessState-C.VIRGL_CCMD_SET_TESS_STATE]
	_ = [1]struct{}{}[cmdTransfer3D-C.VIRGL_CCMD_TRANSFER3D]
	_ = [1]struct{}{}[cmdLinkShader-C.VIRGL_CCMD_LINK_SHADER]
	_ = [1]struct{}{}[cmdClearSurface-C.VIRGL_CCMD_CLEAR_SURFACE]
	_ = [1]struct{}{}[objectMSAASurface-C.VIRGL_OBJECT_MSAA_SURFACE]
	_ = [1]struct{}{}[transferFromHost-C.VIRGL_TRANSFER_FROM_HOST]
	_ = [1]struct{}{}[queryStateSize-C.sizeof_struct_virgl_host_query_state]
)

var (
	initOnce sync.Once
	initErr  error
	// be is the backend that the renderer renders with.
	be backend

	// xfbEmulated is whether the renderer emulates transform feedback,
	// which Zink has none of here, and xfbSSBO the vertex shader's first
	// shader buffer slot that it keeps for its targets.
	xfbEmulated bool
	xfbSSBO     uint32
)

// Init creates the process's backend, if it has not: Zink's screen, on the
// Vulkan that the renderer links, or, where TTC_VIRGL_BACKEND is "vkgl",
// the Go backend's device (vkgl_unsafe.go).
func Init() error {
	initOnce.Do(func() {
		if os.Getenv("TTC_VIRGL_BACKEND") == "vkgl" {
			be, initErr = initVkgl()
		} else {
			be, initErr = initZink()
		}
		if initErr != nil {
			return
		}
		xfbEmulated, xfbSSBO = be.xfb()
	})
	return initErr
}

// Available reports whether the renderer renders for virgl's guests here.
func Available() bool {
	return true
}

// Capsets returns virgl's capability sets, of what the backend offers.
func Capsets() ([]Capset, error) {
	if err := Init(); err != nil {
		return nil, err
	}
	v1 := make([]byte, C.sizeof_struct_virgl_caps_v1)
	be.fillCaps(CapsetVirgl, v1)
	v2 := make([]byte, C.sizeof_struct_virgl_caps_v2)
	be.fillCaps(CapsetVirgl2, v2)
	return []Capset{
		{ID: CapsetVirgl, MaxVersion: 1, Data: v1},
		{ID: CapsetVirgl2, MaxVersion: 2, Data: v2},
	}, nil
}

// Resource is a guest's classic resource: the backend's resource, and the
// guest's memory of it, which transfers copy to and from.
type Resource struct {
	id      uint32
	args    ResourceArgs
	res     backendResource
	backing []byte
	release func()
	refs    atomic.Int32
}

// NewResource returns a resource of a, whose guest's memory is backing.
// release is called once the resource needs backing no more.
func NewResource(id uint32, a ResourceArgs, backing []byte, release func()) (*Resource, error) {
	if err := Init(); err != nil {
		return nil, err
	}
	res, err := be.newResource(a)
	if err != nil {
		return nil, fmt.Errorf("resource %d %+v: %w", id, a, err)
	}
	r := &Resource{id: id, args: a, res: res, backing: backing, release: release}
	r.refs.Store(1)
	return r, nil
}

// Clone returns a copy of r for a fork's child: a resource of r's
// parameters and contents, whose guest's memory is backing, and which calls
// release as NewResource's does. What wrote r must be done; Sync waits for
// the copy.
func (r *Resource) Clone(backing []byte, release func()) (*Resource, error) {
	c, err := NewResource(r.id, r.args, backing, release)
	if err != nil {
		return nil, err
	}
	t := &transferContext
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.createLocked(); err != nil {
		// The caller releases backing, as if NewResource had failed.
		c.release = nil
		c.DecRef()
		return nil, err
	}
	t.pipe.copyResource(c.res, r.res)
	return c, nil
}

// Backing returns the guest's memory of r.
func (r *Resource) Backing() []byte {
	return r.backing
}

// IncRef takes a reference to r, which DecRef releases.
func (r *Resource) IncRef() {
	r.incRef()
}

// ID returns the resource's ID.
func (r *Resource) ID() uint32 {
	return r.id
}

func (r *Resource) incRef() {
	r.refs.Add(1)
}

// DecRef releases a reference to r, which its creator holds, as do the
// contexts that use it, and their objects and state.
func (r *Resource) DecRef() {
	if r.refs.Add(-1) != 0 {
		return
	}
	r.res.unref()
	r.res = nil
	if r.release != nil {
		r.release()
	}
}

func transfer(pipe backendContext, r *Resource, a TransferArgs) error {
	if len(r.backing) == 0 {
		return errors.New("no memory")
	}
	if err := pipe.transfer(r.res, a, r.backing); err != nil {
		return fmt.Errorf("transferring resource %d %+v: %w", r.id, a, err)
	}
	return nil
}

// transferContext is the renderer's own backend context, for the transfers
// of no virgl context's, and the copies of clones.
var transferContext ownContext

// ownContext is a backend context of the renderer's own.
type ownContext struct {
	mu   sync.Mutex
	pipe backendContext
}

// createLocked creates the context if it has none.
//
// Preconditions: t.mu is locked.
func (t *ownContext) createLocked() error {
	if t.pipe == nil {
		pipe, err := be.newContext()
		if err != nil {
			return err
		}
		t.pipe = pipe
	}
	return nil
}

// Transfer executes a transfer between r and the guest's memory of it, for
// no context.
func Transfer(r *Resource, a TransferArgs) error {
	t := &transferContext
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.createLocked(); err != nil {
		return err
	}
	err := transfer(t.pipe, r, a)
	// Contexts' later work follows the transfer on the GPU's queue.
	t.pipe.flushAsync()
	return err
}

// Sync waits until the copies of the resources that Clone returned are
// done.
func Sync() {
	t := &transferContext
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pipe == nil {
		return
	}
	if f := t.pipe.flush(); f != nil {
		f.wait()
	}
}
