// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package virtgpu implements a virtual GPU with the interface of Linux's
// virtio-gpu driver: a render node, /dev/dri/renderD128, whose contexts a
// Renderer outside the Sentry renders.
//
// Lock order:
//
//	renderFD.mu
//	  device.submitMu
//	    device.mu
//
// resource.mapsMu is a lock of a memmap.Mappable's methods other than
// Translate, and device.mu, which Translate takes, is below the memory
// manager's locks.
package virtgpu

import (
	goContext "context"
	"fmt"
	"sync/atomic"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	ktime "gvisor.dev/gvisor/pkg/sentry/ktime"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

const (
	// renderNodePath is the render node's path in /dev.
	renderNodePath = "dri/renderD128"

	// firstMapOffset is the first fake offset at which the render node maps a
	// resource. Like Linux's DRM_FILE_PAGE_OFFSET_START on 64-bit
	// architectures, it is the page after the first 4 GiB.
	firstMapOffset = 1<<32 + hostarch.PageSize

	// maxCapsetID and maxRings are the largest capability set ID and number
	// of rings that a context may have, as in Linux.
	maxCapsetID = 63
	maxRings    = 64

	// maxDebugNameLen is the longest debug name of a context, as in Linux.
	maxDebugNameLen = 63

	// maxCommandSize is the largest command buffer that EXECBUFFER and
	// RESOURCE_CREATE_BLOB accept.
	maxCommandSize = 16 << 20

	// defaultMaxContexts and defaultMaxResources are the default limits on
	// the device's contexts and resources.
	defaultMaxContexts  = 1024
	defaultMaxResources = 1 << 16

	// maxOpenFiles is the most files of the render node that may be open,
	// since each has a context ID.
	maxOpenFiles = 1 << 20
)

// ForkPolicy is what a fork does with a virtual GPU that is in use.
type ForkPolicy int

const (
	// ForkIneligible refuses the fork.
	ForkIneligible ForkPolicy = iota

	// ForkDeviceLost forks, and the child's GPU state is lost: its contexts
	// fail, its fences signal with an error, and its mappings of GPU memory
	// raise SIGBUS. Its new contexts get a renderer of its own.
	ForkDeviceLost

	// ForkSnapshot forks the GPU's state with a snapshot of the renderer.
	ForkSnapshot
)

// String returns the policy's name, as --gfx-fork spells it.
func (p ForkPolicy) String() string {
	switch p {
	case ForkIneligible:
		return "ineligible"
	case ForkDeviceLost:
		return "device-lost"
	case ForkSnapshot:
		return "snapshot"
	default:
		return fmt.Sprintf("ForkPolicy(%d)", int(p))
	}
}

// Options configures a virtual GPU.
type Options struct {
	// Capsets are the capability sets of the renderer that NewRenderer
	// starts.
	Capsets []Capset

	// NewRenderer starts a renderer. The device starts one when a context is
	// first created, and after it loses one.
	NewRenderer NewRendererFunc

	// ForkPolicy is what a fork does with the GPU while it is in use.
	ForkPolicy ForkPolicy

	// MaxContexts and MaxResources limit the device's contexts and resources.
	// Zero means a default.
	MaxContexts  int
	MaxResources int
}

// processNewRenderer is the NewRendererFunc of the last device registered in
// the process. Every branch in a Sentry process starts its renderers the same
// way, so a device that a load creates starts its renderers with it.
var processNewRenderer atomic.Pointer[NewRendererFunc]

// Register registers a virtual GPU in vfsObj as the render node
// /dev/dri/renderD128.
func Register(vfsObj *vfs.VirtualFilesystem, opts Options) error {
	d, err := newDevice(opts)
	if err != nil {
		return err
	}
	processNewRenderer.Store(&opts.NewRenderer)
	return vfsObj.RegisterDevice(vfs.CharDevice, linux.DRM_MAJOR, linux.DRM_RENDER_MINOR_BASE, d, &vfs.RegisterDeviceOptions{
		GroupName: "drm",
		Pathname:  renderNodePath,
		FilePerms: 0666,
	})
}

func newDevice(opts Options) (*device, error) {
	capsetMask := uint64(0)
	for _, c := range opts.Capsets {
		if c.ID == 0 || c.ID > maxCapsetID {
			return nil, fmt.Errorf("capability set ID %d is out of range", c.ID)
		}
		if capsetMask&(1<<c.ID) != 0 {
			return nil, fmt.Errorf("capability set %d is listed twice", c.ID)
		}
		if len(c.Data) == 0 {
			return nil, fmt.Errorf("capability set %d is empty", c.ID)
		}
		capsetMask |= 1 << c.ID
	}
	if opts.NewRenderer == nil {
		return nil, fmt.Errorf("no renderer")
	}
	d := &device{
		capsets:       opts.Capsets,
		capsetMask:    capsetMask,
		forkPolicy:    opts.ForkPolicy,
		newRenderer:   opts.NewRenderer,
		maxContexts:   opts.MaxContexts,
		maxResources:  opts.MaxResources,
		ctxIDs:        idAllocator{max: maxOpenFiles},
		nextResID:     1,
		nextFenceID:   1,
		nextMapOffset: firstMapOffset,
		resources:     make(map[uint32]*resource),
		mapOffsets:    make(map[uint64]*resource),
		timelines:     make(map[timelineKey]*timeline),
	}
	if d.maxContexts == 0 {
		d.maxContexts = defaultMaxContexts
	}
	if d.maxResources == 0 {
		d.maxResources = defaultMaxResources
	}
	return d, nil
}

// device is the virtual GPU.
//
// +stateify savable
type device struct {
	capsets      []Capset
	capsetMask   uint64
	forkPolicy   ForkPolicy
	maxContexts  int
	maxResources int

	// newRenderer starts a renderer. It is not saved: a load gets it from
	// the context that it loads with.
	newRenderer NewRendererFunc `state:"nosave"`

	// submitMu serializes the device's calls to its renderer, so that the
	// renderer sees them in the order that the device makes them.
	submitMu submitMutex `state:"nosave"`

	// renderer is the device's renderer, or nil if the device has not
	// started one. It is protected by submitMu. It is not saved: what it held
	// is lost to a load.
	renderer Renderer `state:"nosave"`

	// mu protects the fields below, and the state of the device's contexts,
	// resources, and fences that their comments say it protects.
	mu deviceMutex `state:"nosave"`

	// gen is the generation of the device's renderer, which grows with each
	// renderer that the device starts, and with each load. Contexts and
	// resources of an earlier generation are lost.
	gen uint64
	// lost is whether the renderer of generation gen was lost.
	lost bool
	// contexts is the number of contexts of generation gen.
	contexts int

	ctxIDs        idAllocator
	nextResID     uint32
	nextFenceID   uint64
	nextMapOffset uint64
	resources     map[uint32]*resource
	mapOffsets    map[uint64]*resource
	timelines     map[timelineKey]*timeline

	// clock timestamps fences. It is the kernel's monotonic clock, once a
	// file of the device is opened.
	clock ktime.Clock `state:"nosave"`
}

// CtxNewRenderer is a Context.Value key for the NewRendererFunc of a loaded
// virtual GPU.
const CtxNewRenderer contextID = iota

type contextID int

// afterLoad is invoked by stateify. The renderer is not saved, so whatever it
// held is lost: contexts and resources, which the new generation makes lost,
// and fences, which are signaled with an error.
func (d *device) afterLoad(ctx goContext.Context) {
	if f, ok := ctx.Value(CtxNewRenderer).(NewRendererFunc); ok {
		d.newRenderer = f
	} else if f := processNewRenderer.Load(); f != nil {
		d.newRenderer = *f
	}
	d.gen++
	d.lost = false
	d.contexts = 0
	for key, tl := range d.timelines {
		for _, f := range tl.pending {
			// Blocked waiters are not saved, and epoll rechecks the readiness
			// of every file after a load, so there is no one to wake.
			f.signalLocked(-int32(linuxerr.ENODEV.Errno()), 0, false /* notify */)
		}
		delete(d.timelines, key)
	}
}

// Open implements vfs.Device.Open.
func (d *device) Open(ctx context.Context, mnt *vfs.Mount, vfsd *vfs.Dentry, opts vfs.OpenOptions) (*vfs.FileDescription, error) {
	d.mu.Lock()
	ctxID, ok := d.ctxIDs.alloc()
	if d.clock == nil {
		if k := kernel.KernelFromContext(ctx); k != nil {
			d.clock = k.MonotonicClock()
		}
	}
	d.mu.Unlock()
	if !ok {
		return nil, linuxerr.ENOSPC
	}
	fd := &renderFD{
		dev:          d,
		ctxID:        ctxID,
		handles:      idAllocator{max: 1<<31 - 1},
		objects:      make(map[uint32]*resource),
		handleCounts: make(map[*resource]int),
		prime:        make(map[*resource]uint32),
	}
	if err := fd.vfsfd.Init(fd, opts.Flags, auth.CredentialsFromContext(ctx), mnt, vfsd, &vfs.FileDescriptionOptions{
		UseDentryMetadata: true,
	}); err != nil {
		d.mu.Lock()
		d.ctxIDs.release(ctxID)
		d.mu.Unlock()
		return nil, err
	}
	return &fd.vfsfd, nil
}

// nowLocked returns the time for a fence's timestamp.
//
// Preconditions: d.mu is locked.
func (d *device) nowLocked() int64 {
	if d.clock == nil {
		return 0
	}
	return d.clock.Now().Nanoseconds()
}

// capset returns the capability set with the given ID.
func (d *device) capset(id uint32) (*Capset, bool) {
	for i := range d.capsets {
		if d.capsets[i].ID == id {
			return &d.capsets[i], true
		}
	}
	return nil, false
}

// rendererLocked returns the device's renderer of generation gen. It returns
// ENODEV if gen is not the current generation, or the renderer was lost,
// since whatever wants the renderer was lost with it.
//
// Preconditions: d.submitMu is locked.
func (d *device) rendererLocked(gen uint64) (Renderer, error) {
	d.mu.Lock()
	cur, lost := d.gen, d.lost
	d.mu.Unlock()
	if gen != cur || lost {
		return nil, linuxerr.ENODEV
	}
	if d.renderer == nil {
		return nil, linuxerr.ENODEV
	}
	return d.renderer, nil
}

// startRendererLocked returns the generation of the device's renderer,
// starting a renderer if the device has none, or lost the last one.
//
// Preconditions: d.submitMu is locked.
func (d *device) startRendererLocked() (uint64, error) {
	d.mu.Lock()
	gen, lost := d.gen, d.lost
	d.mu.Unlock()
	if d.renderer != nil && !lost {
		return gen, nil
	}
	if d.newRenderer == nil {
		return 0, linuxerr.ENODEV
	}
	if d.renderer != nil {
		d.renderer.Close()
		d.renderer = nil
		d.mu.Lock()
		d.gen++
		d.lost = false
		d.contexts = 0
		gen = d.gen
		d.mu.Unlock()
	}
	r, err := d.newRenderer(&rendererEvents{dev: d, gen: gen})
	if err != nil {
		log.Warningf("virtgpu: starting a renderer: %v", err)
		return 0, linuxerr.ENODEV
	}
	d.renderer = r
	return gen, nil
}

// rendererEvents receives the events of the renderer of one generation.
type rendererEvents struct {
	dev *device
	gen uint64
}

// FenceSignaled implements RendererEvents.FenceSignaled.
func (e *rendererEvents) FenceSignaled(f Fence) {
	d := e.dev
	d.mu.Lock()
	defer d.mu.Unlock()
	if e.gen != d.gen {
		return
	}
	key := timelineOf(f)
	tl, ok := d.timelines[key]
	if !ok {
		return
	}
	now := d.nowLocked()
	n := 0
	for n < len(tl.pending) && tl.pending[n].id <= f.ID {
		n++
	}
	signaled := tl.pending[:n]
	tl.pending = tl.pending[n:]
	if len(tl.pending) == 0 {
		delete(d.timelines, key)
	}
	for _, pf := range signaled {
		pf.signalLocked(0, now, true /* notify */)
	}
}

// RendererLost implements RendererEvents.RendererLost.
func (e *rendererEvents) RendererLost(err error) {
	d := e.dev
	d.mu.Lock()
	defer d.mu.Unlock()
	if e.gen != d.gen || d.lost {
		return
	}
	log.Warningf("virtgpu: lost the renderer: %v", err)
	d.lost = true
	d.failPendingFencesLocked()
}

// failPendingFencesLocked signals every pending fence with ENODEV.
//
// Preconditions: d.mu is locked.
func (d *device) failPendingFencesLocked() {
	now := d.nowLocked()
	for key, tl := range d.timelines {
		for _, f := range tl.pending {
			f.signalLocked(-int32(linuxerr.ENODEV.Errno()), now, true /* notify */)
		}
		delete(d.timelines, key)
	}
}

// newFenceLocked creates a fence that follows everything submitted to ctxID's
// ring, or to the global timeline if !hasRing, until now.
//
// Preconditions: d.submitMu and d.mu are locked.
func (d *device) newFenceLocked(ctxID uint32, hasRing bool, ring uint32) *fence {
	f := &fence{
		id: d.nextFenceID,
	}
	d.nextFenceID++
	if hasRing {
		f.timeline = timelineKey{ctxID: ctxID, ring: ring}
	} else {
		f.timeline = timelineKey{global: true}
	}
	tl, ok := d.timelines[f.timeline]
	if !ok {
		tl = &timeline{}
		d.timelines[f.timeline] = tl
	}
	tl.pending = append(tl.pending, f)
	return f
}

// createFenceLocked creates a fence as newFenceLocked does, and asks r to
// signal it. If r cannot, the fence is signaled with an error.
//
// Preconditions: d.submitMu is locked.
func (d *device) createFenceLocked(r Renderer, ctxID uint32, hasRing bool, ring uint32) *fence {
	d.mu.Lock()
	f := d.newFenceLocked(ctxID, hasRing, ring)
	d.mu.Unlock()
	if err := r.CreateFence(Fence{ID: f.id, CtxID: ctxID, HasRing: hasRing, Ring: ring}); err != nil {
		log.Warningf("virtgpu: creating fence %d: %v", f.id, err)
		d.mu.Lock()
		d.cancelFenceLocked(f, -int32(linuxerr.EIO.Errno()))
		d.mu.Unlock()
	}
	return f
}

// cancelFenceLocked signals a pending fence with an error status.
//
// Preconditions: d.mu is locked.
func (d *device) cancelFenceLocked(f *fence, status int32) {
	if f.signaled {
		return
	}
	if tl, ok := d.timelines[f.timeline]; ok {
		for i, pf := range tl.pending {
			if pf == f {
				tl.pending = append(tl.pending[:i], tl.pending[i+1:]...)
				break
			}
		}
		if len(tl.pending) == 0 {
			delete(d.timelines, f.timeline)
		}
	}
	f.signalLocked(status, d.nowLocked(), true /* notify */)
}

// allocResourceIDLocked returns an unused resource ID. Like Linux's driver,
// by default, it does not reuse IDs until they wrap around.
//
// Preconditions: d.mu is locked.
func (d *device) allocResourceIDLocked() (uint32, error) {
	if len(d.resources) >= d.maxResources {
		return 0, linuxerr.ENOSPC
	}
	for {
		id := d.nextResID
		d.nextResID++
		if d.nextResID == 0 {
			d.nextResID = 1
		}
		if _, ok := d.resources[id]; !ok {
			return id, nil
		}
	}
}

// validateLiveFork returns an error if the device cannot follow a fork while
// it is in use.
func (d *device) validateLiveFork() error {
	switch d.forkPolicy {
	case ForkDeviceLost:
		return nil
	case ForkIneligible:
		return fmt.Errorf("the virtual GPU is in use, and its fork semantics are %s (see --gfx-fork)", d.forkPolicy)
	default:
		return fmt.Errorf("the virtual GPU's fork semantics %s are not implemented", d.forkPolicy)
	}
}
