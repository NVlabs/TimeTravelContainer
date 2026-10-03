// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pgalloc

import (
	"reflect"

	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/sync/locking"
)

// memoryFileMutex is sync.Mutex with the correctness validator, as
// //pkg/sync/locking:generic_mutex makes it, but for Lock, which tries the
// mutex for some tens of microseconds before it blocks.
//
// A MemoryFile's mutex is taken at every allocation and free of its pages,
// and held for microseconds. A goroutine that blocks on it is woken when it
// is unlocked, and the Go runtime may wake a thread to run it. On KVM, where
// waking a thread, and parking one, is a futex() call that leaves the guest,
// a Sentry building SQLite spent a third of its time waking threads for this
// mutex alone.
type memoryFileMutex struct {
	mu sync.Mutex
}

var memoryFileprefixIndex *locking.MutexClass

// memoryFilelockNames is a list of user-friendly lock names.
var memoryFilelockNames []string

const (
	// memoryFileLockTries is how many times Lock tries the mutex before it
	// blocks.
	memoryFileLockTries = 64

	// memoryFileLockPause is how many loads Lock makes between tries: in all,
	// it tries for some tens of microseconds, less than a goroutine that
	// blocks costs on KVM.
	memoryFileLockPause = 1024
)

// memoryFileLockSpin is only loaded, by Lock between tries of the mutex.
var memoryFileLockSpin atomicbitops.Uint32

// Lock locks m.
// +checklocksignore
func (m *memoryFileMutex) Lock() {
	locking.AddGLock(memoryFileprefixIndex, -1)
	for range memoryFileLockTries {
		if m.mu.TryLock() {
			return
		}
		for range memoryFileLockPause {
			memoryFileLockSpin.Load()
		}
	}
	m.mu.Lock()
}

// Unlock unlocks m.
// +checklocksignore
func (m *memoryFileMutex) Unlock() {
	locking.DelGLock(memoryFileprefixIndex, -1)
	m.mu.Unlock()
}

func init() {
	memoryFileprefixIndex = locking.NewMutexClass(reflect.TypeFor[memoryFileMutex](), memoryFilelockNames)
}
