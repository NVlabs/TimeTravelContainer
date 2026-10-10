// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin && cgo

package virgl

/*
#include <stdlib.h>
#include "virgl.h"
#include "virgl_hw.h"
#include "virgl_protocol.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"unsafe"
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

	// xfbEmulated is whether the renderer emulates transform feedback,
	// which Zink has none of here, and xfbSSBO the vertex shader's first
	// shader buffer slot that it keeps for its targets.
	xfbEmulated bool
	xfbSSBO     uint32
)

// Init creates the process's Zink screen, on the Vulkan that the renderer
// links, if it has not.
func Init() error {
	initOnce.Do(func() {
		if C.ttc_virgl_init() != 0 {
			initErr = errors.New("Zink found no usable Vulkan device")
			return
		}
		var ssbo C.uint32_t
		xfbEmulated = bool(C.ttc_virgl_xfb(&ssbo))
		xfbSSBO = uint32(ssbo)
	})
	return initErr
}

// Available reports whether the renderer renders for virgl's guests here.
func Available() bool {
	return true
}

// Capsets returns virgl's capability sets, of what Zink offers.
func Capsets() ([]Capset, error) {
	if err := Init(); err != nil {
		return nil, err
	}
	v1 := make([]byte, C.sizeof_struct_virgl_caps_v1)
	C.ttc_virgl_fill_caps(CapsetVirgl, unsafe.Pointer(&v1[0]), C.uint32_t(len(v1)))
	v2 := make([]byte, C.sizeof_struct_virgl_caps_v2)
	C.ttc_virgl_fill_caps(CapsetVirgl2, unsafe.Pointer(&v2[0]), C.uint32_t(len(v2)))
	return []Capset{
		{ID: CapsetVirgl, MaxVersion: 1, Data: v1},
		{ID: CapsetVirgl2, MaxVersion: 2, Data: v2},
	}, nil
}

// Resource is a guest's classic resource: a Gallium resource of Zink's, and
// the guest's memory of it, which transfers copy to and from.
type Resource struct {
	id      uint32
	pipe    *C.struct_pipe_resource
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
	p := C.ttc_virgl_resource_create(C.uint32_t(a.Target), C.uint32_t(a.Format), C.uint32_t(a.Bind),
		C.uint32_t(a.Width), C.uint32_t(a.Height), C.uint32_t(a.Depth), C.uint32_t(a.ArraySize),
		C.uint32_t(a.LastLevel), C.uint32_t(a.NrSamples), C.uint32_t(a.Flags))
	if p == nil {
		return nil, fmt.Errorf("Zink refused resource %d %+v", id, a)
	}
	r := &Resource{id: id, pipe: p, backing: backing, release: release}
	r.refs.Store(1)
	return r, nil
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
	C.ttc_virgl_resource_unref(r.pipe)
	r.pipe = nil
	if r.release != nil {
		r.release()
	}
}

func transfer(pipe *C.struct_pipe_context, r *Resource, a TransferArgs) error {
	if len(r.backing) == 0 {
		return errors.New("no memory")
	}
	errno := C.ttc_virgl_transfer(pipe, r.pipe, C.bool(a.ToHost), C.uint32_t(a.Level),
		C.int32_t(a.X), C.int32_t(a.Y), C.int32_t(a.Z), C.int32_t(a.W), C.int32_t(a.H), C.int32_t(a.D),
		(*C.uint8_t)(unsafe.Pointer(&r.backing[0])), C.uint64_t(len(r.backing)), C.uint64_t(a.Offset),
		C.uint32_t(a.Stride), C.uint32_t(a.LayerStride))
	if errno != 0 {
		return fmt.Errorf("transferring resource %d %+v: errno %d", r.id, a, int(errno))
	}
	return nil
}

// transferContext is the renderer's own Gallium context, for the transfers
// of no virgl context's.
var transferContext struct {
	mu   sync.Mutex
	pipe *C.struct_pipe_context
}

// Transfer executes a transfer between r and the guest's memory of it, for
// no context.
func Transfer(r *Resource, a TransferArgs) error {
	t := &transferContext
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pipe == nil {
		if t.pipe = C.ttc_virgl_context_create(); t.pipe == nil {
			return errNoContext
		}
	}
	err := transfer(t.pipe, r, a)
	// Contexts' later work follows the transfer on the GPU's queue.
	C.ttc_virgl_flush_async(t.pipe)
	return err
}
