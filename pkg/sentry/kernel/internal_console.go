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

package kernel

import (
	"fmt"
	"io"
	"time"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/pipefs"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/usermem"
	"gvisor.dev/gvisor/pkg/waiter"
)

const maxInternalConsoleIO = 1 << 20

// InternalConsoleStream identifies an output stream of an internal console.
type InternalConsoleStream uint8

const (
	// InternalConsoleStdout is the console's standard output stream.
	InternalConsoleStdout InternalConsoleStream = 1
	// InternalConsoleStderr is the console's standard error stream.
	InternalConsoleStderr InternalConsoleStream = 2
)

// InternalConsoleReadiness identifies control-side console operations that can
// make progress without blocking.
type InternalConsoleReadiness struct {
	Stdout bool
	Stderr bool
	Stdin  bool
}

// InternalConsoleObservation is a versioned, point-in-time view of the state
// relevant to deciding whether an interactive terminal is waiting for input.
// Counters are part of Sentry state and therefore survive live fork and
// dehydration. MonotonicNS is measured in the guest kernel's monotonic clock.
type InternalConsoleObservation struct {
	Version                  uint32 `json:"version"`
	SessionEpoch             uint64 `json:"session_epoch"`
	MonotonicNS              int64  `json:"monotonic_ns"`
	Terminal                 bool   `json:"terminal"`
	InputPendingBytes        uint64 `json:"input_pending_bytes"`
	InputReadable            bool   `json:"input_readable"`
	InputConsumedBytes       uint64 `json:"input_consumed_bytes"`
	ReplicaBlockedReaders    uint64 `json:"replica_blocked_readers"`
	ReplicaReadWaiters       uint64 `json:"replica_read_waiters"`
	Canonical                bool   `json:"canonical"`
	Echo                     bool   `json:"echo"`
	TermiosGeneration        uint64 `json:"termios_generation"`
	ForegroundPGID           int32  `json:"foreground_pgid"`
	ForegroundPGIDGeneration uint64 `json:"foreground_pgid_generation"`
	SessionLeaderPID         int32  `json:"session_leader_pid"`
	OutputBytes              uint64 `json:"output_bytes"`
	InputFlushGeneration     uint64 `json:"input_flush_generation"`
	ReplicaOpenCount         uint64 `json:"replica_open_count"`
}

// InternalConsole owns either the control-side peers of three Sentry-internal
// pipes or the master of a Sentry-internal PTY. The process-side endpoints live
// in its FDTable. Both sides are serialized as part of Kernel state, so live
// fork duplicates queued data and connectivity.
//
// +stateify savable
type InternalConsole struct {
	mu sync.Mutex `state:"nosave"`

	sessionEpoch uint64
	terminal     bool
	stdinClosed  bool
	master       *vfs.FileDescription

	stdin  *vfs.FileDescription
	stdout *vfs.FileDescription
	stderr *vfs.FileDescription
}

type internalConsoleObserver interface {
	InternalConsoleObservation() InternalConsoleObservation
	InternalConsoleObservationEventRegister(*waiter.Entry)
	InternalConsoleObservationEventUnregister(*waiter.Entry)
}

// ObserveInternalConsole returns one coherent terminal observation without
// consuming input or output. Pipe-backed exec sessions report Terminal=false
// and retain the generic console-level fields.
func (k *Kernel) ObserveInternalConsole(ctx context.Context, id string) (InternalConsoleObservation, error) {
	console, err := k.internalConsole(id)
	if err != nil {
		return InternalConsoleObservation{}, err
	}
	console.mu.Lock()
	defer console.mu.Unlock()
	observation := InternalConsoleObservation{
		Version: 1, SessionEpoch: console.sessionEpoch, Terminal: console.terminal,
		MonotonicNS: k.MonotonicClock().Now().Nanoseconds(),
	}
	if !console.terminal {
		return observation, nil
	}
	observer, ok := console.master.Impl().(internalConsoleObserver)
	if !ok {
		return InternalConsoleObservation{}, fmt.Errorf("internal console %q master %T does not expose observations", id, console.master.Impl())
	}
	terminal := observer.InternalConsoleObservation()
	terminal.Version = observation.Version
	terminal.SessionEpoch = observation.SessionEpoch
	terminal.MonotonicNS = observation.MonotonicNS
	terminal.Terminal = true
	return terminal, nil
}

