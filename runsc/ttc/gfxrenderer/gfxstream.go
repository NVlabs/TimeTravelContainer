// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package gfxrenderer

/*
#cgo linux LDFLAGS: -ldl

#include <dlfcn.h>
#include <errno.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/uio.h>
#include <unistd.h>

// The parts of gfxstream's host API, virtio-gpu-gfxstream-renderer.h and
// virtio-gpu-gfxstream-renderer-unstable.h, that the renderer uses.

struct stream_renderer_box {
	uint32_t x, y, z;
	uint32_t w, h, d;
};

struct stream_renderer_resource_create_args {
	uint32_t handle;
	uint32_t target;
	uint32_t format;
	uint32_t bind;
	uint32_t width;
	uint32_t height;
	uint32_t depth;
	uint32_t array_size;
	uint32_t last_level;
	uint32_t nr_samples;
	uint32_t flags;
};

struct stream_renderer_fence {
	uint32_t flags;
	uint64_t fence_id;
	uint32_t ctx_id;
	uint8_t ring_idx;
};

struct stream_renderer_handle {
	int64_t os_handle;
	uint32_t handle_type;
};

struct stream_renderer_debug {
	uint32_t debug_type;
	const char* message;
};

struct stream_renderer_param {
	uint64_t key;
	uint64_t value;
};

struct stream_renderer_command {
	uint32_t ctx_id;
	uint32_t cmd_size;
	uint8_t* cmd;
	uint32_t num_in_fences;
	struct stream_renderer_handle* fences;
};

struct stream_renderer_create_blob {
	uint32_t blob_mem;
	uint32_t blob_flags;
	uint64_t blob_id;
	uint64_t size;
};

#define STREAM_RENDERER_PARAM_USER_DATA 1
#define STREAM_RENDERER_PARAM_RENDERER_FLAGS 2
#define STREAM_RENDERER_PARAM_FENCE_CALLBACK 3
#define STREAM_RENDERER_PARAM_DEBUG_CALLBACK 6
#define STREAM_RENDERER_PARAM_RENDERER_FEATURES 11

#define STREAM_RENDERER_FLAG_FENCE (1 << 0)
#define STREAM_RENDERER_FLAG_FENCE_RING_IDX (1 << 1)

#define STREAM_HANDLE_TYPE_MEM_OPAQUE_FD 0x1
#define STREAM_HANDLE_TYPE_MEM_DMABUF 0x2
#define STREAM_HANDLE_TYPE_MEM_SHM 0x4

struct ttc_gfxstream {
	void* lib;
	int (*init)(struct stream_renderer_param*, uint64_t);
	void (*teardown)(void);
	int (*resource_create)(struct stream_renderer_resource_create_args*, struct iovec*, uint32_t);
	void (*resource_unref)(uint32_t);
	void (*context_destroy)(uint32_t);
	int (*submit_cmd)(struct stream_renderer_command*);
	int (*transfer_read_iov)(uint32_t, uint32_t, uint32_t, uint32_t, uint32_t,
	                         struct stream_renderer_box*, uint64_t, struct iovec*, int);
	int (*transfer_write_iov)(uint32_t, uint32_t, int, uint32_t, uint32_t,
	                          struct stream_renderer_box*, uint64_t, struct iovec*, unsigned int);
	void (*get_cap_set)(uint32_t, uint32_t*, uint32_t*);
	void (*fill_caps)(uint32_t, uint32_t, void*);
	int (*resource_attach_iov)(int, struct iovec*, int);
	void (*ctx_attach_resource)(int, int);
	void (*ctx_detach_resource)(int, int);
	int (*create_blob)(uint32_t, uint32_t, const struct stream_renderer_create_blob*,
	                   const struct iovec*, uint32_t, const struct stream_renderer_handle*);
	int (*export_blob)(uint32_t, struct stream_renderer_handle*);
	int (*context_create)(uint32_t, uint32_t, const char*, uint32_t);
	int (*create_fence)(const struct stream_renderer_fence*);
	int (*resource_map_info)(uint32_t, uint32_t*);
};

static struct ttc_gfxstream ttc_g;

// ttc_fence_fd is the pipe to which the fence callback writes the fences that
// gfxstream signals, from its own threads.
static int ttc_fence_fd = -1;

struct ttc_fence_record {
	uint64_t fence_id;
	uint32_t ctx_id;
	uint32_t ring_idx;
	uint32_t flags;
	uint32_t pad;
};

static void ttc_fence_callback(void* cookie, struct stream_renderer_fence* fence) {
	struct ttc_fence_record rec = {
		.fence_id = fence->fence_id,
		.ctx_id = fence->ctx_id,
		.ring_idx = fence->ring_idx,
		.flags = fence->flags,
	};
	// Writes to a pipe of less than PIPE_BUF bytes are atomic.
	for (;;) {
		ssize_t n = write(ttc_fence_fd, &rec, sizeof(rec));
		if (n >= 0 || errno != EINTR) {
			return;
		}
	}
}

static void ttc_debug_callback(void* cookie, struct stream_renderer_debug* debug) {
	fprintf(stderr, "gfxstream: %s\n", debug->message);
}

// ttc_gfxstream_open loads gfxstream from path, and returns NULL or an error.
static const char* ttc_gfxstream_open(const char* path) {
	void* lib = dlopen(path, RTLD_NOW | RTLD_LOCAL);
	if (lib == NULL) {
		return dlerror();
	}
#define TTC_SYM(field, name)                         \
	do {                                             \
		*(void**)(&ttc_g.field) = dlsym(lib, name);  \
		if (ttc_g.field == NULL) {                   \
			return "missing " name;                  \
		}                                            \
	} while (0)
	TTC_SYM(init, "stream_renderer_init");
	TTC_SYM(teardown, "stream_renderer_teardown");
	TTC_SYM(resource_create, "stream_renderer_resource_create");
	TTC_SYM(resource_unref, "stream_renderer_resource_unref");
	TTC_SYM(context_destroy, "stream_renderer_context_destroy");
	TTC_SYM(submit_cmd, "stream_renderer_submit_cmd");
	TTC_SYM(transfer_read_iov, "stream_renderer_transfer_read_iov");
	TTC_SYM(transfer_write_iov, "stream_renderer_transfer_write_iov");
	TTC_SYM(get_cap_set, "stream_renderer_get_cap_set");
	TTC_SYM(fill_caps, "stream_renderer_fill_caps");
	TTC_SYM(resource_attach_iov, "stream_renderer_resource_attach_iov");
	TTC_SYM(ctx_attach_resource, "stream_renderer_ctx_attach_resource");
	TTC_SYM(ctx_detach_resource, "stream_renderer_ctx_detach_resource");
	TTC_SYM(create_blob, "stream_renderer_create_blob");
	TTC_SYM(export_blob, "stream_renderer_export_blob");
	TTC_SYM(context_create, "stream_renderer_context_create");
	TTC_SYM(create_fence, "stream_renderer_create_fence");
	TTC_SYM(resource_map_info, "stream_renderer_resource_map_info");
#undef TTC_SYM
	ttc_g.lib = lib;
	return NULL;
}

static int ttc_gfxstream_init(uint64_t flags, const char* features, int fence_fd) {
	ttc_fence_fd = fence_fd;
	struct stream_renderer_param params[5] = {
		{STREAM_RENDERER_PARAM_USER_DATA, 0},
		{STREAM_RENDERER_PARAM_RENDERER_FLAGS, flags},
		{STREAM_RENDERER_PARAM_FENCE_CALLBACK, (uint64_t)(uintptr_t)&ttc_fence_callback},
		{STREAM_RENDERER_PARAM_DEBUG_CALLBACK, (uint64_t)(uintptr_t)&ttc_debug_callback},
		{STREAM_RENDERER_PARAM_RENDERER_FEATURES, (uint64_t)(uintptr_t)features},
	};
	return ttc_g.init(params, features[0] != '\0' ? 5 : 4);
}

static void ttc_gfxstream_teardown(void) { ttc_g.teardown(); }

static void ttc_get_cap_set(uint32_t set, uint32_t* max_ver, uint32_t* max_size) {
	ttc_g.get_cap_set(set, max_ver, max_size);
}

static void ttc_fill_caps(uint32_t set, uint32_t version, void* caps) {
	ttc_g.fill_caps(set, version, caps);
}

static int ttc_context_create(uint32_t ctx_id, uint32_t nlen, const char* name, uint32_t capset_id) {
	return ttc_g.context_create(ctx_id, nlen, name, capset_id);
}

static void ttc_context_destroy(uint32_t ctx_id) { ttc_g.context_destroy(ctx_id); }

static int ttc_resource_create(struct stream_renderer_resource_create_args* args, void* backing,
                               uint64_t size) {
	struct iovec iov = {.iov_base = backing, .iov_len = size};
	int ret = ttc_g.resource_create(args, &iov, 1);
	if (ret != 0) {
		return ret;
	}
	return ttc_g.resource_attach_iov(args->handle, &iov, 1);
}

static int ttc_create_blob(uint32_t ctx_id, uint32_t res_id, uint32_t mem, uint32_t flags,
                           uint64_t blob_id, uint64_t size) {
	struct stream_renderer_create_blob blob = {
		.blob_mem = mem,
		.blob_flags = flags,
		.blob_id = blob_id,
		.size = size,
	};
	return ttc_g.create_blob(ctx_id, res_id, &blob, NULL, 0, NULL);
}

static int ttc_export_blob(uint32_t res_id, int64_t* fd, uint32_t* type) {
	struct stream_renderer_handle handle = {.os_handle = -1};
	int ret = ttc_g.export_blob(res_id, &handle);
	*fd = handle.os_handle;
	*type = handle.handle_type;
	return ret;
}

static int ttc_resource_map_info(uint32_t res_id, uint32_t* map_info) {
	return ttc_g.resource_map_info(res_id, map_info);
}

static void ttc_ctx_attach_resource(uint32_t ctx_id, uint32_t res_id) {
	ttc_g.ctx_attach_resource(ctx_id, res_id);
}

static void ttc_ctx_detach_resource(uint32_t ctx_id, uint32_t res_id) {
	ttc_g.ctx_detach_resource(ctx_id, res_id);
}

static void ttc_resource_unref(uint32_t res_id) { ttc_g.resource_unref(res_id); }

static int ttc_submit_cmd(uint32_t ctx_id, void* cmd, uint32_t size) {
	struct stream_renderer_command c = {
		.ctx_id = ctx_id,
		.cmd_size = size,
		.cmd = (uint8_t*)cmd,
	};
	return ttc_g.submit_cmd(&c);
}

static int ttc_transfer(uint32_t res_id, uint32_t ctx_id, int to_host, uint32_t level,
                        uint32_t stride, uint32_t layer_stride, uint32_t x, uint32_t y,
                        uint32_t z, uint32_t w, uint32_t h, uint32_t d, uint64_t offset) {
	struct stream_renderer_box box = {x, y, z, w, h, d};
	if (to_host) {
		return ttc_g.transfer_write_iov(res_id, ctx_id, (int)level, stride, layer_stride, &box,
		                                offset, NULL, 0);
	}
	return ttc_g.transfer_read_iov(res_id, ctx_id, level, stride, layer_stride, &box, offset,
	                               NULL, 0);
}

static int ttc_create_fence(uint64_t fence_id, uint32_t ctx_id, uint32_t ring, int has_ring) {
	struct stream_renderer_fence fence = {
		.flags = STREAM_RENDERER_FLAG_FENCE | (has_ring ? STREAM_RENDERER_FLAG_FENCE_RING_IDX : 0),
		.fence_id = fence_id,
		.ctx_id = ctx_id,
		.ring_idx = (uint8_t)ring,
	};
	return ttc_g.create_fence(&fence);
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// Renderer flags: STREAM_RENDERER_FLAGS_*.
const (
	flagUseSurfaceless = 1 << 3
	flagUseVK          = 1 << 5
	flagExternalBlob   = 1 << 6
	flagSystemBlob     = 1 << 7
)

// Blob handle types: STREAM_HANDLE_TYPE_MEM_*.
const (
	handleTypeOpaqueFD = 0x1
	handleTypeDMABuf   = 0x2
	handleTypeSHM      = 0x4
)

// fenceRecordSize is the size of struct ttc_fence_record.
const fenceRecordSize = 24

// gfxstream is gfxstream's renderer, loaded into the process. Its methods
// must be called from one goroutine at a time.
type gfxstream struct{}

// openGfxstream loads gfxstream's host library.
func openGfxstream(path string) (*gfxstream, error) {
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	if msg := C.ttc_gfxstream_open(cpath); msg != nil {
		return nil, fmt.Errorf("loading %s: %s", path, C.GoString(msg))
	}
	return &gfxstream{}, nil
}

// init starts the renderer: Vulkan only, without a window, with every blob in
// shared memory that it can export. It writes the fences that it signals to
// fenceFD.
func (g *gfxstream) init(features string, fenceFD int) error {
	cfeatures := C.CString(features)
	// gfxstream keeps a pointer to the features.
	flags := C.uint64_t(flagUseVK | flagUseSurfaceless | flagExternalBlob | flagSystemBlob)
	if ret := C.ttc_gfxstream_init(flags, cfeatures, C.int(fenceFD)); ret != 0 {
		C.free(unsafe.Pointer(cfeatures))
		return fmt.Errorf("stream_renderer_init: %d", int(ret))
	}
	return nil
}

func (g *gfxstream) teardown() {
	C.ttc_gfxstream_teardown()
}

// capset returns a capability set's latest version and contents.
func (g *gfxstream) capset(id uint32) (uint32, []byte) {
	var maxVer, maxSize C.uint32_t
	C.ttc_get_cap_set(C.uint32_t(id), &maxVer, &maxSize)
	if maxSize == 0 {
		return 0, nil
	}
	buf := C.calloc(1, C.size_t(maxSize))
	defer C.free(buf)
	C.ttc_fill_caps(C.uint32_t(id), maxVer, buf)
	return uint32(maxVer), C.GoBytes(buf, C.int(maxSize))
}

func (g *gfxstream) contextCreate(ctxID, capsetID uint32, name string) int {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	return int(C.ttc_context_create(C.uint32_t(ctxID), C.uint32_t(len(name)), cname, C.uint32_t(capsetID)))
}

func (g *gfxstream) contextDestroy(ctxID uint32) {
	C.ttc_context_destroy(C.uint32_t(ctxID))
}

// resourceCreate creates a classic resource whose backing is the size bytes
// at backing, which must stay mapped while the resource exists.
func (g *gfxstream) resourceCreate(resID uint32, target, format, bind, width, height, depth, arraySize, lastLevel, nrSamples, flags uint32, backing unsafe.Pointer, size uint64) int {
	args := C.struct_stream_renderer_resource_create_args{
		handle:     C.uint32_t(resID),
		target:     C.uint32_t(target),
		format:     C.uint32_t(format),
		bind:       C.uint32_t(bind),
		width:      C.uint32_t(width),
		height:     C.uint32_t(height),
		depth:      C.uint32_t(depth),
		array_size: C.uint32_t(arraySize),
		last_level: C.uint32_t(lastLevel),
		nr_samples: C.uint32_t(nrSamples),
		flags:      C.uint32_t(flags),
	}
	return int(C.ttc_resource_create(&args, backing, C.uint64_t(size)))
}

func (g *gfxstream) createBlob(ctxID, resID, mem, flags uint32, blobID, size uint64) int {
	return int(C.ttc_create_blob(C.uint32_t(ctxID), C.uint32_t(resID), C.uint32_t(mem), C.uint32_t(flags), C.uint64_t(blobID), C.uint64_t(size)))
}

// exportBlob returns the descriptor of a blob's memory, which the caller
// owns, and its type. gfxstream gives it up once.
func (g *gfxstream) exportBlob(resID uint32) (int, uint32, int) {
	var fd C.int64_t
	var typ C.uint32_t
	ret := C.ttc_export_blob(C.uint32_t(resID), &fd, &typ)
	return int(fd), uint32(typ), int(ret)
}

func (g *gfxstream) mapInfo(resID uint32) uint32 {
	var info C.uint32_t
	if C.ttc_resource_map_info(C.uint32_t(resID), &info) != 0 {
		return 0
	}
	return uint32(info)
}

func (g *gfxstream) attachResource(ctxID, resID uint32) {
	C.ttc_ctx_attach_resource(C.uint32_t(ctxID), C.uint32_t(resID))
}

func (g *gfxstream) detachResource(ctxID, resID uint32) {
	C.ttc_ctx_detach_resource(C.uint32_t(ctxID), C.uint32_t(resID))
}

func (g *gfxstream) resourceUnref(resID uint32) {
	C.ttc_resource_unref(C.uint32_t(resID))
}

func (g *gfxstream) submitCommand(ctxID uint32, cmd []byte) int {
	if len(cmd) == 0 {
		return int(C.ttc_submit_cmd(C.uint32_t(ctxID), nil, 0))
	}
	// gfxstream does not keep the command.
	ccmd := C.CBytes(cmd)
	defer C.free(ccmd)
	return int(C.ttc_submit_cmd(C.uint32_t(ctxID), ccmd, C.uint32_t(len(cmd))))
}

func (g *gfxstream) transfer(resID, ctxID uint32, toHost bool, level, stride, layerStride, x, y, z, w, h, d uint32, offset uint64) int {
	cToHost := C.int(0)
	if toHost {
		cToHost = 1
	}
	return int(C.ttc_transfer(C.uint32_t(resID), C.uint32_t(ctxID), cToHost, C.uint32_t(level), C.uint32_t(stride), C.uint32_t(layerStride),
		C.uint32_t(x), C.uint32_t(y), C.uint32_t(z), C.uint32_t(w), C.uint32_t(h), C.uint32_t(d), C.uint64_t(offset)))
}

func (g *gfxstream) createFence(fenceID uint64, ctxID, ring uint32, hasRing bool) int {
	cHasRing := C.int(0)
	if hasRing {
		cHasRing = 1
	}
	return int(C.ttc_create_fence(C.uint64_t(fenceID), C.uint32_t(ctxID), C.uint32_t(ring), cHasRing))
}
