// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tmpfs

import (
	"os"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/sentry/fsutil"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/ttcvolume"
)

// diskMapping maps a TTC volume object, whose pages translations of files of
// the object map.
//
// A mapping is a branch's, of the branch's private object of a file, which
// writable mappings write, or every branch's, of a sealed object, which
// nothing writes: a fork seals the objects of the branches that it forks,
// which a write copies first. The branches of a family, in one Sentry, share
// the mapping of a sealed object, and with it the pages that it maps: those
// of the host's page cache, and the copies that a platform that copies file
// pages into guest memory keeps. A fork keeps its source's translations of
// the objects that it seals, and its child takes them as they are, so that
// neither faults on them again.
//
// Each mapping is of one object: a file that maps another object maps it with
// another diskMapping, and releases the old one, which is closed once nothing
// maps its pages. Translations of it remain valid until the file invalidates
// them.
type diskMapping struct {
	fsutil.MmapCachedFile

	// shared is set once the mapping is of a sealed object.
	shared atomicbitops.Bool

	// key identifies the sealed object of a shared mapping, and refs counts
	// the files that use it. Both are protected by diskMappings.mu.
	key  diskMappingKey
	refs int
}

// diskMappingKey identifies a sealed object of a volume.
type diskMappingKey struct {
	repo   *ttcvolume.Repository
	object string
}

// diskMappings are the shared mappings of sealed objects, of every volume of
// the Sentry's branches.
var diskMappings = struct {
	mu sync.Mutex
	m  map[diskMappingKey]*diskMapping
}{m: make(map[diskMappingKey]*diskMapping)}

// newDiskMapping returns a mapping of file, which it takes, the host file of
// object, of repo.
func newDiskMapping(file *os.File, repo *ttcvolume.Repository, object string) (*diskMapping, error) {
	fd, err := takeFD(file)
	if err != nil {
		return nil, err
	}
	m := &diskMapping{}
	m.SetFD(fd)
	// The Sentry may close the descriptor while it is idle, to stay within
	// the host's limit on them, and open the object again when it next maps
	// it: the object is the same while the mapping is of it, and a mapping
	// of a sealed object maps it only for reading.
	m.SetReopen(func() (int, error) {
		file, err := repo.OpenObject(object, !m.shared.Load())
		if err != nil {
			return -1, err
		}
		return takeFD(file)
	})
	return m, nil
}

// takeFD returns a descriptor of file, which it closes: file's finalizer
// would close its own.
func takeFD(file *os.File) (int, error) {
	fd, err := unix.Dup(int(file.Fd()))
	file.Close()
	return fd, err
}

// LiveForkShared implements memmap.LiveForkSharedFile.LiveForkShared.
func (m *diskMapping) LiveForkShared() bool {
	return m.shared.Load()
}

var _ memmap.LiveForkSharedFile = (*diskMapping)(nil)

// acquireSharedDiskMapping returns the shared mapping of object, a sealed
// object of repo, which file, which it takes, is open to.
func acquireSharedDiskMapping(repo *ttcvolume.Repository, object string, file *os.File) (*diskMapping, error) {
	key := diskMappingKey{repo, object}
	diskMappings.mu.Lock()
	if m := diskMappings.m[key]; m != nil {
		m.refs++
		diskMappings.mu.Unlock()
		file.Close()
		return m, nil
	}
	diskMappings.mu.Unlock()
	m, err := newDiskMapping(file, repo, object)
	if err != nil {
		return nil, err
	}
	return shareDiskMapping(repo, object, m), nil
}

// shareDiskMapping makes m, a mapping of object, a sealed object of repo,
// the mapping that files of the object share, or, if they share another
// already, releases m and returns that one. Translations of m remain valid
// either way: m maps what the shared mapping does.
func shareDiskMapping(repo *ttcvolume.Repository, object string, m *diskMapping) *diskMapping {
	m.shared.Store(true)
	key := diskMappingKey{repo, object}
	diskMappings.mu.Lock()
	if shared := diskMappings.m[key]; shared != nil {
		shared.refs++
		diskMappings.mu.Unlock()
		m.MappableRelease()
		return shared
	}
	m.key = key
	m.refs = 1
	diskMappings.m[key] = m
	diskMappings.mu.Unlock()
	return m
}

// forgetSharedDiskMapping makes files of object, of repo, that map it next
// map it anew: its host file is about to be replaced.
func forgetSharedDiskMapping(repo *ttcvolume.Repository, object string) {
	diskMappings.mu.Lock()
	delete(diskMappings.m, diskMappingKey{repo, object})
	diskMappings.mu.Unlock()
}

// release releases a file's use of m.
func (m *diskMapping) release() {
	diskMappings.mu.Lock()
	if m.key.repo == nil {
		diskMappings.mu.Unlock()
		m.MappableRelease()
		return
	}
	m.refs--
	last := m.refs == 0
	if last && diskMappings.m[m.key] == m {
		delete(diskMappings.m, m.key)
	}
	diskMappings.mu.Unlock()
	if last {
		m.MappableRelease()
	}
}