// WatchInternalConsole emits an initial terminal observation, coalesced
// snapshots after Sentry-side change notifications, and periodic heartbeats.
// It performs no polling. cancel terminates the transient subscription during
// branch lifecycle transitions.
func (k *Kernel) WatchInternalConsole(ctx context.Context, id string, coalesce, heartbeat time.Duration, cancel <-chan struct{}, emit func(InternalConsoleObservation) error) error {
	if coalesce <= 0 || heartbeat <= 0 {
		return fmt.Errorf("internal console watch intervals must be positive")
	}
	console, err := k.internalConsole(id)
	if err != nil {
		return err
	}
	console.mu.Lock()
	if !console.terminal {
		console.mu.Unlock()
		return fmt.Errorf("internal console %q is not a terminal", id)
	}
	master := console.master
	master.IncRef()
	sessionEpoch := console.sessionEpoch
	console.mu.Unlock()
	defer master.DecRef(ctx)

	observer, ok := master.Impl().(internalConsoleObserver)
	if !ok {
		return fmt.Errorf("internal console %q master %T does not expose observations", id, master.Impl())
	}
	entry, changed := waiter.NewTransientChannelEntry(waiter.EventInternal)
	observer.InternalConsoleObservationEventRegister(&entry)
	defer observer.InternalConsoleObservationEventUnregister(&entry)

	observe := func() InternalConsoleObservation {
		observation := observer.InternalConsoleObservation()
		observation.Version = 1
		observation.SessionEpoch = sessionEpoch
		observation.MonotonicNS = k.MonotonicClock().Now().Nanoseconds()
		observation.Terminal = true
		return observation
	}
	semantic := func(observation InternalConsoleObservation) InternalConsoleObservation {
		observation.MonotonicNS = 0
		return observation
	}
	last := observe()
	if err := emit(last); err != nil {
		return err
	}

	heartbeatTimer := time.NewTimer(heartbeat)
	defer heartbeatTimer.Stop()
	coalesceTimer := time.NewTimer(coalesce)
	if !coalesceTimer.Stop() {
		<-coalesceTimer.C
	}
	defer coalesceTimer.Stop()
	resetHeartbeat := func() {
		if !heartbeatTimer.Stop() {
			select {
			case <-heartbeatTimer.C:
			default:
			}
		}
		heartbeatTimer.Reset(heartbeat)
	}
	for {
		select {
		case <-cancel:
			return nil
		case <-heartbeatTimer.C:
			last = observe()
			if err := emit(last); err != nil {
				return err
			}
			heartbeatTimer.Reset(heartbeat)
		case <-changed:
			coalesceTimer.Reset(coalesce)
			select {
			case <-cancel:
				if !coalesceTimer.Stop() {
					select {
					case <-coalesceTimer.C:
					default:
					}
				}
				return nil
			case <-coalesceTimer.C:
			}
			current := observe()
			if semantic(current) == semantic(last) {
				continue
			}
			if err := emit(current); err != nil {
				return err
			}
			last = current
			resetHeartbeat()
		}
	}
}

