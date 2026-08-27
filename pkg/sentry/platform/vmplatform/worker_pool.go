// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package vmplatform

import (
	"context"
	"fmt"
	"runtime"
	"sync"
)

// Priority determines worker queue ordering.
type Priority uint8

const (
	PriorityApplication Priority = iota
	PriorityMonitor
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
	ctx      context.Context
	priority Priority
	run      func(context.Context, VCPU) (any, error)
	done     chan workerResult

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

// WorkerPool is a fixed set of thread-affine vCPU workers shared by
// application execution and high-priority monitor transactions.
type WorkerPool struct {
	vm VM

	high   chan *workerJob
	normal chan *workerJob
	stop   chan struct{}

	mu      sync.Mutex
	workers []*vcpuWorker
	idle    int
	closed  bool
	next    int

	wg sync.WaitGroup
}

// NewWorkerPool creates count vCPUs on count permanently locked OS threads.
func NewWorkerPool(vm VM, count int) (*WorkerPool, error) {
	if vm == nil {
		return nil, fmt.Errorf("nil VM")
	}
	if count <= 0 {
		return nil, fmt.Errorf("invalid vCPU worker count %d", count)
	}
	p := &WorkerPool{
		vm:     vm,
		high:   make(chan *workerJob, count*2),
		normal: make(chan *workerJob),
		stop:   make(chan struct{}),
		idle:   count,
	}
	ready := make(chan workerInitialized, count)
	for i := 0; i < count; i++ {
		p.wg.Add(1)
		go p.workerMain(i, ready)
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
		var job *workerJob
		select {
		case job = <-p.high:
		default:
			select {
			case job = <-p.high:
			case job = <-p.normal:
			case <-p.stop:
				return
			}
		}
		if job == nil {
			continue
		}
		p.markBusy()
		w.setRunning(job)
		job.assign(w)
		value, err := job.run(job.ctx, vcpu)
		job.unassign(w)
		w.setRunning(nil)
		p.markIdle()
		select {
		case job.done <- workerResult{value: value, err: err}:
		default:
		}
	}
}

func (p *WorkerPool) markBusy() {
	p.mu.Lock()
	p.idle--
	p.mu.Unlock()
}

func (p *WorkerPool) markIdle() {
	p.mu.Lock()
	p.idle++
	p.mu.Unlock()
}

// Do runs fn on a worker. A pending monitor job interrupts one application
// worker if no worker is idle.
func (p *WorkerPool) Do(ctx context.Context, priority Priority, fn func(context.Context, VCPU) (any, error)) (any, error) {
	if fn == nil {
		return nil, fmt.Errorf("nil vCPU job")
	}
	job := &workerJob{ctx: ctx, priority: priority, run: fn, done: make(chan workerResult, 1)}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, fmt.Errorf("vCPU worker pool is closed")
	}
	if priority == PriorityMonitor && p.idle == 0 {
		p.interruptApplicationLocked()
	}
	p.mu.Unlock()

	queue := p.normal
	if priority == PriorityMonitor {
		queue = p.high
	}
	select {
	case queue <- job:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.stop:
		return nil, fmt.Errorf("vCPU worker pool is closed")
	}
	select {
	case result := <-job.done:
		return result.value, result.err
	case <-ctx.Done():
		job.interrupt()
		select {
		case result := <-job.done:
			// Cancellation and a synchronous guest exit may race. If the
			// syscall/fault HVC won, its PC and register state are already
			// architecturally committed and must be delivered; reporting only
			// context cancellation would resume after the instruction without
			// emulating it. A monitor-recorded interrupted exit is the only
			// application result that cancellation supersedes.
			if result.err == nil {
				if exit, ok := result.value.(Exit); ok && exit.Reason != ExitInterrupted {
					return result.value, nil
				}
			}
			return nil, ctx.Err()
		case <-p.stop:
			return nil, ctx.Err()
		}
	case <-p.stop:
		job.interrupt()
		return nil, fmt.Errorf("vCPU worker pool is closed")
	}
}

// RunApplication executes one application vCPU job.
func (p *WorkerPool) RunApplication(ctx context.Context, state any) (Exit, error) {
	value, err := p.Do(ctx, PriorityApplication, func(ctx context.Context, vcpu VCPU) (any, error) {
		return vcpu.Run(ctx, state)
	})
	if err != nil {
		return Exit{}, err
	}
	exit, ok := value.(Exit)
	if !ok {
		return Exit{}, fmt.Errorf("application vCPU returned result of type %T, want vmplatform.Exit", value)
	}
	return exit, nil
}

func (p *WorkerPool) interruptApplicationLocked() {
	if len(p.workers) == 0 {
		return
	}
	for i := 0; i < len(p.workers); i++ {
		index := (p.next + i) % len(p.workers)
		if job := p.workers[index].runningJob(); job != nil && job.priority == PriorityApplication {
			p.next = (index + 1) % len(p.workers)
			job.interrupt()
			return
		}
	}
}

// InterruptApplications requests an exit from every application job. Monitor
// jobs are never interrupted because their acknowledged mutations must run to
// completion.
func (p *WorkerPool) InterruptApplications() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, worker := range p.workers {
		if job := worker.runningJob(); job != nil && job.priority == PriorityApplication {
			job.interrupt()
		}
	}
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
