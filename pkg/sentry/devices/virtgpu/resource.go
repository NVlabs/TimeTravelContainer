// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package virtgpu

import (
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/waiter"
)

// resource is a resource of the virtual GPU, a GEM object in Linux's terms.
// GEM handles, mappings, and dma-bufs hold references to it, and the renderer
// destroys it once they are all gone.
//
// +stateify savable
type resource struct {
	resourceRefs

	dev *device
	id  uint32
	// gen is the generation of the renderer that holds the resource.
	gen uint64
	// blob is whether the resource is a blob, and blobMem and blobFlags
	// describe it.
	blob      bool
	blobMem   uint32
	blobFlags uint32
	// size is the resource's size in bytes, a whole number of pages.
	size uint64
	// mapOffset is the fake offset at which the render node maps the
	// resource.
	mapOffset uint64
	// ring is whether the resource is a gfxstream command ring.
	ring bool
	// ringContents is, in a resource that a fork lost, what its command ring
	// held when the fork saved it.
	ringContents []byte `state:".([]byte)"`

	// The following fields are protected by device.mu.

	// fences are the resource's fences that are not yet signaled, the latest
	// of each timeline, as Linux's reservation object for the resource would
	// have them.
	fences map[timelineKey]*fence
	// queue is notified when one of fences is signaled.
	queue waiter.Queue
	// memory is the resource's memory, once mapped. It is not saved: the
	// renderer that it belongs to is not.
	memory ResourceMemory `state:"nosave"`
	// dmabuf is the resource's dma-buf, while it has one.
	dmabuf *dmabufFD
	// lost is the memory of the device's MemoryFile that the resource maps
	// instead of its renderer's, once a load lost the renderer. It is not
	// saved: a later fork's child makes its own.
	lost memmap.FileRange `state:"nosave"`

	// mapsMu protects mappings.
	mapsMu resourceMapsMutex `state:"nosave"`
	// mappings are the resource's mappings.
	mappings memmap.MappingSet
}

var _ memmap.Mappable = (*resource)(nil)
var _ memmap.MappingIdentity = (*resource)(nil)
var _ memmap.LiveForkMappingValidator = (*resource)(nil)

// mappable returns whether the guest may map the resource. Linux maps a
// classic resource's backing, and a blob only if it was created mappable.
func (r *resource) mappable() bool {
	return !r.blob || r.blobFlags&linux.VIRTGPU_BLOB_FLAG_USE_MAPPABLE != 0
}

// host3D returns whether the resource is a blob of the renderer's memory,
// which Linux maps only whole.
func (r *resource) host3D() bool {
	return r.blob && r.blobMem != linux.VIRTGPU_BLOB_MEM_GUEST
}

// DecRef implements memmap.MappingIdentity.DecRef.
func (r *resource) DecRef(ctx context.Context) {
	r.resourceRefs.DecRef(func() {
		r.dev.destroyResource(r)
	})
}

// destroyResource destroys a resource that nothing refers to any more.
func (d *device) destroyResource(r *resource) {
	d.mu.Lock()
	delete(d.resources, r.id)
	delete(d.mapOffsets, r.mapOffset)
	for _, f := range r.fences {
		f.unobserveLocked(r)
	}
	r.fences = nil
	mem := r.memory
	r.memory = nil
	lost := r.lost
	r.lost = memmap.FileRange{}
	d.mu.Unlock()
	if lost.Length() != 0 {
		d.mf.DecRef(lost)
	}

	d.submitMu.Lock()
	if renderer, err := d.rendererLocked(r.gen); err == nil {
		renderer.DestroyResource(r.id)
	}
	d.submitMu.Unlock()
	if mem != nil {
		mem.Release()
	}
}

// fenceSignaledLocked implements fenceObserver.fenceSignaledLocked.
func (r *resource) fenceSignaledLocked(f *fence, notify bool) {
	if r.fences[f.timeline] == f {
		delete(r.fences, f.timeline)
	}
	if notify {
		r.queue.Notify(waiter.ReadableEvents | waiter.WritableEvents)
	}
}

// addFenceLocked adds f to the resource's fences, as a submission that uses
// the resource adds its fence to the resource's reservation object.
//
// Preconditions: device.mu is locked.
func (r *resource) addFenceLocked(f *fence) {
	for _, leaf := range f.leaves() {
		if leaf.signaled {
			continue
		}
		if prev, ok := r.fences[leaf.timeline]; ok {
			if prev.id >= leaf.id {
				continue
			}
			prev.unobserveLocked(r)
		}
		if r.fences == nil {
			r.fences = make(map[timelineKey]*fence)
		}
		r.fences[leaf.timeline] = leaf
		leaf.observeLocked(r)
	}
}

// fenceLocked returns a fence that is signaled once all of the resource's
// current fences are.
//
// Preconditions: device.mu is locked.
func (r *resource) fenceLocked() *fence {
	fences := make([]*fence, 0, len(r.fences))
	for _, f := range r.fences {
		fences = append(fences, f)
	}
	return mergeFencesLocked(fences...)
}

