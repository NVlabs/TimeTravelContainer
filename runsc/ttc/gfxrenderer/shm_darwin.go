// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package gfxrenderer

/*
#include <errno.h>
#include <fcntl.h>
#include <stdint.h>
#include <stdio.h>
#include <sys/mman.h>
#include <unistd.h>

// ttc_shm_create returns anonymous POSIX shared memory of size bytes, or a
// negative errno.
static int ttc_shm_create(uint64_t size, uint64_t seq) {
	char name[32];
	int fd;
	for (;;) {
		// macOS limits names to 31 characters.
		snprintf(name, sizeof(name), "/ttcgfx.%d.%llu", getpid(), (unsigned long long)seq++);
		fd = shm_open(name, O_RDWR | O_CREAT | O_EXCL, 0600);
		if (fd >= 0) {
			shm_unlink(name);
			break;
		}
		if (errno != EEXIST) {
			return -errno;
		}
	}
	if (ftruncate(fd, (off_t)size) != 0) {
		int err = errno;
		close(fd);
		return -err;
	}
	return fd;
}
*/
import "C"

import (
	"sync/atomic"

	"golang.org/x/sys/unix"
)

var shmSeq atomic.Uint64

// createSharedMemory returns anonymous shared memory of size bytes.
func createSharedMemory(size uint64) (int, error) {
	fd := int(C.ttc_shm_create(C.uint64_t(size), C.uint64_t(shmSeq.Add(1)<<16)))
	if fd < 0 {
		return -1, unix.Errno(-fd)
	}
	unix.CloseOnExec(fd)
	return fd, nil
}