// NewInternalConsoleFDTable creates process stdio backed only by Sentry-owned
// pipes and registers their control-side peers under id. The caller owns the
// returned FDTable reference.
func (k *Kernel) NewInternalConsoleFDTable(ctx context.Context, id string) (*FDTable, error) {
	if id == "" {
		return nil, fmt.Errorf("internal console ID must not be empty")
	}

	type pipePair struct {
		reader *vfs.FileDescription
		writer *vfs.FileDescription
	}
	pairs := make([]pipePair, 0, 3)
	for range 3 {
		reader, writer, err := pipefs.NewConnectedPipeFDs(ctx, k.PipeMount(), 0)
		if err != nil {
			for _, pair := range pairs {
				pair.reader.DecRef(ctx)
				pair.writer.DecRef(ctx)
			}
			return nil, fmt.Errorf("creating internal console pipe: %w", err)
		}
		pairs = append(pairs, pipePair{reader: reader, writer: writer})
	}

	// Only the control peers are nonblocking. Guest stdio retains ordinary
	// blocking pipe semantics.
	controlPeers := []*vfs.FileDescription{pairs[0].writer, pairs[1].reader, pairs[2].reader}
	for _, peer := range controlPeers {
		if err := peer.SetStatusFlags(ctx, auth.CredentialsFromContext(ctx), peer.StatusFlags()|linux.O_NONBLOCK); err != nil {
			for _, pair := range pairs {
				pair.reader.DecRef(ctx)
				pair.writer.DecRef(ctx)
			}
			return nil, fmt.Errorf("making internal console peer nonblocking: %w", err)
		}
	}

	k.internalConsolesMu.Lock()
	if _, ok := k.internalConsoles[id]; ok {
		k.internalConsolesMu.Unlock()
		for _, pair := range pairs {
			pair.reader.DecRef(ctx)
			pair.writer.DecRef(ctx)
		}
		return nil, fmt.Errorf("internal console %q already exists", id)
	}
	if k.internalConsoles == nil {
		k.internalConsoles = make(map[string]*InternalConsole)
	}
	k.nextInternalConsoleEpoch++
	console := &InternalConsole{
		sessionEpoch: k.nextInternalConsoleEpoch,
		stdin:        pairs[0].writer,
		stdout:       pairs[1].reader,
		stderr:       pairs[2].reader,
	}
	k.internalConsoles[id] = console
	k.internalConsolesMu.Unlock()

	fdTable := k.NewFDTable()
	guestFiles := []*vfs.FileDescription{pairs[0].reader, pairs[1].writer, pairs[2].writer}
	for guestFD, file := range guestFiles {
		if _, err := fdTable.NewFDAt(ctx, int32(guestFD), file, FDFlags{}); err != nil {
			fdTable.DecRef(ctx)
			k.internalConsolesMu.Lock()
			delete(k.internalConsoles, id)
			k.internalConsolesMu.Unlock()
			for _, pair := range pairs {
				pair.reader.DecRef(ctx)
				pair.writer.DecRef(ctx)
			}
			return nil, fmt.Errorf("installing internal console fd %d: %w", guestFD, err)
		}
	}
	for _, file := range guestFiles {
		file.DecRef(ctx)
	}
	return fdTable, nil
}

// NewInternalTTYConsoleFDTable installs replica as the process stdin, stdout, and
// stderr and registers master as its nonblocking control endpoint. master and
// replica are borrowed; the caller retains its references.
func (k *Kernel) NewInternalTTYConsoleFDTable(ctx context.Context, id string, master, replica *vfs.FileDescription) (*FDTable, error) {
	if id == "" {
		return nil, fmt.Errorf("internal console ID must not be empty")
	}
	if master == nil || replica == nil {
		return nil, fmt.Errorf("internal PTY master and replica must not be nil")
	}
	if err := master.SetStatusFlags(ctx, auth.CredentialsFromContext(ctx), master.StatusFlags()|linux.O_NONBLOCK); err != nil {
		return nil, fmt.Errorf("making internal PTY master nonblocking: %w", err)
	}

	k.internalConsolesMu.Lock()
	if _, ok := k.internalConsoles[id]; ok {
		k.internalConsolesMu.Unlock()
		return nil, fmt.Errorf("internal console %q already exists", id)
	}
	if k.internalConsoles == nil {
		k.internalConsoles = make(map[string]*InternalConsole)
	}
	master.IncRef()
	k.nextInternalConsoleEpoch++
	console := &InternalConsole{sessionEpoch: k.nextInternalConsoleEpoch, terminal: true, master: master}
	k.internalConsoles[id] = console
	k.internalConsolesMu.Unlock()

	fdTable := k.NewFDTable()
	for guestFD := range 3 {
		replaced, err := fdTable.NewFDAt(ctx, int32(guestFD), replica, FDFlags{})
		if replaced != nil {
			replaced.DecRef(ctx)
		}
		if err != nil {
			fdTable.DecRef(ctx)
			k.internalConsolesMu.Lock()
			if k.internalConsoles[id] == console {
				delete(k.internalConsoles, id)
			}
			k.internalConsolesMu.Unlock()
			master.DecRef(ctx)
			return nil, fmt.Errorf("installing internal PTY fd %d: %w", guestFD, err)
		}
	}
	return fdTable, nil
}

