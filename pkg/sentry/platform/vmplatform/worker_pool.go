// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package vmplatform

import (
	"context"
	"fmt"
	"runtime"
	"sync"
)

type workerResult struct {
	value any
	err   error
}

type workerInitialized struct {
	worker *vcpuWorker
	err    error
}

type workerJob struct {
	ctx  context.Context
	run  func(context.Context, VCPU) (any, error)
	done chan workerResult

	mu       sync.Mutex
	assigned *vcpuWorker
}

func (j *workerJob) assign(w *vcpuWorker) {
	j.mu.Lock()
	j.assigned = w
	j.mu.Unlock()
}

func (j *workerJob) unassign(w *vcpuWorker) {
	j.mu.Lock()
	if j.assigned == w {
		j.assigned = nil
	}
	j.mu.Unlock()
}

func (j *workerJob) interrupt() {
	j.mu.Lock()
	w := j.assigned
	j.mu.Unlock()
	if w != nil {
		_ = w.vcpu.RequestExit()
	}
}

type vcpuWorker struct {
	id   int
	pool *WorkerPool
	vcpu VCPU

	mu      sync.Mutex
	running *workerJob
}

func (w *vcpuWorker) setRunning(job *workerJob) {
	w.mu.Lock()
	w.running = job
	w.mu.Unlock()
}

func (w *vcpuWorker) runningJob() *workerJob {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.running
}

// WorkerPool is a fixed set of thread-affine monitor vCPU workers. Application
// execution belongs to the host VMEngine because a sentry-at-EL1 engine must
// preserve the calling Sentry thread's continuation.
type WorkerPool struct {
	vm     VM
	idBase int

	jobs chan *workerJob
	stop chan struct{}

	mu      sync.Mutex
	workers []*vcpuWorker
	closed  bool

	wg sync.WaitGroup
}

// NewWorkerPool creates count vCPUs on count permanently locked OS threads.
func NewWorkerPool(vm VM, count int) (*WorkerPool, error) {
	return NewWorkerPoolWithIDBase(vm, count, 0)
}

// NewWorkerPoolWithIDBase creates count thread-affine workers whose VMM IDs
// start at idBase. Execution engines use this to reserve disjoint monitor and
// application vCPU state slots without changing the portable worker policy.
func NewWorkerPoolWithIDBase(vm VM, count, idBase int) (*WorkerPool, error) {
	if vm == nil {
		return nil, fmt.Errorf("nil VM")
	}
	if count <= 0 {
		return nil, fmt.Errorf("invalid vCPU worker count %d", count)
	}
	if idBase < 0 || idBase > int(^uint(0)>>1)-count {
		return nil, fmt.Errorf("invalid vCPU worker ID base %d for count %d", idBase, count)
	}
	p := &WorkerPool{
		vm:     vm,
		idBase: idBase,
		jobs:   make(chan *workerJob),
		stop:   make(chan struct{}),
	}
	ready := make(chan workerInitialized, count)
	for i := 0; i < count; i++ {
		p.wg.Add(1)
		go p.workerMain(idBase+i, ready)
	}
	for i := 0; i < count; i++ {
		result := <-ready
		if result.err != nil {
			p.closeQueues()
			p.wg.Wait()
			return nil, fmt.Errorf("creating vCPU worker %d: %w", i, result.err)
		}
		p.workers = append(p.workers, result.worker)
	}
	return p, nil
}

func (p *WorkerPool) workerMain(id int, ready chan<- workerInitialized) {
	// This goroutine owns its VCPU for its entire lifetime.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer p.wg.Done()
	vcpu, err := p.vm.CreateVCPU(id)
	if err != nil {
		ready <- workerInitialized{err: err}
		return
	}
	w := &vcpuWorker{id: id, pool: p, vcpu: vcpu}
	ready <- workerInitialized{worker: w}
	defer vcpu.Close()

	for {
		select {
		case job := <-p.jobs:
			if job == nil {
				continue
			}
			w.setRunning(job)
			job.assign(w)
			value, err := job.run(job.ctx, vcpu)
			job.unassign(w)
			w.setRunning(nil)
			select {
			case job.done <- workerResult{value: value, err: err}:
			default:
			}
		case <-p.stop:
			return
		}
	}
}

// Do runs fn on a thread-affine monitor vCPU worker.
func (p *WorkerPool) Do(ctx context.Context, fn func(context.Context, VCPU) (any, error)) (any, error) {
	if fn == nil {
		return nil, fmt.Errorf("nil vCPU job")
	}
	job := &workerJob{ctx: ctx, run: fn, done: make(chan workerResult, 1)}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, fmt.Errorf("vCPU worker pool is closed")
	}
	p.mu.Unlock()

	select {
	case p.jobs <- job:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.stop:
		return nil, fmt.Errorf("vCPU worker pool is closed")
	}
	// A worker is running the job, whose effects may already be visible:
	// monitor transactions, for example, ignore interruption and always
	// complete. If the caller gives up or the pool stops, interrupt the job,
	// but wait for it and report what it actually did either way, so that
	// callers never mistake a completed job for a failed one, or overlap it
	// with the next.
	select {
	case result := <-job.done:
		return result.value, result.err
	case <-ctx.Done():
	case <-p.stop:
	}
	job.interrupt()
	result := <-job.done
	return result.value, result.err
}

func (p *WorkerPool) closeQueues() {
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		close(p.stop)
		for _, worker := range p.workers {
			if job := worker.runningJob(); job != nil {
				job.interrupt()
			}
		}
	}
	p.mu.Unlock()
}

// Close stops all workers. The caller closes the VM separately after this
// method returns.
func (p *WorkerPool) Close() {
	p.closeQueues()
	p.wg.Wait()
}
