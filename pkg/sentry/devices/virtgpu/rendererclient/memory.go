// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rendererclient

import (
	"fmt"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/devices/virtgpu"
	"gvisor.dev/gvisor/pkg/sentry/fsutil"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
)

// hostMemory is shared memory of the renderer: the memory of a resource,
// which the Sentry maps once, whole, and which platforms map into the guest
// rather than copy (see memmap.HostMemoryFile).
type hostMemory struct {
	memmap.NoBufferedIOFallback
	memmap.DefaultMemoryType
	fsutil.MmapFileRefs

	// fd is the shared memory's file descriptor, and mapping is the Sentry's
	// mapping of its first size bytes.
	fd      int
	mapping []byte
	size    uint64
}

var _ virtgpu.ResourceMemory = (*hostMemory)(nil)
var _ memmap.HostMemoryFile = (*hostMemory)(nil)

// newHostMemory maps size bytes of the shared memory fd, which it takes.
func newHostMemory(fd int, size uint64) (*hostMemory, error) {
	length, ok := hostarch.PageRoundUp(size)
	if !ok || length == 0 {
		return nil, fmt.Errorf("shared memory of %d bytes", size)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, fmt.Errorf("fstat of shared memory: %w", err)
	}
	if uint64(st.Size) < length {
		return nil, fmt.Errorf("shared memory of %d bytes is shorter than its %d", st.Size, length)
	}
	mapping, err := unix.Mmap(fd, 0, int(length), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("mapping shared memory: %w", err)
	}
	m := &hostMemory{
		fd:      fd,
		mapping: mapping,
		size:    length,
	}
	m.MmapFileRefs.Closer = m
	return m, nil
}

// File implements virtgpu.ResourceMemory.File.
func (m *hostMemory) File() memmap.File {
	return m
}

// Offset implements virtgpu.ResourceMemory.Offset.
func (m *hostMemory) Offset() uint64 {
	return 0
}

// Release implements virtgpu.ResourceMemory.Release.
func (m *hostMemory) Release() {
	m.MappableRelease()
}

// Close is called once the memory is released and no page is referenced.
func (m *hostMemory) Close() error {
	err := unix.Munmap(m.mapping)
	m.mapping = nil
	if closeErr := unix.Close(m.fd); err == nil {
		err = closeErr
	}
	m.fd = -1
	return err
}

// DataFD implements memmap.File.DataFD.
func (m *hostMemory) DataFD(fr memmap.FileRange) (int, error) {
	return m.fd, nil
}

// FD implements memmap.File.FD.
func (m *hostMemory) FD() int {
	return m.fd
}

// checkRange returns an error if fr is not within the memory.
func (m *hostMemory) checkRange(fr memmap.FileRange) error {
	if fr.Start > fr.End || fr.End > m.size {
		return fmt.Errorf("range %v of %d bytes of shared memory", fr, m.size)
	}
	return nil
}
