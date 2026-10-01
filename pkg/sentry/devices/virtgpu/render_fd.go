// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package virtgpu

import (
	"time"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/marshal"
	"gvisor.dev/gvisor/pkg/marshal/primitive"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/usermem"
	"gvisor.dev/gvisor/pkg/waiter"
)

const (
	// maxBOHandles is the most GEM handles that one EXECBUFFER may use.
	maxBOHandles = 1 << 16

	// maxResourceSize is the largest resource that the device creates.
	maxResourceSize = 1 << 36
)

// renderFD is an open file of the render node: a DRM file, with at most one
// context of the virtual GPU, and GEM handles for its resources.
//
// +stateify savable
type renderFD struct {
	vfsfd vfs.FileDescription
	vfs.FileDescriptionDefaultImpl
	vfs.DentryMetadataFileDescriptionImpl
	vfs.NoLockFD

	dev *device
	// ctxID is the ID of the file's context. Like Linux's driver, the device
	// assigns it when the file is opened, and creates the context later.
	ctxID uint32

	// mu protects the fields below.
	mu renderFDMutex `state:"nosave"`

	// ctxCreated is whether the file's context was created, and ctxGen is
	// the generation of the renderer that holds it.
	ctxCreated bool
	ctxGen     uint64
	// numRings is the number of the context's rings, which have timelines
	// of their own.
	numRings uint32

	// handles allocates GEM handles, and objects maps them to resources.
	handles idAllocator
	objects map[uint32]*resource
	// handleCounts is the number of the file's handles for each resource.
	// The file may map only resources that it has handles for.
	handleCounts map[*resource]int
	// prime maps the resources that the file exported or imported to their
	// handles, as Linux's struct drm_prime_file_private does.
	prime map[*resource]uint32
}

var _ vfs.LiveForkFileDescription = (*renderFD)(nil)

// Release implements vfs.FileDescriptionImpl.Release.
func (fd *renderFD) Release(ctx context.Context) {
	d := fd.dev
	fd.mu.Lock()
	objects := fd.objects
	fd.objects = nil
	fd.handleCounts = nil
	fd.prime = nil
	created, gen := fd.ctxCreated, fd.ctxGen
	fd.ctxCreated = false
	fd.mu.Unlock()

	// Like Linux, close every handle, then destroy the context.
	for _, r := range objects {
		if created {
			fd.detach(gen, r)
		}
		r.DecRef(ctx)
	}
	if created {
		d.submitMu.Lock()
		if renderer, err := d.rendererLocked(gen); err == nil {
			renderer.DestroyContext(fd.ctxID)
		}
		d.submitMu.Unlock()
	}
	d.mu.Lock()
	if created {
		d.contextDestroyedLocked(fd.ctxID, gen)
	}
	d.ctxIDs.release(fd.ctxID)
	d.mu.Unlock()
	d.closeIdleRenderer()
}

// contextDestroyedLocked cleans up after a context of generation gen: the
// renderer will signal no more fences on its rings.
//
// Preconditions: d.mu is locked.
func (d *device) contextDestroyedLocked(ctxID uint32, gen uint64) {
	if gen != d.gen {
		return
	}
	d.contexts--
	now := d.nowLocked()
	for key, tl := range d.timelines {
		if key.global || key.ctxID != ctxID {
			continue
		}
		for _, f := range tl.pending {
			f.signalLocked(-int32(linuxerr.ECANCELED.Errno()), now, true /* notify */)
		}
		delete(d.timelines, key)
	}
}

// ValidateLiveFork implements vfs.LiveForkFileDescription.ValidateLiveFork.
func (fd *renderFD) ValidateLiveFork() ([]*vfs.FileDescription, error) {
	fd.mu.Lock()
	inUse := fd.ctxCreated || len(fd.objects) != 0
	fd.mu.Unlock()
	if !inUse {
		return nil, nil
	}
	return nil, fd.dev.validateLiveFork()
}

// Readiness implements waiter.Waitable.Readiness. Linux's DRM file is
// readable when it has events, which the device never sends.
func (fd *renderFD) Readiness(mask waiter.EventMask) waiter.EventMask {
	return 0
}

// Epollable implements vfs.FileDescriptionImpl.Epollable.
func (fd *renderFD) Epollable() bool {
	return true
}

// ConfigureMMap implements vfs.FileDescriptionImpl.ConfigureMMap.
func (fd *renderFD) ConfigureMMap(ctx context.Context, opts *memmap.MMapOpts) error {
	d := fd.dev
	// Like Linux, map only from the start of a resource's range of offsets:
	// see drm_gem_mmap().
	d.mu.Lock()
	r, ok := d.mapOffsets[opts.Offset]
	d.mu.Unlock()
	if !ok {
		return linuxerr.EINVAL
	}
	fd.mu.Lock()
	if fd.handleCounts[r] == 0 {
		fd.mu.Unlock()
		return linuxerr.EACCES
	}
	// The file's handle holds a reference, so r is alive.
	r.IncRef()
	fd.mu.Unlock()
	defer r.DecRef(ctx)
	return r.configureMMap(opts, 0)
}

