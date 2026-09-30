// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package virtgpu

import (
	goContext "context"
	"encoding/binary"
	"fmt"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

// This file implements what a fork with ForkDeviceLost does to the guest's
// mappings of the GPU's memory, which belongs to the source's renderer: the
// child's mappings map memory of the child's own. It reads as zeros, as the
// contents of a GPU that was reset may, except for gfxstream's command rings,
// whose guest drivers busy-wait on them and give up only when the ring says
// that it is in error. So the child gets a copy of each ring, in error.

const (
	// ringStorageSize is the size of gfxstream's struct asg_ring_storage,
	// which starts a command ring's blob: the rings' state, which the child
	// copies. The rest of the blob is a transfer buffer.
	ringStorageSize = 3 * 4096

	// ringInErrorOffset is the offset of the in_error field of the ring
	// storage's to_host ring: struct ring_buffer's config, at 2180, holds
	// struct asg_ring_config, whose in_error is its seventh field.
	ringInErrorOffset = 2180 + 6*4
)

// isCommandRing returns whether a blob that a context of capsetID creates is
// the context's command ring. gfxstream's Vulkan driver creates it as a
// mappable host blob with blob ID 0, which names no allocation of the
// renderer's.
func isCommandRing(capsetID, mem, flags uint32, blobID uint64) bool {
	return capsetID == linux.VIRTGPU_DRM_CAPSET_GFXSTREAM_VULKAN &&
		mem == linux.VIRTGPU_BLOB_MEM_HOST3D &&
		flags&linux.VIRTGPU_BLOB_FLAG_USE_MAPPABLE != 0 &&
		blobID == 0
}

// saveRingContents is invoked by stateify. It saves what a command ring's
// storage holds, for a fork's child, which loses the renderer.
func (r *resource) saveRingContents() []byte {
	if !r.ring || r.dev.forkPolicy != ForkDeviceLost || r.size < ringStorageSize {
		return nil
	}
	d := r.dev
	d.mu.Lock()
	mem := r.memory
	d.mu.Unlock()
	if mem == nil {
		return nil
	}
	contents := make([]byte, ringStorageSize)
	fr := memmap.FileRange{Start: mem.Offset(), End: mem.Offset() + ringStorageSize}
	ims, err := mem.File().MapInternal(fr, hostarch.Read)
	if err == nil {
		_, err = safemem.CopySeq(safemem.BlockSeqOf(safemem.BlockFromSafeSlice(contents)), ims)
	}
	if err != nil {
		// The child's ring will be all zeros, and its driver will wait for
		// a renderer that will never answer.
		log.Warningf("virtgpu: saving command ring %d: %v", r.id, err)
		return nil
	}
	return contents
}

// loadRingContents is invoked by stateify.
func (r *resource) loadRingContents(_ goContext.Context, contents []byte) {
	r.ringContents = contents
}

// lostMemoryLocked returns the offset in the device's MemoryFile of the
// memory that the resource, which a load lost, maps instead of its renderer's,
// allocating it the first time.
//
// Preconditions: device.mu is locked. The resource's memory is nil.
func (r *resource) lostMemoryLocked() (uint64, error) {
	if r.lost.Length() != 0 {
		return r.lost.Start, nil
	}
	d := r.dev
	if d.mf == nil || r.gen == d.gen {
		// The memory belonged to a renderer that is gone, not to one that a
		// fork lost.
		return 0, fmt.Errorf("resource %d has no memory", r.id)
	}
	fr, err := d.mf.Allocate(r.size, pgalloc.AllocOpts{Kind: usage.Anonymous, Mode: pgalloc.AllocateAndWritePopulate})
	if err != nil {
		return 0, err
	}
	if len(r.ringContents) == ringStorageSize {
		contents := append([]byte(nil), r.ringContents...)
		binary.LittleEndian.PutUint32(contents[ringInErrorOffset:], 1)
		blocks, lease, err := memmap.MapInternalWithLease(d.mf, memmap.FileRange{Start: fr.Start, End: fr.Start + ringStorageSize}, hostarch.Write)
		if err == nil {
			_, err = safemem.CopySeq(blocks, safemem.BlockSeqOf(safemem.BlockFromSafeSlice(contents)))
		}
		if lease != nil {
			lease.Release()
		}
		if err != nil {
			d.mf.DecRef(fr)
			return 0, err
		}
	}
	r.lost = fr
	return fr.Start, nil
}
