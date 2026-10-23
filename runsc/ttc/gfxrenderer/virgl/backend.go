// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build (darwin || linux) && cgo

package virgl

// The decoder executes virgl's commands on a backend: package vkgl's
// Gallium on Vulkan, in Go (vkgl_unsafe.go). The backend's operations are
// Gallium's, of the commands' payloads, whose lengths and handles the
// decoder validated; operations that return errors refuse what they cannot
// execute, with their state unchanged.

// initBackend creates the process's backend: vkgl, Gallium's state and
// guests' shaders on Vulkan, of no Mesa.
func initBackend() (backend, error) {
	return initVkgl()
}

// backend is a renderer's device, which contexts and resources are of.
type backend interface {
	// fillCaps fills a virgl capability set of what the backend renders.
	fillCaps(set uint32, caps []byte)
	// xfb reports whether the backend emulates transform feedback, and
	// the vertex shader's first shader buffer slot of its targets.
	xfb() (emulated bool, ssbo uint32)
	// newResource returns a resource of a.
	newResource(a ResourceArgs) (backendResource, error)
	// newContext returns a Gallium context.
	newContext() (backendContext, error)
	// formatIntClass returns 1 if virgl's format is of signed integers, 2
	// if of unsigned, and 0 otherwise.
	formatIntClass(format uint32) uint32
	// parseShader parses a guest's TGSI, of numTokens tokens as the guest
	// counted them.
	parseShader(text string, numTokens uint32) (shaderCode, error)
}

// backendResource is a backend's resource.
type backendResource interface {
	unref()
	target() uint32
	width(level uint32) uint32
	height(level uint32) uint32
	// checkSurface returns nil if a surface of the resource, of format and
	// of a virgl surface's val0 and val1, is valid.
	checkSurface(format, val0, val1 uint32) error
}

// shaderCode is a shader's parsed TGSI, of which a clone creates the
// shader again, of a copy of its own.
type shaderCode interface {
	dup() shaderCode
	free()
}

// backendFence is what a context executed until a flush.
type backendFence interface {
	wait()
}

// surface is a framebuffer's surface: a level, or layers, of a resource,
// as a format.
type surface struct {
	res                backendResource
	format, val0, val1 uint32
	nrSamples          uint32
}

// drawArgs are a draw's buffers: its index buffer, and its indirect
// buffers.
type drawArgs struct {
	index                   backendResource
	indexSize, indexOffset  uint32
	indirect, indirectCount backendResource
}

// backendContext is a backend's Gallium context, of a sub-context. Its
// objects are its own, which deleteObject deletes.
type backendContext interface {
	// transfer copies a box between r and the guest's memory of it, data.
	transfer(r backendResource, a TransferArgs, data []byte) error
	// copyResource copies every level and layer of src to dst, a resource
	// of src's parameters.
	copyResource(dst, src backendResource)
	// unbind unbinds the context's state, as Gallium's frontends do before
	// they destroy a context and its objects.
	unbind()
	destroy()
	// flush returns a fence of what the context executed, or nil.
	flush() backendFence
	// flushAsync has the GPU execute what the context executed.
	flushAsync()

	createBlend(d []uint32) any
	createRasterizer(d []uint32) any
	createDSA(d []uint32) any
	createSamplerState(d []uint32) any
	// createVertexElements returns vertex elements, 4 dwords each, of the
	// bound vertex buffers' strides.
	createVertexElements(elements []uint32, strides []uint32) any
	// createShader returns a shader of code, of its stream output so, the
	// payload's dwords from its count, whose inputs of sintMask and
	// uintMask are signed and unsigned integers, which captures its stream
	// output if xfb.
	createShader(stage uint32, code shaderCode, so []uint32, sharedMem, sintMask, uintMask uint32, xfb bool) (any, error)
	createSamplerView(r backendResource, format, target, val0, val1, swizzle uint32) any
	createQuery(typ, index uint32) any
	deleteObject(typ, stage uint32, obj any)
	bindObject(typ uint32, obj any)
	bindShader(stage uint32, shader any)

	// setXfb sets the parameters of the next draw's capture, and its
	// targets, or nil.
	setXfb(params []uint32, targets []backendResource)
	// bufferWrite writes data to the buffer r at offset, which the caller
	// checked.
	bufferWrite(r backendResource, offset uint32, data []byte)
	setFramebuffer(cbufs []surface, zsbuf surface) error
	setFramebufferNoAttach(width, height, layers, samples uint32) error
	setViewports(start uint32, d []uint32)
	setScissors(start uint32, d []uint32)
	setVertexBuffers(res []backendResource, offsets []uint32)
	setConstants(stage uint32, d []uint32)
	setUniformBuffer(stage, index uint32, r backendResource, offset, size uint32) error
	setSamplerViews(stage, start uint32, views []any)
	bindSamplers(stage, start uint32, samplers []any)
	setStencilRef(v uint32)
	setBlendColor(d []uint32)
	setPolygonStipple(d []uint32)
	setClipState(d []uint32)
	setSampleMask(mask uint32)
	setMinSamples(n uint32)
	setTessState(d []uint32)
	setShaderBuffers(stage, start uint32, res []backendResource, offsets, sizes []uint32) error
	setAtomicBuffers(start uint32, res []backendResource, offsets, sizes []uint32) error
	setShaderImages(stage, start uint32, res []backendResource, d []uint32) error
	renderCondition(q any, condition bool, mode uint32)

	clear(d []uint32)
	clearTexture(r backendResource, d []uint32) error
	clearSurface(s surface, d []uint32) error
	draw(d []uint32, a drawArgs) error
	blit(dst, src backendResource, d []uint32) error
	copyRegion(dst, src backendResource, d []uint32) error
	inlineWrite(r backendResource, d []uint32, data []byte) error
	launchGrid(d []uint32, indirect backendResource) error
	memoryBarrier(flags uint32)
	textureBarrier(flags uint32)
	beginQuery(q any) bool
	endQuery(q any) bool
	// queryResult writes q's result, a struct virgl_host_query_state, to
	// state if it is ready, or once it is if wait, and reports whether it
	// did.
	queryResult(q any, typ uint32, wait bool, state []byte) bool
	queryResultQBO(q any, qbo backendResource, wait bool, resultType, offset uint32, index int32) error
}