// Ioctl implements vfs.FileDescriptionImpl.Ioctl.
func (fd *renderFD) Ioctl(ctx context.Context, uio usermem.IO, sysno uintptr, args arch.SyscallArguments) (uintptr, error) {
	t := kernel.TaskFromContext(ctx)
	if t == nil {
		panic("Ioctl should be called from a task context")
	}
	cmd := args[1].Uint()
	addr := args[2].Pointer()
	if ioctlType(cmd) != linux.DRM_IOCTL_BASE {
		return 0, linuxerr.ENOTTY
	}
	nr := linux.IOC_NR(cmd)
	if nr >= linux.DRM_COMMAND_BASE && nr < drmCommandEnd {
		switch nr - linux.DRM_COMMAND_BASE {
		case linux.DRM_VIRTGPU_MAP:
			return 0, fd.mapIoctl(t, cmd, addr)
		case linux.DRM_VIRTGPU_EXECBUFFER:
			return 0, fd.execbuffer(ctx, t, cmd, addr)
		case linux.DRM_VIRTGPU_GETPARAM:
			return 0, fd.getparam(t, cmd, addr)
		case linux.DRM_VIRTGPU_RESOURCE_CREATE:
			return 0, fd.resourceCreate(t, cmd, addr)
		case linux.DRM_VIRTGPU_RESOURCE_INFO:
			return 0, fd.resourceInfo(t, cmd, addr)
		case linux.DRM_VIRTGPU_TRANSFER_FROM_HOST:
			return 0, fd.transfer(t, cmd, addr, false /* toHost */)
		case linux.DRM_VIRTGPU_TRANSFER_TO_HOST:
			return 0, fd.transfer(t, cmd, addr, true /* toHost */)
		case linux.DRM_VIRTGPU_WAIT:
			return 0, fd.wait(ctx, t, cmd, addr)
		case linux.DRM_VIRTGPU_GET_CAPS:
			return 0, fd.getCaps(t, cmd, addr)
		case linux.DRM_VIRTGPU_RESOURCE_CREATE_BLOB:
			return 0, fd.resourceCreateBlob(t, cmd, addr)
		case linux.DRM_VIRTGPU_CONTEXT_INIT:
			return 0, fd.contextInit(t, cmd, addr)
		default:
			return 0, linuxerr.EINVAL
		}
	}
	switch nr {
	case linux.DRM_NR_VERSION:
		return 0, fd.version(t, cmd, addr)
	case linux.DRM_NR_GET_CAP:
		return 0, fd.getCap(t, cmd, addr)
	case linux.DRM_NR_GEM_CLOSE:
		return 0, fd.gemClose(t, cmd, addr)
	case linux.DRM_NR_PRIME_HANDLE_TO_FD:
		return 0, fd.primeHandleToFD(t, cmd, addr)
	case linux.DRM_NR_PRIME_FD_TO_HANDLE:
		return 0, fd.primeFDToHandle(t, cmd, addr)
	}
	if isSyncobjIoctl(nr) {
		// The driver does not support DRM sync objects, and says so for
		// DRM_CAP_SYNCOBJ.
		return 0, linuxerr.EOPNOTSUPP
	}
	// Linux refuses the other core ioctls to render nodes.
	if log.IsLogging(log.Debug) {
		t.Debugf("virtgpu: refusing DRM ioctl %#x", cmd)
	}
	return 0, linuxerr.EACCES
}

// drmCommandEnd is the end of the range of drivers' ioctl numbers.
const drmCommandEnd = 0xa0

// isSyncobjIoctl returns whether nr is one of the DRM core's ioctls for sync
// objects.
func isSyncobjIoctl(nr uint32) bool {
	return (nr >= 0xbf && nr <= 0xc5) || (nr >= 0xca && nr <= 0xcd) || nr == 0xcf
}

// ioctlType returns the type of an ioctl command.
func ioctlType(cmd uint32) uint32 {
	return (cmd >> linux.IOC_TYPESHIFT) & (1<<linux.IOC_TYPEBITS - 1)
}

// ioctlDir returns the direction of an ioctl command.
func ioctlDir(cmd uint32) uint32 {
	return cmd >> linux.IOC_DIRSHIFT
}

// The directions of the kernel's DRM ioctls.
const (
	kernelRW = linux.IOC_READ | linux.IOC_WRITE
	kernelW  = linux.IOC_WRITE
)

// copyInArg copies in the argument of the DRM ioctl cmd, whose kernel struct
// is arg and direction kdir, as Linux's drm_ioctl() does: as much of the
// argument as both cmd and the kernel's command define, zero-extended. So
// older and newer callers work, whose structs are shorter or longer.
func copyInArg(t *kernel.Task, cmd, kdir uint32, addr hostarch.Addr, arg marshal.Marshallable) error {
	buf := make([]byte, arg.SizeBytes())
	if ioctlDir(cmd)&kdir&linux.IOC_WRITE != 0 {
		n := min(int(linux.IOC_SIZE(cmd)), len(buf))
		if _, err := t.CopyInBytes(addr, buf[:n]); err != nil {
			return err
		}
	}
	arg.UnmarshalBytes(buf)
	return nil
}

