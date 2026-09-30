// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package virtgpu

import (
	"gvisor.dev/gvisor/pkg/sentry/memmap"
)

// Renderer renders what the contexts of a virtual GPU submit. It plays the
// part of a virtual machine's virtio-gpu device: the Sentry implements the
// guest kernel's driver, and hands the Renderer what that driver would queue
// for the device. Command buffers are opaque to the Sentry.
//
// The Sentry chooses every ID that a Renderer sees, of contexts, resources,
// and fences, so that they can outlive a Renderer across forks.
//
// The Sentry makes one call at a time, in the order in which the guest's
// driver would queue the commands. The Renderer reports fences and its own
// loss through the RendererEvents that it was created with, from any
// goroutine, and without waiting for the Sentry's calls to return.
type Renderer interface {
	// CreateContext creates a context of the given capability set.
	CreateContext(ctxID, capsetID uint32, name string) error

	// DestroyContext destroys a context.
	DestroyContext(ctxID uint32)

	// CreateResource creates a classic resource, whose contents the guest
	// moves to and from the renderer's copy with transfers.
	CreateResource(ctxID, resID uint32, args ResourceArgs) error

	// CreateBlob creates a blob resource, after the context's command, if
	// any, that describes it.
	CreateBlob(ctxID, resID uint32, args BlobArgs) error

	// MapResource returns the memory that the guest maps for a resource: a
	// classic resource's backing, or a mappable blob's memory.
	MapResource(resID uint32) (ResourceMemory, error)

	// AttachResource lets a context use a resource.
	AttachResource(ctxID, resID uint32)

	// DetachResource ends a context's use of a resource.
	DetachResource(ctxID, resID uint32)

	// DestroyResource destroys a resource.
	DestroyResource(resID uint32)

	// SubmitCommand submits a command buffer to a context.
	SubmitCommand(ctxID uint32, cmd []byte) error

	// Transfer copies between a classic resource's backing and the
	// renderer's copy of the resource.
	Transfer(ctxID, resID uint32, args TransferArgs) error

	// CreateFence asks the renderer to signal a fence once everything
	// submitted to its timeline before it is done.
	CreateFence(f Fence) error

	// Close releases the Renderer. It reports nothing afterward.
	Close()
}

// RendererEvents receives what a Renderer reports.
type RendererEvents interface {
	// FenceSignaled reports that a fence is signaled, and with it every
	// fence created before it on its timeline.
	FenceSignaled(f Fence)

	// RendererLost reports that the Renderer stopped. It will signal no more
	// fences.
	RendererLost(err error)
}

// NewRendererFunc starts a Renderer that reports to events.
type NewRendererFunc func(events RendererEvents) (Renderer, error)

// Fence identifies a fence to a Renderer.
type Fence struct {
	// ID is unique among the device's fences, and grows with each fence
	// that the device creates.
	ID uint64

	// CtxID is the context whose submission the fence follows.
	CtxID uint32

	// HasRing is whether the fence is on the timeline of the context's ring
	// Ring. Otherwise, it is on the device's global timeline.
	HasRing bool
	Ring    uint32
}

// Capset is a capability set of a renderer: a kind of context that it
// supports, and what the guest's driver for that kind needs to know about
// the renderer.
//
// +stateify savable
type Capset struct {
	// ID is one of linux.VIRTGPU_DRM_CAPSET_*.
	ID uint32
	// MaxVersion is the latest version of the capability set.
	MaxVersion uint32
	// Data is the capability set, whichever version the guest asks for.
	Data []byte
}

// ResourceArgs describes a classic resource, as struct
// drm_virtgpu_resource_create does.
type ResourceArgs struct {
	Target    uint32
	Format    uint32
	Bind      uint32
	Width     uint32
	Height    uint32
	Depth     uint32
	ArraySize uint32
	LastLevel uint32
	NrSamples uint32
	Flags     uint32
	// Size is the size of the resource's backing, in bytes.
	Size uint64
}

// BlobArgs describes a blob resource.
type BlobArgs struct {
	// Mem is one of linux.VIRTGPU_BLOB_MEM_*.
	Mem uint32
	// Flags is a mask of linux.VIRTGPU_BLOB_FLAG_*.
	Flags uint32
	// ID names what the context has allocated for the blob, or is 0.
	ID uint64
	// Size is the blob's size, in bytes.
	Size uint64
}

// TransferArgs describes a transfer, as struct
// drm_virtgpu_3d_transfer_to_host does.
type TransferArgs struct {
	// ToHost is whether the transfer is to the renderer's copy, rather than
	// from it.
	ToHost      bool
	X, Y, Z     uint32
	W, H, D     uint32
	Level       uint32
	Offset      uint32
	Stride      uint32
	LayerStride uint32
}

// ResourceMemory is the memory of a resource that the guest maps.
type ResourceMemory interface {
	// File returns the memmap.File that holds the memory, from Offset for
	// the resource's size.
	File() memmap.File

	// Offset returns where the memory starts in File.
	Offset() uint64

	// Release releases the memory. The device calls it once, after the
	// renderer destroyed the resource and nothing maps it any more.
	Release()
}