func (k *Kernel) internalConsole(id string) (*InternalConsole, error) {
	k.internalConsolesMu.Lock()
	console := k.internalConsoles[id]
	k.internalConsolesMu.Unlock()
	if console == nil {
		return nil, fmt.Errorf("internal console %q does not exist", id)
	}
	return console, nil
}

// ReadInternalConsole reads up to maxBytes from one output stream without
// blocking. wouldBlock is true when no output is currently available.
func (k *Kernel) ReadInternalConsole(ctx context.Context, id string, stream InternalConsoleStream, maxBytes int) (data []byte, eof, wouldBlock bool, err error) {
	if maxBytes <= 0 || maxBytes > maxInternalConsoleIO {
		return nil, false, false, fmt.Errorf("internal console read size %d is outside 1..%d", maxBytes, maxInternalConsoleIO)
	}
	console, err := k.internalConsole(id)
	if err != nil {
		return nil, false, false, err
	}
	console.mu.Lock()
	defer console.mu.Unlock()
	var file *vfs.FileDescription
	switch stream {
	case InternalConsoleStdout:
		if console.terminal {
			file = console.master
		} else {
			file = console.stdout
		}
	case InternalConsoleStderr:
		if console.terminal {
			return nil, true, false, nil
		}
		file = console.stderr
	default:
		return nil, false, false, fmt.Errorf("invalid internal console stream %d", stream)
	}
	buf := make([]byte, maxBytes)
	n, err := file.Read(ctx, usermem.BytesIOSequence(buf), vfs.ReadOptions{})
	if linuxerr.Equals(linuxerr.ErrWouldBlock, err) {
		return nil, false, true, nil
	}
	if err != nil {
		if err == io.EOF {
			return nil, true, false, nil
		}
		if console.terminal && linuxerr.Equals(linuxerr.EIO, err) && file.Readiness(waiter.EventHUp)&waiter.EventHUp != 0 {
			return nil, true, false, nil
		}
		return nil, false, false, err
	}
	eof = n == 0
	if n > 0 {
		// A pipe read may return its final buffered bytes without returning EOF.
		// Report data and EOF together when the last writer is gone and the read
		// drained the buffer, avoiding a follow-up control RPC during shutdown.
		ready := file.Readiness(waiter.ReadableEvents | waiter.EventHUp)
		eof = ready&waiter.EventHUp != 0 && ready&waiter.ReadableEvents == 0
	}
	return buf[:n], eof, false, nil
}