// copyOutArg copies out the argument of the DRM ioctl cmd, as copyInArg
// copies it in.
func copyOutArg(t *kernel.Task, cmd, kdir uint32, addr hostarch.Addr, arg marshal.Marshallable) error {
	if ioctlDir(cmd)&kdir&linux.IOC_READ == 0 {
		return nil
	}
	buf := make([]byte, arg.SizeBytes())
	arg.MarshalBytes(buf)
	n := min(int(linux.IOC_SIZE(cmd)), len(buf))
	_, err := t.CopyOutBytes(addr, buf[:n])
	return err
}

// version implements DRM_IOCTL_VERSION.
func (fd *renderFD) version(t *kernel.Task, cmd uint32, addr hostarch.Addr) error {
	var v linux.DRMVersion
	if err := copyInArg(t, cmd, kernelRW, addr, &v); err != nil {
		return err
	}
	// Those of Linux's virtio-gpu driver, which Mesa's drivers look for.
	v.VersionMajor = 0
	v.VersionMinor = 1
	v.VersionPatchlevel = 0
	for _, field := range []struct {
		len   *uint64
		buf   uint64
		value string
	}{
		{&v.NameLen, v.Name, "virtio_gpu"},
		{&v.DateLen, v.Date, "0"},
		{&v.DescLen, v.Desc, "virtio GPU"},
	} {
		n := min(uint64(len(field.value)), *field.len)
		*field.len = uint64(len(field.value))
		if n != 0 && field.buf != 0 {
			if _, err := t.CopyOutBytes(hostarch.Addr(field.buf), []byte(field.value[:n])); err != nil {
				return err
			}
		}
	}
	return copyOutArg(t, cmd, kernelRW, addr, &v)
}

// getCap implements DRM_IOCTL_GET_CAP.
func (fd *renderFD) getCap(t *kernel.Task, cmd uint32, addr hostarch.Addr) error {
	var c linux.DRMGetCap
	if err := copyInArg(t, cmd, kernelRW, addr, &c); err != nil {
		return err
	}
	switch c.Capability {
	case linux.DRM_CAP_TIMESTAMP_MONOTONIC:
		c.Value = 1
	case linux.DRM_CAP_PRIME:
		c.Value = linux.DRM_PRIME_CAP_IMPORT | linux.DRM_PRIME_CAP_EXPORT
	case linux.DRM_CAP_SYNCOBJ, linux.DRM_CAP_SYNCOBJ_TIMELINE:
		c.Value = 0
	default:
		// The render node has no modesetting, whose capabilities these are.
		return linuxerr.EOPNOTSUPP
	}
	return copyOutArg(t, cmd, kernelRW, addr, &c)
}

// getparam implements DRM_IOCTL_VIRTGPU_GETPARAM.
func (fd *renderFD) getparam(t *kernel.Task, cmd uint32, addr hostarch.Addr) error {
	var p linux.DRMVirtgpuGetparam
	if err := copyInArg(t, cmd, kernelRW, addr, &p); err != nil {
		return err
	}
	var value int32
	switch p.Param {
	case linux.VIRTGPU_PARAM_3D_FEATURES,
		linux.VIRTGPU_PARAM_CAPSET_QUERY_FIX,
		linux.VIRTGPU_PARAM_RESOURCE_BLOB,
		linux.VIRTGPU_PARAM_HOST_VISIBLE,
		linux.VIRTGPU_PARAM_CONTEXT_INIT,
		linux.VIRTGPU_PARAM_EXPLICIT_DEBUG_NAME:
		value = 1
	case linux.VIRTGPU_PARAM_CROSS_DEVICE:
		// The device cannot share resources with other virtio devices.
		value = 0
	case linux.VIRTGPU_PARAM_SUPPORTED_CAPSET_IDS:
		value = int32(fd.dev.capsetMask)
	default:
		// Including ChromeOS's parameters 9 and 10, since upstream Linux
		// defines 9 differently.
		return linuxerr.EINVAL
	}
	// Linux writes an int.
	_, err := primitive.CopyInt32Out(t, hostarch.Addr(p.Value), value)
	return err
}

// getCaps implements DRM_IOCTL_VIRTGPU_GET_CAPS.
func (fd *renderFD) getCaps(t *kernel.Task, cmd uint32, addr hostarch.Addr) error {
	var c linux.DRMVirtgpuGetCaps
	if err := copyInArg(t, cmd, kernelRW, addr, &c); err != nil {
		return err
	}
	d := fd.dev
	if len(d.capsets) == 0 {
		return linuxerr.ENOSYS
	}
	if c.Size == 0 {
		return linuxerr.EINVAL
	}
	capset, ok := d.capset(c.CapSetID)
	if !ok || capset.MaxVersion < c.CapSetVer {
		return linuxerr.EINVAL
	}
	n := min(int(c.Size), len(capset.Data))
	_, err := t.CopyOutBytes(hostarch.Addr(c.Addr), capset.Data[:n])
	return err
}

