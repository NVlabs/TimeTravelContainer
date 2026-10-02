// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package venus

import (
	"encoding/binary"
	"fmt"
	"sync"
)

// Cross-domain contexts, with which guests' minigbm, Android's gralloc on
// virtio-gpu, allocates Android's buffers: the renderer tells it how to lay
// out an image, as crosvm's does, and gives it shared memory of that layout
// as a blob resource, of the ID of the layout, which processes share as
// dma-bufs. minigbm keeps the layouts that it learned, and creates blobs of
// a layout's ID without asking for it again. The layout is that
// of the host's Vulkan's linear images (see layout_unsafe.go), so that the
// images that guests' Vulkan creates of a buffer (see modifier_unsafe.go)
// share its memory: what the GPU renders to one, the CPU reads, and another
// process samples, without copies.
//
// minigbm's protocol is virtgpu_cross_domain_protocol.h's. Only its queries
// of image requirements are served: Wayland's channels have no use here.

// CapsetCrossDomain is the ID of the cross-domain capability set.
const CapsetCrossDomain = 5

// CrossDomainCapset returns the cross-domain capability set, struct
// CrossDomainCapabilities: version 1, no channels, and dma-bufs of the host's
// GPU memory.
func CrossDomainCapset() []byte {
	var b []byte
	for _, v := range []uint32{1, 0, 1, 1} {
		b = binary.LittleEndian.AppendUint32(b, v)
	}
	return b
}

// Cross-domain commands.
const (
	crossDomainCmdInit                 = 1
	crossDomainCmdGetImageRequirements = 2
	crossDomainHeaderSize              = 8
)

// crossDomain is the state of a cross-domain context.
type crossDomain struct {
	mu sync.Mutex
	// ring is the resource of the guest's query ring, where the renderer
	// writes replies.
	ring uint32
	// layouts are the sizes of the images whose requirements the guest
	// queried, by blob ID, and the IDs by query.
	sizes   map[uint32]uint64
	layouts map[imageQuery]uint32
}

// imageQuery is a query of an image's requirements.
type imageQuery struct {
	width, height, format, flags uint32
}

func newCrossDomain() *crossDomain {
	return &crossDomain{sizes: make(map[uint32]uint64), layouts: make(map[imageQuery]uint32)}
}

// clone returns a copy of cd for a fork's child.
func (cd *crossDomain) clone() *crossDomain {
	cd.mu.Lock()
	defer cd.mu.Unlock()
	c := &crossDomain{ring: cd.ring, sizes: make(map[uint32]uint64, len(cd.sizes)), layouts: make(map[imageQuery]uint32, len(cd.layouts))}
	for id, size := range cd.sizes {
		c.sizes[id] = size
	}
	for q, id := range cd.layouts {
		c.layouts[q] = id
	}
	return c
}

// submit executes cross-domain commands of context c.
func (cd *crossDomain) submit(c *Context, cmds []byte) error {
	for len(cmds) != 0 {
		if len(cmds) < crossDomainHeaderSize {
			return fmt.Errorf("a cross-domain command of %d bytes", len(cmds))
		}
		size := int(binary.LittleEndian.Uint16(cmds[2:]))
		if size < crossDomainHeaderSize || size > len(cmds) {
			return fmt.Errorf("a cross-domain command of %d bytes, of %d", size, len(cmds))
		}
		cmd := cmds[:size]
		cmds = cmds[size:]
		switch cmd[0] {
		case crossDomainCmdInit:
			if size < 16 {
				return fmt.Errorf("an init command of %d bytes", size)
			}
			cd.mu.Lock()
			cd.ring = binary.LittleEndian.Uint32(cmd[8:])
			cd.mu.Unlock()
		case crossDomainCmdGetImageRequirements:
			if size < 24 {
				return fmt.Errorf("an image requirements command of %d bytes", size)
			}
			width := binary.LittleEndian.Uint32(cmd[8:])
			height := binary.LittleEndian.Uint32(cmd[12:])
			format := binary.LittleEndian.Uint32(cmd[16:])
			flags := binary.LittleEndian.Uint32(cmd[20:])
			if err := cd.imageRequirements(c, width, height, format, flags); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported cross-domain command %d", cmd[0])
		}
	}
	return nil
}

// minigbm's buffer usages, BO_USE_*.
const (
	boUseRendering = 1 << 2
	boUseTexture   = 1 << 5
)

// Formats of DRM, fourcc codes.
const (
	drmFormatR8          = 0x20203852 // R8
	drmFormatRGB565      = 0x36314752 // RG16
	drmFormatBGR888      = 0x34324742 // BG24
	drmFormatXRGB8888    = 0x34325258 // XR24
	drmFormatXBGR8888    = 0x34324258 // XB24
	drmFormatARGB8888    = 0x34325241 // AR24
	drmFormatABGR8888    = 0x34324241 // AB24
	drmFormatABGR2101010 = 0x30334241 // AB30
	drmFormatXBGR2101010 = 0x30334258 // XB30
	drmFormatABGR16F     = 0x48344241 // AB4H
	drmFormatNV12        = 0x3231564e // NV12
	drmFormatNV21        = 0x3132564e // NV21
	drmFormatYVU420      = 0x32315659 // YV12
	drmFormatP010        = 0x30313050 // P010
)