// WaitInternalConsole waits until at least one requested control-side operation
// can make progress. It retains endpoint references while sleeping, so branch
// lifecycle operations remain independent of the wait. timedOut is true when no
// requested operation became ready before timeout.
func (k *Kernel) WaitInternalConsole(ctx context.Context, id string, want InternalConsoleReadiness, timeout time.Duration, cancel <-chan struct{}) (ready InternalConsoleReadiness, timedOut bool, err error) {
	if !want.Stdout && !want.Stderr && !want.Stdin {
		return ready, false, fmt.Errorf("internal console wait requests no operations")
	}
	if timeout < 0 {
		return ready, false, fmt.Errorf("internal console wait timeout %s is negative", timeout)
	}

	type waitTarget struct {
		file       *vfs.FileDescription
		mask       waiter.EventMask
		stdoutMask waiter.EventMask
		stderrMask waiter.EventMask
		stdinMask  waiter.EventMask
	}
	var targets []waitTarget
	addTarget := func(file *vfs.FileDescription, stdoutMask, stderrMask, stdinMask waiter.EventMask) {
		mask := stdoutMask | stderrMask | stdinMask
		for i := range targets {
			if targets[i].file == file {
				targets[i].mask |= mask
				targets[i].stdoutMask |= stdoutMask
				targets[i].stderrMask |= stderrMask
				targets[i].stdinMask |= stdinMask
				return
			}
		}
		targets = append(targets, waitTarget{file: file, mask: mask, stdoutMask: stdoutMask, stderrMask: stderrMask, stdinMask: stdinMask})
	}

	// Pin every selected endpoint before releasing the console registry lock.
	// RemoveInternalConsole may then proceed without invalidating this wait.
	k.internalConsolesMu.Lock()
	console := k.internalConsoles[id]
	if console == nil {
		k.internalConsolesMu.Unlock()
		return ready, false, fmt.Errorf("internal console %q does not exist", id)
	}
	console.mu.Lock()
	k.internalConsolesMu.Unlock()
	if want.Stdout {
		if console.terminal {
			addTarget(console.master, waiter.ReadableEvents|waiter.EventHUp|waiter.EventErr, 0, 0)
		} else {
			addTarget(console.stdout, waiter.ReadableEvents|waiter.EventHUp|waiter.EventErr, 0, 0)
		}
	}
	if want.Stderr {
		if console.terminal {
			// A terminal has one combined output stream; stderr reads report EOF.
			ready.Stderr = true
		} else {
			addTarget(console.stderr, 0, waiter.ReadableEvents|waiter.EventHUp|waiter.EventErr, 0)
		}
	}
	if want.Stdin {
		if console.stdinClosed || (!console.terminal && console.stdin == nil) {
			// Let the caller retry and observe the definitive closed-input error.
			ready.Stdin = true
		} else if console.terminal {
			addTarget(console.master, 0, 0, waiter.WritableEvents|waiter.EventHUp|waiter.EventErr)
		} else {
			addTarget(console.stdin, 0, 0, waiter.WritableEvents|waiter.EventHUp|waiter.EventErr)
		}
	}
	for i := range targets {
		targets[i].file.IncRef()
	}
	console.mu.Unlock()
	defer func() {
		for i := range targets {
			targets[i].file.DecRef(ctx)
		}
	}()

	updateReadiness := func() bool {
		for i := range targets {
			target := &targets[i]
			events := target.file.Readiness(target.mask)
			ready.Stdout = ready.Stdout || events&target.stdoutMask != 0
			ready.Stderr = ready.Stderr || events&target.stderrMask != 0
			ready.Stdin = ready.Stdin || events&target.stdinMask != 0
		}
		return ready.Stdout || ready.Stderr || ready.Stdin
	}

	notify := make(chan struct{}, 1)
	entries := make([]waiter.Entry, len(targets))
	registered := 0
	defer func() {
		for i := 0; i < registered; i++ {
			targets[i].file.EventUnregister(&entries[i])
		}
	}()
	for i := range targets {
		entries[i] = waiter.NewFunctionEntry(targets[i].mask, func(waiter.EventMask) {
			select {
			case notify <- struct{}{}:
			default:
			}
		})
		if err := targets[i].file.EventRegister(&entries[i]); err != nil {
			return ready, false, fmt.Errorf("registering internal console readiness: %w", err)
		}
		registered++
	}

	// Check after registration to close the usual readiness-registration race.
	if updateReadiness() {
		return ready, false, nil
	}
	if timeout == 0 {
		return ready, true, nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-notify:
			if updateReadiness() {
				return ready, false, nil
			}
		case <-cancel:
			// Lifecycle cancellation is a spurious wakeup to callers; they retry the
			// nonblocking operation after fork or teardown releases opMu.
			return ready, false, nil
		case <-ctx.Done():
			return ready, false, ctx.Err()
		case <-timer.C:
			return ready, true, nil
		}
	}
}

