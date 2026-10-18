// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kernel

import (
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/mm"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
)

// Folds of a Kernel's address spaces; see mm.MemoryManager.FoldStep.
//
// A started Kernel whose application MemoryFile has home chunks folds its
// address spaces when asked: when an address space on the file releases its
// pages, which others may then hold alone, and when a capture's references on
// the file's pages go. A Kernel's folds wait while something reads where its
// address spaces' pages are, as a capture, fork or save does
// (quiescePausedAnd).

const (
	// foldStepBytes is the most a fold step moves, holding an address
	// space's activeMu.
	foldStepBytes = hostarch.HugePageSize

	// foldRate is how many bytes per second a Kernel's folds move at most,
	// copying them.
	foldRate = 1 << 30

	// foldDelay is how long a Kernel waits, once asked to fold, before it
	// does: a burst of releases, as of a branch's processes, makes one pass.
	foldDelay = 50 * time.Millisecond

	// collapseRetry is how long a Kernel waits, after a pass that left
	// candidates for collapse waiting for discarded pages to be written again,
	// before it looks at them again, as Linux's khugepaged sleeps between
	// scans.
	collapseRetry = 10 * time.Second
)

// kernelFolds is where a Kernel's folds are.
type kernelFolds struct {
	mu sync.Mutex

	// cond is broadcast when holds or stepping changes.
	cond *sync.Cond

	// holds is how many holdFolds are in effect; no fold step starts while
	// it is not 0.
	//
	// +checklocks:mu
	holds int

	// stepping is true while a fold step runs.
	//
	// +checklocks:mu
	stepping bool

	// pending is true if a fold pass is asked for and not yet started.
	//
	// +checklocks:mu
	pending bool

	// running is true while the Kernel's fold goroutine runs.
	//
	// +checklocks:mu
	running bool

	// stopped is true once the Kernel stopped folding.
	//
	// +checklocks:mu
	stopped bool

	// retrying is true while a pass is set to start after collapseRetry.
	//
	// +checklocks:mu
	retrying bool
}

// foldKernels holds the started Kernels that fold, by application MemoryFile.
var foldKernels struct {
	mu sync.Mutex

	// +checklocks:mu
	kernels map[*pgalloc.MemoryFile]map[*Kernel]struct{}
}

func init() {
	mm.FoldRequestHook = requestFolds
}

// requestFolds asks each started Kernel whose application MemoryFile is mf to
// fold its address spaces.
func requestFolds(mf *pgalloc.MemoryFile) {
	foldKernels.mu.Lock()
	kernels := make([]*Kernel, 0, len(foldKernels.kernels[mf]))
	for k := range foldKernels.kernels[mf] {
		kernels = append(kernels, k)
	}
	foldKernels.mu.Unlock()
	for _, k := range kernels {
		k.requestFolds()
	}
}

// startFolds has k fold its address spaces from now on, if its MemoryFile has
// home chunks, and once now: a Kernel restored or forked may have pages to
// fold, or homes to adopt, at once.
//
// Preconditions: k is starting.
func (k *Kernel) startFolds() {
	if k.mf == nil || !k.mf.HomesEnabled() {
		return
	}
	k.folds.mu.Lock()
	if k.folds.cond == nil {
		k.folds.cond = sync.NewCond(&k.folds.mu)
	}
	k.folds.stopped = false
	k.folds.mu.Unlock()
	foldKernels.mu.Lock()
	if foldKernels.kernels == nil {
		foldKernels.kernels = make(map[*pgalloc.MemoryFile]map[*Kernel]struct{})
	}
	kernels := foldKernels.kernels[k.mf]
	if kernels == nil {
		kernels = make(map[*Kernel]struct{})
		foldKernels.kernels[k.mf] = kernels
		mf := k.mf
		mf.SetFoldNotify(func() { requestFolds(mf) })
	}
	kernels[k] = struct{}{}
	foldKernels.mu.Unlock()
	k.requestFolds()
}