// memoryLocked returns the resource's memory, asking the renderer for it the
// first time.
//
// Preconditions: device.submitMu is locked.
func (r *resource) memoryLocked() (ResourceMemory, error) {
	d := r.dev
	d.mu.Lock()
	mem := r.memory
	d.mu.Unlock()
	if mem != nil {
		return mem, nil
	}
	renderer, err := d.rendererLocked(r.gen)
	if err != nil {
		return nil, err
	}
	mem, err = renderer.MapResource(r.id)
	if err != nil {
		log.Warningf("virtgpu: mapping resource %d: %v", r.id, err)
		return nil, linuxerr.EINVAL
	}
	d.mu.Lock()
	r.memory = mem
	d.mu.Unlock()
	return mem, nil
}

// configureMMap sets up opts, an mmap of the resource from offset, with a
// reference on the resource for the mapping.
func (r *resource) configureMMap(opts *memmap.MMapOpts, offset uint64) error {
	// Like Linux, refuse private mappings, which would copy on write: see
	// drm_gem_shmem_mmap().
	if !r.mappable() || opts.Private {
		return linuxerr.EINVAL
	}
	end, ok := hostarch.Addr(offset).AddLength(opts.Length)
	if !ok || uint64(end) > r.size {
		return linuxerr.EINVAL
	}
	// Linux maps the renderer's memory only whole: see
	// drivers/gpu/drm/virtio/virtgpu_vram.c:virtio_gpu_vram_mmap().
	if r.host3D() && (offset != 0 || opts.Length != r.size) {
		return linuxerr.EINVAL
	}
	d := r.dev
	d.submitMu.Lock()
	_, err := r.memoryLocked()
	d.submitMu.Unlock()
	if err != nil {
		return err
	}
	opts.Offset = offset
	opts.Mappable = r
	opts.MappingIdentity = r
	// The renderer's memory is never executable: platforms that map it into
	// guests in place do not let it be.
	opts.MaxPerms.Execute = false
	r.IncRef()
	return nil
}

// MappedName implements memmap.MappingIdentity.MappedName.
func (r *resource) MappedName(ctx context.Context) string {
	return "/dev/" + renderNodePath
}

// DeviceID implements memmap.MappingIdentity.DeviceID.
func (r *resource) DeviceID() uint64 {
	return 0
}

// InodeID implements memmap.MappingIdentity.InodeID.
func (r *resource) InodeID() uint64 {
	return 0
}

// Msync implements memmap.MappingIdentity.Msync.
func (r *resource) Msync(ctx context.Context, mr memmap.MappableRange) error {
	return nil
}

// AddMapping implements memmap.Mappable.AddMapping.
func (r *resource) AddMapping(ctx context.Context, ms memmap.MappingSpace, ar hostarch.AddrRange, offset uint64, writable bool) error {
	r.mapsMu.Lock()
	defer r.mapsMu.Unlock()
	r.mappings.AddMapping(ms, ar, offset, writable)
	return nil
}

// RemoveMapping implements memmap.Mappable.RemoveMapping.
func (r *resource) RemoveMapping(ctx context.Context, ms memmap.MappingSpace, ar hostarch.AddrRange, offset uint64, writable bool) {
	r.mapsMu.Lock()
	defer r.mapsMu.Unlock()
	r.mappings.RemoveMapping(ms, ar, offset, writable)
}

// CopyMapping implements memmap.Mappable.CopyMapping.
func (r *resource) CopyMapping(ctx context.Context, ms memmap.MappingSpace, srcAR, dstAR hostarch.AddrRange, offset uint64, writable bool) error {
	return r.AddMapping(ctx, ms, dstAR, offset, writable)
}

// Translate implements memmap.Mappable.Translate.
func (r *resource) Translate(ctx context.Context, required, optional memmap.MappableRange, at hostarch.AccessType) ([]memmap.Translation, error) {
	d := r.dev
	d.mu.Lock()
	var file memmap.File
	var offset uint64
	if mem := r.memory; mem != nil {
		file, offset = mem.File(), mem.Offset()
	} else if lostOffset, err := r.lostMemoryLocked(); err == nil {
		file, offset = d.mf, lostOffset
	} else {
		d.mu.Unlock()
		log.Warningf("virtgpu: mapping resource %d: %v", r.id, err)
		return nil, &memmap.BusError{Err: linuxerr.EFAULT}
	}
	d.mu.Unlock()
	var err error
	if required.End > r.size {
		err = &memmap.BusError{Err: linuxerr.EFAULT}
	}
	source := optional.Intersect(memmap.MappableRange{Start: 0, End: r.size})
	if source.Length() == 0 {
		return nil, err
	}
	return []memmap.Translation{
		{
			Source: source,
			File:   file,
			Offset: offset + source.Start,
			Perms:  hostarch.AnyAccess,
		},
	}, err
}

// InvalidateUnsavable implements memmap.Mappable.InvalidateUnsavable. No
// translation of the resource can be saved: they map the renderer's memory,
// which a save cannot encode, or memory that a child makes its own.
func (r *resource) InvalidateUnsavable(ctx context.Context) error {
	r.mapsMu.Lock()
	defer r.mapsMu.Unlock()
	r.mappings.InvalidateAll(memmap.InvalidateOpts{InvalidatePrivate: true})
	return nil
}

// ValidateLiveForkMapping implements memmap.LiveForkMappingValidator.
func (r *resource) ValidateLiveForkMapping() error {
	return r.dev.validateLiveFork()
}