// vkFormatOfDRM returns the Vulkan format of a DRM format whose images the
// host's Vulkan lays out, or 0.
func vkFormatOfDRM(format uint32) uint32 {
	switch format {
	case drmFormatR8:
		return 9 // VK_FORMAT_R8_UNORM
	case drmFormatRGB565:
		return 4 // VK_FORMAT_R5G6B5_UNORM_PACK16
	case drmFormatXBGR8888, drmFormatABGR8888:
		return 37 // VK_FORMAT_R8G8B8A8_UNORM
	case drmFormatXRGB8888, drmFormatARGB8888:
		return 44 // VK_FORMAT_B8G8R8A8_UNORM
	case drmFormatABGR2101010, drmFormatXBGR2101010:
		return 64 // VK_FORMAT_A2B10G10R10_UNORM_PACK32
	case drmFormatABGR16F:
		return 97 // VK_FORMAT_R16G16B16A16_SFLOAT
	}
	return 0
}

// cpuLayout lays out an image of a DRM format whose images the host's
// Vulkan does not lay out, as minigbm lays out those that the CPU alone
// uses: rows of 64-byte multiples, and planes one after another.
func cpuLayout(format, width, height uint32) (imageLayout, bool) {
	align := func(n, a uint64) uint64 { return (n + a - 1) &^ (a - 1) }
	w, h := uint64(width), uint64(height)
	var l imageLayout
	plane := func(rowBytes, rows uint64) {
		pitch := align(rowBytes, 64)
		l.planes = append(l.planes, planeLayout{offset: l.size, rowPitch: pitch})
		l.size += pitch * rows
	}
	switch format {
	case drmFormatBGR888:
		plane(3*w, h)
	case drmFormatNV12, drmFormatNV21:
		plane(w, h)
		plane(2*((w+1)/2), (h+1)/2)
	case drmFormatP010:
		plane(2*w, h)
		plane(4*((w+1)/2), (h+1)/2)
	case drmFormatYVU420:
		// Android's YV12: chroma rows are half the luma's, of 16-byte
		// multiples.
		pitch := align(w, 16)
		l.planes = append(l.planes, planeLayout{offset: 0, rowPitch: pitch})
		l.size = pitch * h
		cpitch := align(pitch/2, 16)
		for i := 0; i < 2; i++ {
			l.planes = append(l.planes, planeLayout{offset: l.size, rowPitch: cpitch})
			l.size += cpitch * ((h + 1) / 2)
		}
	default:
		if vkFormatOfDRM(format) == 0 {
			return imageLayout{}, false
		}
		// A format of the host's whose image it cannot create linear.
		bpp := uint64(4)
		switch format {
		case drmFormatR8:
			bpp = 1
		case drmFormatRGB565:
			bpp = 2
		case drmFormatABGR16F:
			bpp = 8
		}
		plane(bpp*w, h)
	}
	return l, true
}

// imageRequirements answers a query of the requirements of an image, which
// it allocates: the reply in the query ring is struct
// CrossDomainImageRequirements.
func (cd *crossDomain) imageRequirements(c *Context, width, height, format, flags uint32) error {
	if width == 0 || height == 0 || width > 16384 || height > 16384 {
		return fmt.Errorf("an image of %dx%d", width, height)
	}
	var l imageLayout
	ok := false
	if vkFormat := vkFormatOfDRM(format); vkFormat != 0 {
		usage := uint32(imageUsageTransfers)
		if flags&boUseTexture != 0 {
			usage |= imageUsageSampled
		}
		if flags&boUseRendering != 0 {
			usage |= imageUsageColorAttachment
		}
		if d, err := getLayoutDevice(); err != nil {
			c.logf("no layout device: %v", err)
		} else if l, err = d.linearLayout(vkFormat, width, height, usage, 1); err != nil {
			c.logf("%v", err)
		} else {
			ok = true
		}
	}
	if !ok {
		if l, ok = cpuLayout(format, width, height); !ok {
			return fmt.Errorf("an image of unsupported format %#x", format)
		}
	}
	cd.mu.Lock()
	defer cd.mu.Unlock()
	c.resMu.Lock()
	ring := c.resources[cd.ring]
	c.resMu.Unlock()
	if ring == nil || len(ring.mem.data) < 64 {
		return fmt.Errorf("no query ring %d", cd.ring)
	}
	q := imageQuery{width, height, format, flags}
	id, ok := cd.layouts[q]
	if !ok {
		id = uint32(len(cd.layouts) + 1)
		cd.layouts[q] = id
		cd.sizes[id] = l.size
	}
	reply := ring.mem.data[:64]
	clear(reply)
	for i, p := range l.planes {
		binary.LittleEndian.PutUint32(reply[4*i:], uint32(p.rowPitch))
		binary.LittleEndian.PutUint32(reply[16+4*i:], uint32(p.offset))
	}
	binary.LittleEndian.PutUint64(reply[32:], 0) // DRM_FORMAT_MOD_LINEAR
	binary.LittleEndian.PutUint64(reply[40:], l.size)
	binary.LittleEndian.PutUint32(reply[48:], id)
	binary.LittleEndian.PutUint32(reply[52:], mapCacheCached)
	binary.LittleEndian.PutUint32(reply[56:], ^uint32(0)) // memory_idx
	binary.LittleEndian.PutUint32(reply[60:], ^uint32(0)) // physical_device_idx
	return nil
}

// newBlob returns memory for a blob resource of size bytes of an image
// whose requirements the guest queried, of blob ID id.
func (cd *crossDomain) newBlob(id uint32, size uint64) (*sharedMemory, error) {
	cd.mu.Lock()
	want, ok := cd.sizes[id]
	cd.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no image of blob ID %d", id)
	}
	if size > want {
		return nil, fmt.Errorf("a blob of %d bytes of an image of %d", size, want)
	}
	return newSharedMemory(want)
}