// WriteInternalConsole writes data to the process's standard input without
// blocking. A short write may be returned when the input queue is full.
func (k *Kernel) WriteInternalConsole(ctx context.Context, id string, data []byte) (n int, wouldBlock bool, err error) {
	if len(data) > maxInternalConsoleIO {
		return 0, false, fmt.Errorf("internal console write size %d exceeds %d", len(data), maxInternalConsoleIO)
	}
	console, err := k.internalConsole(id)
	if err != nil {
		return 0, false, err
	}
	console.mu.Lock()
	defer console.mu.Unlock()
	if console.stdinClosed || (!console.terminal && console.stdin == nil) {
		return 0, false, fmt.Errorf("internal console %q stdin is closed", id)
	}
	input := console.stdin
	if console.terminal {
		input = console.master
	}
	written, err := input.Write(ctx, usermem.BytesIOSequence(data), vfs.WriteOptions{})
	if linuxerr.Equals(linuxerr.ErrWouldBlock, err) {
		return int(written), true, nil
	}
	return int(written), false, err
}

// CloseInternalConsoleStdin closes the control-side stdin writer. Guest reads
// observe EOF after queued input is consumed. Closing twice is idempotent.
func (k *Kernel) CloseInternalConsoleStdin(ctx context.Context, id string) error {
	console, err := k.internalConsole(id)
	if err != nil {
		return err
	}
	console.mu.Lock()
	defer console.mu.Unlock()
	if console.stdinClosed {
		return nil
	}
	if console.terminal {
		_, err := console.master.Write(ctx, usermem.BytesIOSequence([]byte{4}), vfs.WriteOptions{})
		if err != nil {
			return err
		}
		console.stdinClosed = true
		return nil
	}
	if console.stdin != nil {
		console.stdin.DecRef(ctx)
		console.stdin = nil
	}
	console.stdinClosed = true
	return nil
}

type internalConsoleWindowSizer interface {
	SetInternalConsoleWindowSize(size linux.Winsize)
}

// ResizeInternalConsole changes the window size of a PTY-backed console and
// delivers SIGWINCH to its foreground process group when the size changed.
func (k *Kernel) ResizeInternalConsole(ctx context.Context, id string, size linux.Winsize) error {
	console, err := k.internalConsole(id)
	if err != nil {
		return err
	}
	console.mu.Lock()
	defer console.mu.Unlock()
	if !console.terminal {
		return fmt.Errorf("internal console %q is not a terminal", id)
	}
	sizer, ok := console.master.Impl().(internalConsoleWindowSizer)
	if !ok {
		return fmt.Errorf("internal console %q master %T does not support resize", id, console.master.Impl())
	}
	sizer.SetInternalConsoleWindowSize(size)
	return nil
}

// RemoveInternalConsole unregisters id and releases its control endpoints.
// Process-side file descriptions remain valid until their FD tables release
// them. Branch control serialization must prevent concurrent console I/O.
func (k *Kernel) RemoveInternalConsole(ctx context.Context, id string) error {
	k.internalConsolesMu.Lock()
	console := k.internalConsoles[id]
	if console != nil {
		delete(k.internalConsoles, id)
	}
	k.internalConsolesMu.Unlock()
	if console == nil {
		return fmt.Errorf("internal console %q does not exist", id)
	}
	releaseInternalConsole(ctx, console)
	return nil
}

func releaseInternalConsole(ctx context.Context, console *InternalConsole) {
	if console.terminal {
		console.master.DecRef(ctx)
		return
	}
	if console.stdin != nil {
		console.stdin.DecRef(ctx)
	}
	console.stdout.DecRef(ctx)
	console.stderr.DecRef(ctx)
}

func (k *Kernel) releaseInternalConsoles(ctx context.Context) {
	k.internalConsolesMu.Lock()
	consoles := k.internalConsoles
	k.internalConsoles = nil
	k.internalConsolesMu.Unlock()
	for _, console := range consoles {
		releaseInternalConsole(ctx, console)
	}
}