// contextInit implements DRM_IOCTL_VIRTGPU_CONTEXT_INIT.
func (fd *renderFD) contextInit(t *kernel.Task, cmd uint32, addr hostarch.Addr) error {
	var ci linux.DRMVirtgpuContextInit
	if err := copyInArg(t, cmd, kernelRW, addr, &ci); err != nil {
		return err
	}
	// Linux supports this many parameters.
	if ci.NumParams > 4 {
		return linuxerr.EINVAL
	}
	params := make([]linux.DRMVirtgpuContextSetParam, ci.NumParams)
	if _, err := linux.CopyDRMVirtgpuContextSetParamSliceIn(t, hostarch.Addr(ci.CtxSetParams), params); err != nil {
		return err
	}

	fd.mu.Lock()
	defer fd.mu.Unlock()
	if fd.ctxCreated {
		return linuxerr.EEXIST
	}
	var (
		capsetID     uint32
		haveCapset   bool
		numRings     uint32
		haveNumRings bool
		name         string
		haveName     bool
		// havePollRings is whether the parameters had a mask of rings to
		// poll.
		havePollRings bool
	)
	for _, p := range params {
		switch p.Param {
		case linux.VIRTGPU_CONTEXT_PARAM_CAPSET_ID:
			if haveCapset || p.Value > maxCapsetID || fd.dev.capsetMask&(1<<p.Value) == 0 {
				return linuxerr.EINVAL
			}
			capsetID, haveCapset = uint32(p.Value), true
		case linux.VIRTGPU_CONTEXT_PARAM_NUM_RINGS:
			if haveNumRings || p.Value > maxRings {
				return linuxerr.EINVAL
			}
			numRings, haveNumRings = uint32(p.Value), true
		case linux.VIRTGPU_CONTEXT_PARAM_DEBUG_NAME:
			if haveName {
				return linuxerr.EINVAL
			}
			var err error
			if name, err = copyInDebugName(t, hostarch.Addr(p.Value)); err != nil {
				return err
			}
			haveName = true
		case linux.VIRTGPU_CONTEXT_PARAM_POLL_RINGS_MASK:
			// The device delivers no fence events through the file, which
			// a mask of no rings asks for, as Mesa's Venus driver's does.
			if havePollRings || p.Value != 0 {
				return linuxerr.EINVAL
			}
			havePollRings = true
		default:
			return linuxerr.EINVAL
		}
	}
	if !haveName {
		name = t.Name()
	}
	_, err := fd.createContextLocked(capsetID, numRings, name)
	return err
}