// stopFolds undoes startFolds.
func (k *Kernel) stopFolds() {
	foldKernels.mu.Lock()
	if kernels := foldKernels.kernels[k.mf]; kernels != nil {
		delete(kernels, k)
		if len(kernels) == 0 {
			delete(foldKernels.kernels, k.mf)
			k.mf.SetFoldNotify(nil)
		}
	}
	foldKernels.mu.Unlock()
	k.folds.mu.Lock()
	k.folds.stopped = true
	if k.folds.cond != nil {
		k.folds.cond.Broadcast()
	}
	k.folds.mu.Unlock()
}

// holdFolds keeps k's folds from moving pages until the returned function is
// called, once any fold step under way has ended. Holds may nest.
func (k *Kernel) holdFolds() func() {
	k.folds.mu.Lock()
	if k.folds.cond == nil {
		k.folds.cond = sync.NewCond(&k.folds.mu)
	}
	k.folds.holds++
	for k.folds.stepping {
		k.folds.cond.Wait()
	}
	k.folds.mu.Unlock()
	return func() {
		k.folds.mu.Lock()
		k.folds.holds--
		k.folds.cond.Broadcast()
		k.folds.mu.Unlock()
	}
}

// requestFolds asks k to fold its address spaces.
func (k *Kernel) requestFolds() {
	k.folds.mu.Lock()
	defer k.folds.mu.Unlock()
	if k.folds.stopped || k.folds.cond == nil {
		return
	}
	k.folds.pending = true
	if !k.folds.running {
		k.folds.running = true
		go k.foldLoop() // S/R-SAFE: holdFolds keeps it from moving pages while k is saved.
	}
}

// foldLoop folds k's address spaces while asked to.
func (k *Kernel) foldLoop() {
	time.Sleep(foldDelay)
	for {
		k.folds.mu.Lock()
		if !k.folds.pending || k.folds.stopped {
			k.folds.running = false
			k.folds.mu.Unlock()
			return
		}
		k.folds.pending = false
		k.folds.mu.Unlock()
		start := time.Now()
		var moved uint64
		waiting := false
		for _, m := range k.memoryManagers() {
			for {
				n, done, ok := k.foldStep(m)
				if !ok {
					break
				}
				moved += n
				// Keep to foldRate.
				if ahead := time.Duration(float64(moved)/foldRate*float64(time.Second)) - time.Since(start); ahead > 0 {
					time.Sleep(ahead)
				}
				if done {
					break
				}
			}
			waiting = waiting || m.CollapseWaiting()
		}
		if waiting {
			k.retryFolds()
		}
	}
}

// retryFolds asks k to fold again after collapseRetry.
func (k *Kernel) retryFolds() {
	k.folds.mu.Lock()
	defer k.folds.mu.Unlock()
	if k.folds.retrying || k.folds.stopped {
		return
	}
	k.folds.retrying = true
	time.AfterFunc(collapseRetry, func() {
		k.folds.mu.Lock()
		k.folds.retrying = false
		k.folds.mu.Unlock()
		k.requestFolds()
	})
}

// foldStep runs one fold step of m, once no holdFolds is in effect. ok is
// false if k stopped folding.
func (k *Kernel) foldStep(m *mm.MemoryManager) (moved uint64, done, ok bool) {
	k.folds.mu.Lock()
	for k.folds.holds != 0 && !k.folds.stopped {
		k.folds.cond.Wait()
	}
	if k.folds.stopped {
		k.folds.mu.Unlock()
		return 0, true, false
	}
	k.folds.stepping = true
	k.folds.mu.Unlock()
	moved, done = m.FoldStep(foldStepBytes)
	k.folds.mu.Lock()
	k.folds.stepping = false
	k.folds.cond.Broadcast()
	k.folds.mu.Unlock()
	return moved, done, true
}

// memoryManagers returns the address spaces of k's tasks.
func (k *Kernel) memoryManagers() []*mm.MemoryManager {
	seen := make(map[*mm.MemoryManager]struct{})
	var mms []*mm.MemoryManager
	k.tasks.mu.RLock()
	for t := range k.tasks.Root.tids {
		t.mu.Lock()
		m := t.image.MemoryManager
		t.mu.Unlock()
		if m == nil {
			continue
		}
		if _, ok := seen[m]; !ok {
			seen[m] = struct{}{}
			mms = append(mms, m)
		}
	}
	k.tasks.mu.RUnlock()
	return mms
}
