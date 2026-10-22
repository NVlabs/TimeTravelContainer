// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package vkgl

import (
	"fmt"
	"math/bits"
)

// Gallium's resource targets, of pipe_texture_target.
const (
	TargetBuffer = iota
	Target1D
	Target2D
	Target3D
	TargetCube
	TargetRect
	Target1DArray
	Target2DArray
	TargetCubeArray
	numTargets
)

// ResourceDesc is a resource's parameters, Gallium's pipe_resource's, as
// virgl's resources have them.
type ResourceDesc struct {
	Target    uint32
	Format    uint32
	Bind      uint32
	Width     uint32
	Height    uint32
	Depth     uint32
	ArraySize uint32
	LastLevel uint32
	Samples   uint32
}

// Limits are a device's limits of its resources.
type Limits struct {
	Max2DSize      uint32
	MaxCubeLevels  uint32
	Max3DLevels    uint32
	MaxArrayLayers uint32
}

// normalize returns d of at least 1 of each dimension, a buffer of at
// least a byte, and no samples of 1 sample, as the decoder's pipe.c takes
// virgl's resources, as virglrenderer does.
func (d ResourceDesc) normalize() ResourceDesc {
	if d.Target == TargetBuffer {
		d.Width = max(d.Width, 1)
	}
	d.Height = max(d.Height, 1)
	d.Depth = max(d.Depth, 1)
	d.ArraySize = max(d.ArraySize, 1)
	if d.Samples <= 1 {
		d.Samples = 0
	}
	return d
}

// minify returns the size of a level of a dimension of size.
func minify(size, level uint32) uint32 {
	if level >= 32 {
		return 1
	}
	return max(size>>level, 1)
}

// check returns an error if d, normalized, is not a resource that the
// device of l has, as the decoder's pipe.c checks virgl's.
func (d ResourceDesc) check(l Limits) error {
	bad := func() error { return fmt.Errorf("a resource of %+v", d) }
	if d.Target >= numTargets || d.Width == 0 || d.Samples > 32 || d.Samples&(d.Samples-1) != 0 || d.LastLevel > 15 {
		return bad()
	}
	if d.Samples > 1 && (d.Target != Target2D && d.Target != Target2DArray || d.LastLevel > 0) {
		return bad()
	}
	switch d.Target {
	case TargetBuffer:
		if d.Height != 1 || d.Depth != 1 || d.ArraySize != 1 || d.LastLevel != 0 {
			return bad()
		}
		return nil
	case Target1D, Target1DArray:
		if d.Height != 1 || d.Depth != 1 || d.Width > l.Max2DSize {
			return bad()
		}
	case TargetRect, Target2D, Target2DArray:
		if d.Target == TargetRect && d.LastLevel > 0 || d.Depth != 1 || d.Width > l.Max2DSize || d.Height > l.Max2DSize {
			return bad()
		}
	case TargetCube, TargetCubeArray:
		if d.Depth != 1 || d.Width != d.Height || d.ArraySize%6 != 0 || d.Width > 1<<(l.MaxCubeLevels-1) ||
			d.Target == TargetCube && d.ArraySize != 6 {
			return bad()
		}
	case Target3D:
		max3D := uint32(1) << (l.Max3DLevels - 1)
		if d.ArraySize != 1 || d.Width > max3D || d.Height > max3D || d.Depth > max3D {
			return bad()
		}
	}
	array := d.Target == Target1DArray || d.Target == Target2DArray || d.Target == TargetCubeArray
	if !array && d.Target != TargetCube && d.ArraySize != 1 || array && d.ArraySize > l.MaxArrayLayers {
		return bad()
	}
	size := d.Width
	if d.Target != Target1D && d.Target != Target1DArray {
		size = max(size, d.Height)
	}
	if d.Target == Target3D {
		size = max(size, d.Depth)
	}
	if d.LastLevel > uint32(bits.Len32(size)-1) {
		return bad()
	}
	return nil
}

// levelExtent returns the extent of a level of d, of its texels, rows, and
// layers or slices, as boxes address them: an array's or a cube's layers
// are its z, as Gallium's boxes, and guests', address them.
func (d ResourceDesc) levelExtent(level uint32) (w, h, depth int64) {
	w, h, depth = int64(minify(d.Width, level)), 1, 1
	switch d.Target {
	case TargetBuffer:
		w = int64(d.Width)
	case Target1DArray:
		depth = int64(d.ArraySize)
	case Target3D:
		h, depth = int64(minify(d.Height, level)), int64(minify(d.Depth, level))
	case Target2DArray, TargetCube, TargetCubeArray:
		h, depth = int64(minify(d.Height, level)), int64(d.ArraySize)
	case Target2D, TargetRect:
		h = int64(minify(d.Height, level))
	}
	return w, h, depth
}

// Box is a box of a resource's level, as virgl's commands give them.
type Box struct {
	X, Y, Z              int64
	Width, Height, Depth int64
}

// spanOK reports whether the span of n from x, which a negative n flips,
// as blits' boxes may, is of [0, size).
func spanOK(x, n, size int64) bool {
	end := x + n
	if end < x {
		x, end = end, x
	}
	return x >= 0 && end <= size
}

// boxOK reports whether b is of level of d.
func (d ResourceDesc) boxOK(level uint32, b Box) bool {
	if level > d.LastLevel {
		return false
	}
	w, h, depth := d.levelExtent(level)
	return spanOK(b.X, b.Width, w) && spanOK(b.Y, b.Height, h) && spanOK(b.Z, b.Depth, depth)
}