// copyInDebugName copies in a context's debug name, truncated like Linux
// truncates it.
func copyInDebugName(t *kernel.Task, addr hostarch.Addr) (string, error) {
	name, err := t.CopyInString(addr, maxDebugNameLen+1)
	if err == nil {
		return name, nil
	}
	if !linuxerr.Equals(linuxerr.ENAMETOOLONG, err) {
		return "", err
	}
	buf := make([]byte, maxDebugNameLen)
	if _, err := t.CopyInBytes(addr, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

// createContextLocked creates the file's context.
//
// Preconditions: fd.mu is locked. The file has no context.
func (fd *renderFD) createContextLocked(capsetID, numRings uint32, name string) (uint64, error) {
	d := fd.dev
	d.mu.Lock()
	full := d.contexts >= d.maxContexts
	d.mu.Unlock()
	if full {
		return 0, linuxerr.ENOSPC
	}
	d.submitMu.Lock()
	gen, err := d.startRendererLocked()
	if err == nil {
		if err = d.renderer.CreateContext(fd.ctxID, capsetID, name); err != nil {
			log.Warningf("virtgpu: creating context %d with capability set %d: %v", fd.ctxID, capsetID, err)
			err = linuxerr.EINVAL
		}
	}
	d.submitMu.Unlock()
	if err != nil {
		return 0, err
	}
	d.mu.Lock()
	if gen == d.gen {
		d.contexts++
	}
	d.mu.Unlock()
	fd.ctxCreated = true
	fd.ctxGen = gen
	fd.numRings = numRings
	return gen, nil
}

// contextLocked returns the generation of the file's context, which it
// creates with the default capability set if the file has none, as Linux's
// driver does. It returns ENODEV if the context was lost.
//
// Preconditions: fd.mu is locked.
func (fd *renderFD) contextLocked(t *kernel.Task) (uint64, error) {
	if !fd.ctxCreated {
		return fd.createContextLocked(0, 0, t.Name())
	}
	d := fd.dev
	d.mu.Lock()
	lost := fd.ctxGen != d.gen || d.lost
	d.mu.Unlock()
	if lost {
		return 0, linuxerr.ENODEV
	}
	return fd.ctxGen, nil
}

// lookupLocked returns the resource of a handle, with a reference.
//
// Preconditions: fd.mu is locked.
func (fd *renderFD) lookupLocked(handle uint32) (*resource, error) {
	r, ok := fd.objects[handle]
	if !ok {
		return nil, linuxerr.ENOENT
	}
	r.IncRef()
	return r, nil
}

// lookup returns the resource of a handle, with a reference.
func (fd *renderFD) lookup(handle uint32) (*resource, error) {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	return fd.lookupLocked(handle)
}

// newHandleLocked gives the file a handle for r, which takes a reference on
// r, and, like Linux's driver, creates the file's context if it has none, and
// lets the context use r.
//
// Preconditions: fd.mu is locked.
func (fd *renderFD) newHandleLocked(t *kernel.Task, r *resource) (uint32, error) {
	handle, ok := fd.handles.alloc()
	if !ok {
		return 0, linuxerr.ENOSPC
	}
	r.IncRef()
	fd.objects[handle] = r
	fd.handleCounts[r]++
	// Linux ignores a failure to create the context here.
	if gen, err := fd.contextLocked(t); err == nil && gen == r.gen {
		d := fd.dev
		d.submitMu.Lock()
		if renderer, err := d.rendererLocked(gen); err == nil {
			renderer.AttachResource(fd.ctxID, r.id)
		}
		d.submitMu.Unlock()
	}
	return handle, nil
}

// detach ends the use of r by the file's context of generation gen.
func (fd *renderFD) detach(gen uint64, r *resource) {
	if gen != r.gen {
		return
	}
	d := fd.dev
	d.submitMu.Lock()
	if renderer, err := d.rendererLocked(gen); err == nil {
		renderer.DetachResource(fd.ctxID, r.id)
	}
	d.submitMu.Unlock()
}

// resourceParams describes a resource to create.
type resourceParams struct {
	blob      bool
	blobMem   uint32
	blobFlags uint32
	size      uint64
}

// newResource creates a resource with create, which asks the renderer to
// create it, and gives the file a handle for it.
func (fd *renderFD) newResource(t *kernel.Task, params resourceParams, create func(r Renderer, ctxID, resID uint32) error) (*resource, uint32, error) {
	d := fd.dev
	size, ok := hostarch.Addr(params.size).RoundUp()
	if !ok || params.size == 0 || uint64(size) > maxResourceSize {
		return nil, 0, linuxerr.EINVAL
	}

	fd.mu.Lock()
	defer fd.mu.Unlock()
	gen, err := fd.contextLocked(t)
	if err != nil {
		return nil, 0, err
	}

	d.mu.Lock()
	id, err := d.allocResourceIDLocked()
	if err != nil {
		d.mu.Unlock()
		return nil, 0, err
	}
	if d.nextMapOffset+uint64(size) > 1<<63-1 {
		d.mu.Unlock()
		return nil, 0, linuxerr.ENOSPC
	}
	r := &resource{
		dev:       d,
		id:        id,
		gen:       gen,
		blob:      params.blob,
		blobMem:   params.blobMem,
		blobFlags: params.blobFlags,
		size:      uint64(size),
		mapOffset: d.nextMapOffset,
	}
	r.InitRefs()
	d.nextMapOffset += uint64(size)
	// Reserve the ID and offset while the renderer creates the resource.
	d.resources[id] = r
	d.mapOffsets[r.mapOffset] = r
	if gen == d.gen {
		d.genResources++
	}
	d.mu.Unlock()

	d.submitMu.Lock()
	renderer, err := d.rendererLocked(gen)
	if err == nil {
		if err = create(renderer, fd.ctxID, id); err != nil {
			log.Warningf("virtgpu: creating resource %d: %v", id, err)
			err = linuxerr.EINVAL
		}
	}
	d.submitMu.Unlock()
	if err != nil {
		d.mu.Lock()
		delete(d.resources, id)
		delete(d.mapOffsets, r.mapOffset)
		if gen == d.gen {
			d.genResources--
		}
		d.mu.Unlock()
		// The renderer has nothing to destroy.
		r.resourceRefs.DecRef(nil)
		return nil, 0, err
	}

	// The handle takes over the creation's reference.
	handle, err := fd.newHandleLocked(t, r)
	r.DecRef(t)
	if err != nil {
		return nil, 0, err
	}
	return r, handle, nil
}

// resourceCreate implements DRM_IOCTL_VIRTGPU_RESOURCE_CREATE.
func (fd *renderFD) resourceCreate(t *kernel.Task, cmd uint32, addr hostarch.Addr) error {
	var rc linux.DRMVirtgpuResourceCreate
	if err := copyInArg(t, cmd, kernelRW, addr, &rc); err != nil {
		return err
	}
	size := uint64(rc.Size)
	if size == 0 {
		// Like Linux, allocate a page.
		size = hostarch.PageSize
	}
	args := ResourceArgs{
		Target:    rc.Target,
		Format:    rc.Format,
		Bind:      rc.Bind,
		Width:     rc.Width,
		Height:    rc.Height,
		Depth:     rc.Depth,
		ArraySize: rc.ArraySize,
		LastLevel: rc.LastLevel,
		NrSamples: rc.NrSamples,
		Flags:     rc.Flags,
	}
	r, handle, err := fd.newResource(t, resourceParams{size: size}, func(renderer Renderer, ctxID, resID uint32) error {
		a := args
		a.Size = size
		return renderer.CreateResource(ctxID, resID, a)
	})
	if err != nil {
		return err
	}
	rc.BOHandle = handle
	rc.ResHandle = r.id
	return copyOutArg(t, cmd, kernelRW, addr, &rc)
}

// resourceCreateBlob implements DRM_IOCTL_VIRTGPU_RESOURCE_CREATE_BLOB.
func (fd *renderFD) resourceCreateBlob(t *kernel.Task, cmd uint32, addr hostarch.Addr) error {
	var rc linux.DRMVirtgpuResourceCreateBlob
	if err := copyInArg(t, cmd, kernelRW, addr, &rc); err != nil {
		return err
	}
	// See drivers/gpu/drm/virtio/virtgpu_ioctl.c:verify_blob().
	const knownFlags = linux.VIRTGPU_BLOB_FLAG_USE_MAPPABLE | linux.VIRTGPU_BLOB_FLAG_USE_SHAREABLE | linux.VIRTGPU_BLOB_FLAG_USE_CROSS_DEVICE
	if rc.BlobFlags&^knownFlags != 0 || rc.BlobFlags&linux.VIRTGPU_BLOB_FLAG_USE_CROSS_DEVICE != 0 {
		return linuxerr.EINVAL
	}
	switch rc.BlobMem {
	case linux.VIRTGPU_BLOB_MEM_GUEST:
		if rc.BlobID != 0 || rc.CmdSize != 0 {
			return linuxerr.EINVAL
		}
	case linux.VIRTGPU_BLOB_MEM_HOST3D, linux.VIRTGPU_BLOB_MEM_HOST3D_GUEST:
		if rc.CmdSize%4 != 0 {
			return linuxerr.EINVAL
		}
	default:
		return linuxerr.EINVAL
	}
	if rc.CmdSize > maxCommandSize {
		return linuxerr.ENOMEM
	}
	var blobCmd []byte
	if rc.CmdSize != 0 {
		blobCmd = make([]byte, rc.CmdSize)
		if _, err := t.CopyInBytes(hostarch.Addr(rc.Cmd), blobCmd); err != nil {
			return err
		}
	}
	args := BlobArgs{
		Mem:   rc.BlobMem,
		Flags: rc.BlobFlags,
		ID:    rc.BlobID,
		Size:  rc.Size,
	}
	params := resourceParams{
		blob:      true,
		blobMem:   rc.BlobMem,
		blobFlags: rc.BlobFlags,
		size:      rc.Size,
	}
	r, handle, err := fd.newResource(t, params, func(renderer Renderer, ctxID, resID uint32) error {
		if blobCmd != nil {
			if err := renderer.SubmitCommand(ctxID, blobCmd); err != nil {
				return err
			}
		}
		return renderer.CreateBlob(ctxID, resID, args)
	})
	if err != nil {
		return err
	}
	rc.BOHandle = handle
	rc.ResHandle = r.id
	return copyOutArg(t, cmd, kernelRW, addr, &rc)
}

// resourceInfo implements DRM_IOCTL_VIRTGPU_RESOURCE_INFO.
func (fd *renderFD) resourceInfo(t *kernel.Task, cmd uint32, addr hostarch.Addr) error {
	var ri linux.DRMVirtgpuResourceInfo
	if err := copyInArg(t, cmd, kernelRW, addr, &ri); err != nil {
		return err
	}
	r, err := fd.lookup(ri.BOHandle)
	if err != nil {
		return err
	}
	defer r.DecRef(t)
	ri.Size = uint32(r.size)
	ri.ResHandle = r.id
	if r.host3D() {
		ri.BlobMem = r.blobMem
	}
	return copyOutArg(t, cmd, kernelRW, addr, &ri)
}

// mapIoctl implements DRM_IOCTL_VIRTGPU_MAP.
func (fd *renderFD) mapIoctl(t *kernel.Task, cmd uint32, addr hostarch.Addr) error {
	var m linux.DRMVirtgpuMap
	if err := copyInArg(t, cmd, kernelRW, addr, &m); err != nil {
		return err
	}
	r, err := fd.lookup(m.Handle)
	if err != nil {
		return err
	}
	m.Offset = r.mapOffset
	r.DecRef(t)
	return copyOutArg(t, cmd, kernelRW, addr, &m)
}

// execbuffer implements DRM_IOCTL_VIRTGPU_EXECBUFFER. It blocks in ctx, which
// is t's.
func (fd *renderFD) execbuffer(ctx context.Context, t *kernel.Task, cmd uint32, addr hostarch.Addr) error {
	var eb linux.DRMVirtgpuExecbuffer
	if err := copyInArg(t, cmd, kernelRW, addr, &eb); err != nil {
		return err
	}
	const knownFlags = linux.VIRTGPU_EXECBUF_FENCE_FD_IN | linux.VIRTGPU_EXECBUF_FENCE_FD_OUT | linux.VIRTGPU_EXECBUF_RING_IDX
	if eb.Flags&^knownFlags != 0 {
		return linuxerr.EINVAL
	}
	if eb.NumInSyncobjs != 0 || eb.NumOutSyncobjs != 0 {
		// The device does not support DRM sync objects.
		return linuxerr.EINVAL
	}
	if eb.Size > maxCommandSize || eb.NumBOHandles > maxBOHandles {
		return linuxerr.ENOMEM
	}
	hasRing := eb.Flags&linux.VIRTGPU_EXECBUF_RING_IDX != 0

	// Copy in what the submission needs before it waits for its fence.
	command := make([]byte, eb.Size)
	if _, err := t.CopyInBytes(hostarch.Addr(eb.Command), command); err != nil {
		return err
	}
	handles := make([]uint32, eb.NumBOHandles)
	if len(handles) != 0 {
		if _, err := primitive.CopyUint32SliceIn(t, hostarch.Addr(eb.BOHandles), handles); err != nil {
			return err
		}
	}

	fd.mu.Lock()
	gen, err := fd.contextLocked(t)
	if err == nil && hasRing && eb.RingIdx >= fd.numRings {
		err = linuxerr.EINVAL
	}
	var bos []*resource
	for _, h := range handles {
		if err != nil {
			break
		}
		var r *resource
		if r, err = fd.lookupLocked(h); err == nil {
			bos = append(bos, r)
		}
	}
	fd.mu.Unlock()
	defer func() {
		for _, r := range bos {
			r.DecRef(t)
		}
	}()
	if err != nil {
		return err
	}
	ring := timelineKey{global: true}
	if hasRing {
		ring = timelineKey{ctxID: fd.ctxID, ring: eb.RingIdx}
	}

	d := fd.dev
	if eb.Flags&linux.VIRTGPU_EXECBUF_FENCE_FD_IN != 0 {
		in, err := syncFileFence(t, eb.FenceFD)
		if err != nil {
			return err
		}
		// Like Linux, wait for the fences of other timelines.
		for _, f := range in.leaves() {
			if f.timeline == ring {
				continue
			}
			if err := d.waitFence(ctx, f, false /* haveTimeout */, 0); err != nil {
				return err
			}
		}
	}

	needFence := eb.Flags&linux.VIRTGPU_EXECBUF_FENCE_FD_OUT != 0 || len(bos) != 0
	d.submitMu.Lock()
	renderer, err := d.rendererLocked(gen)
	if err == nil {
		if err = renderer.SubmitCommand(fd.ctxID, command); err != nil {
			log.Warningf("virtgpu: submitting to context %d: %v", fd.ctxID, err)
			err = linuxerr.EIO
		}
	}
	var f *fence
	if err == nil && needFence {
		f = d.createFenceLocked(renderer, fd.ctxID, hasRing, eb.RingIdx)
	}
	d.submitMu.Unlock()
	if err != nil {
		return err
	}
	if f == nil {
		return nil
	}
	d.mu.Lock()
	for _, r := range bos {
		r.addFenceLocked(f)
	}
	d.mu.Unlock()
	if eb.Flags&linux.VIRTGPU_EXECBUF_FENCE_FD_OUT == 0 {
		return nil
	}
	out, err := d.newSyncFile(t, f, "")
	if err != nil {
		return err
	}
	defer out.DecRef(t)
	outFD, err := t.NewFDFrom(0, out, kernel.FDFlags{CloseOnExec: true})
	if err != nil {
		return err
	}
	eb.FenceFD = outFD
	return copyOutArg(t, cmd, kernelRW, addr, &eb)
}

// transfer implements DRM_IOCTL_VIRTGPU_TRANSFER_TO_HOST and
// DRM_IOCTL_VIRTGPU_TRANSFER_FROM_HOST.
func (fd *renderFD) transfer(t *kernel.Task, cmd uint32, addr hostarch.Addr, toHost bool) error {
	var x linux.DRMVirtgpu3DTransfer
	if err := copyInArg(t, cmd, kernelRW, addr, &x); err != nil {
		return err
	}
	fd.mu.Lock()
	r, err := fd.lookupLocked(x.BOHandle)
	if err != nil {
		fd.mu.Unlock()
		return err
	}
	defer r.DecRef(t)
	// See virtio_gpu_transfer_to_host_ioctl(): a guest blob has no copy on
	// the host, and only the renderer's blobs take strides.
	if r.blob && !r.host3D() {
		fd.mu.Unlock()
		return linuxerr.EINVAL
	}
	if !r.host3D() && (x.Stride != 0 || x.LayerStride != 0) {
		fd.mu.Unlock()
		return linuxerr.EINVAL
	}
	gen, err := fd.contextLocked(t)
	fd.mu.Unlock()
	if err != nil {
		return err
	}
	if gen != r.gen {
		return linuxerr.ENODEV
	}
	args := TransferArgs{
		ToHost:      toHost,
		X:           x.Box.X,
		Y:           x.Box.Y,
		Z:           x.Box.Z,
		W:           x.Box.W,
		H:           x.Box.H,
		D:           x.Box.D,
		Level:       x.Level,
		Offset:      x.Offset,
		Stride:      x.Stride,
		LayerStride: x.LayerStride,
	}
	d := fd.dev
	d.submitMu.Lock()
	renderer, err := d.rendererLocked(gen)
	if err == nil {
		if err = renderer.Transfer(fd.ctxID, r.id, args); err != nil {
			log.Warningf("virtgpu: transferring resource %d: %v", r.id, err)
			err = linuxerr.EIO
		}
	}
	var f *fence
	if err == nil {
		// Like Linux, fence the transfer on the global timeline.
		f = d.createFenceLocked(renderer, fd.ctxID, false /* hasRing */, 0)
	}
	d.submitMu.Unlock()
	if err != nil {
		return err
	}
	d.mu.Lock()
	r.addFenceLocked(f)
	d.mu.Unlock()
	return nil
}

// wait implements DRM_IOCTL_VIRTGPU_WAIT. It blocks in ctx, which is t's.
func (fd *renderFD) wait(ctx context.Context, t *kernel.Task, cmd uint32, addr hostarch.Addr) error {
	var w linux.DRMVirtgpu3DWait
	if err := copyInArg(t, cmd, kernelRW, addr, &w); err != nil {
		return err
	}
	r, err := fd.lookup(w.Handle)
	if err != nil {
		return err
	}
	defer r.DecRef(t)
	d := fd.dev
	if w.Flags&linux.VIRTGPU_WAIT_NOWAIT != 0 {
		d.mu.Lock()
		idle := len(r.fences) == 0
		d.mu.Unlock()
		if !idle {
			return linuxerr.EBUSY
		}
		return nil
	}
	return d.waitIdle(ctx, r, waitTimeout)
}

// waitIdle blocks until r's fences are signaled, as dma_resv_wait_timeout()
// does. It returns EBUSY if the timeout expires.
func (d *device) waitIdle(ctx context.Context, r *resource, timeout time.Duration) error {
	d.mu.Lock()
	f := r.fenceLocked()
	d.mu.Unlock()
	if err := d.waitFence(ctx, f, true /* haveTimeout */, timeout); err != nil {
		if linuxerr.Equals(linuxerr.ETIMEDOUT, err) {
			return linuxerr.EBUSY
		}
		return err
	}
	return nil
}

// gemClose implements DRM_IOCTL_GEM_CLOSE.
func (fd *renderFD) gemClose(t *kernel.Task, cmd uint32, addr hostarch.Addr) error {
	var c linux.DRMGemClose
	if err := copyInArg(t, cmd, kernelW, addr, &c); err != nil {
		return err
	}
	fd.mu.Lock()
	r, ok := fd.objects[c.Handle]
	if !ok {
		fd.mu.Unlock()
		return linuxerr.EINVAL
	}
	fd.removeHandleLocked(c.Handle, r)
	created, gen := fd.ctxCreated, fd.ctxGen
	fd.mu.Unlock()
	if created {
		fd.detach(gen, r)
	}
	r.DecRef(t)
	return nil
}

// removeHandleLocked removes a handle, whose reference the caller takes over.
//
// Preconditions: fd.mu is locked.
func (fd *renderFD) removeHandleLocked(handle uint32, r *resource) {
	delete(fd.objects, handle)
	fd.handles.release(handle)
	if fd.handleCounts[r]--; fd.handleCounts[r] == 0 {
		delete(fd.handleCounts, r)
	}
	if h, ok := fd.prime[r]; ok && h == handle {
		delete(fd.prime, r)
	}
}

// primeHandleToFD implements DRM_IOCTL_PRIME_HANDLE_TO_FD.
func (fd *renderFD) primeHandleToFD(t *kernel.Task, cmd uint32, addr hostarch.Addr) error {
	var p linux.DRMPrimeHandle
	if err := copyInArg(t, cmd, kernelRW, addr, &p); err != nil {
		return err
	}
	if p.Flags&^(linux.DRM_CLOEXEC|linux.DRM_RDWR) != 0 {
		return linuxerr.EINVAL
	}
	r, err := fd.lookup(p.Handle)
	if err != nil {
		return err
	}
	defer r.DecRef(t)
	dmabuf, err := fd.dev.export(t, r, p.Flags&linux.DRM_RDWR != 0)
	if err != nil {
		return err
	}
	defer dmabuf.DecRef(t)
	newFD, err := t.NewFDFrom(0, dmabuf, kernel.FDFlags{CloseOnExec: p.Flags&linux.DRM_CLOEXEC != 0})
	if err != nil {
		return err
	}
	fd.mu.Lock()
	if fd.objects[p.Handle] == r {
		if _, ok := fd.prime[r]; !ok {
			fd.prime[r] = p.Handle
		}
	}
	fd.mu.Unlock()
	p.FD = newFD
	return copyOutArg(t, cmd, kernelRW, addr, &p)
}

// primeFDToHandle implements DRM_IOCTL_PRIME_FD_TO_HANDLE.
func (fd *renderFD) primeFDToHandle(t *kernel.Task, cmd uint32, addr hostarch.Addr) error {
	var p linux.DRMPrimeHandle
	if err := copyInArg(t, cmd, kernelRW, addr, &p); err != nil {
		return err
	}
	file := t.GetFile(p.FD)
	if file == nil {
		return linuxerr.EBADF
	}
	defer file.DecRef(t)
	dmabuf, ok := file.Impl().(*dmabufFD)
	if !ok || dmabuf.res.dev != fd.dev {
		// Linux imports other devices' dma-bufs as guest blobs, which the
		// device's renderer cannot use.
		return linuxerr.EINVAL
	}
	r := dmabuf.res
	fd.mu.Lock()
	handle, ok := fd.prime[r]
	if !ok {
		var err error
		if handle, err = fd.newHandleLocked(t, r); err != nil {
			fd.mu.Unlock()
			return err
		}
		fd.prime[r] = handle
	}
	fd.mu.Unlock()
	p.Handle = handle
	return copyOutArg(t, cmd, kernelRW, addr, &p)
}
