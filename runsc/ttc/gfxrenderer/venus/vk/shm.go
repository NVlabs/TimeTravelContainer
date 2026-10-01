// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package vk

/*
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/mman.h>
#include <unistd.h>

// vk_shm_create returns anonymous shared memory of size bytes, or a negative
// errno. On macOS, POSIX shared memory's names are global to the user, so
// the memory gets one that is unique and unguessable, exclusively, and loses
// it at once: no other process can open it.
static int vk_shm_create(uint64_t size) {
#ifdef __APPLE__
	char name[32];
	int fd = -1;
	for (int attempt = 0; attempt < 16 && fd < 0; attempt++) {
		// macOS limits names to 31 characters.
		snprintf(name, sizeof(name), "/ttcgfx.%08x%08x", arc4random(), arc4random());
		fd = shm_open(name, O_RDWR | O_CREAT | O_EXCL, 0600);
		if (fd >= 0) {
			shm_unlink(name);
		} else if (errno != EEXIST) {
			return -errno;
		}
	}
	if (fd < 0) {
		return -EEXIST;
	}
#else
	int fd = memfd_create("ttc-venus", MFD_CLOEXEC);
	if (fd < 0) {
		return -errno;
	}
#endif
	if (ftruncate(fd, (off_t)size) != 0) {
		int err = errno;
		close(fd);
		return -err;
	}
	return fd;
}
*/
import "C"

import "golang.org/x/sys/unix"

// CreateSharedMemory returns anonymous shared memory of size bytes, which
// the Sentry can map into the guest.
func CreateSharedMemory(size uint64) (int, error) {
	fd := int(C.vk_shm_create(C.uint64_t(size)))
	if fd < 0 {
		return -1, unix.Errno(-fd)
	}
	unix.CloseOnExec(fd)
	return fd, nil
}
