// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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
	"gvisor.dev/gvisor/pkg/sentry/kernel/pipe"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/usermem"
	"gvisor.dev/gvisor/pkg/waiter"
)

// maxInternalConsoleIO is the most a control call reads or writes at once,
// and what each of the console's pipes holds.
const maxInternalConsoleIO = pipe.MaximumPipeSize

// internalConsoleReadStart is the buffer a console read starts with.
const internalConsoleReadStart = 64 << 10

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

	// TaskWaits describes every task in the terminal's foreground process
	// group: whether it is running, and if it is blocked, in which syscall.
	//
	// The counters above answer "is anyone interested in this terminal", which
	// is not the same question as "is this program still working". A poll
	// registration is counted the moment an fd joins an epoll set and stays
	// counted while the process runs, so a runtime that registers stdin once at
	// startup — node does — looks identical whether it is idle or busy. What
	// separates them is whether any task is actually runnable, which only the
	// Sentry can say.
	TaskWaits []TaskWait `json:"task_waits"`

	// OtherTaskWaits is every task in the sandbox outside the foreground
	// group, in the same shape.
	//
	// A program can be waiting on another process in this sandbox — a tool on
	// its display server, a client on a helper it started. From the foreground
	// group alone, that is indistinguishable from waiting on a machine
	// elsewhere. With the rest of the sandbox in view, an observer can ask
	// whether anything here can move at all, which is the only version of the
	// question that has a general answer.
	OtherTaskWaits []TaskWait `json:"other_task_waits,omitempty"`
}

// TaskWait is one task's scheduling state, for deciding whether the foreground
// program is waiting for input or still doing work.
type TaskWait struct {
	// TID and PID are as seen from the terminal session's PID namespace.
	TID int32 `json:"tid"`
	PID int32 `json:"pid"`
	// State is one of "running-app", "running-sys", "blocked",
	// "blocked-uninterruptible", "stopped", or "unknown". Only the two running
	// states mean work is happening; the rest need Syscall to interpret.
	State string `json:"state"`
	// Syscall is the syscall number the task is executing, or -1 when it is not
	// in one. A blocked task's syscall is what it is blocked *on*: a read, an
	// epoll_wait, a futex, a nanosleep. That distinction is the whole point —
	// a task parked in a futex is idle, one in nanosleep or reading a socket is
	// not, and neither is visible from the terminal's own counters.
	Syscall int32 `json:"syscall"`
	// Awaiting is "epoll", "poll", or "select" when the task is blocked on a
	// set of descriptors, and empty otherwise.
	Awaiting string `json:"awaiting,omitempty"`
	// AwaitedFDs are the descriptors in that set, as the task numbers them.
	AwaitedFDs []int32 `json:"awaited_fds,omitempty"`
	// AwaitedKinds says what each of those descriptors is, in the same order:
	// "tty", "eventfd", "self-pipe", "pipe", "timerfd", "epoll", "socket",
	// "unix-socket", "other", or "closed".
	//
	// The number alone cannot settle the question this exists to answer. What
	// separates a program that has finished from one that has not is who is
	// able to make its descriptors ready: a terminal waits for a person, an
	// eventfd or a self-pipe waits for a thread of this same process, and a
	// socket waits for a machine somewhere else that is already working on a
	// reply. Only the last is proof that the program is not done.
	AwaitedKinds []string `json:"awaited_kinds,omitempty"`
	// TimeoutNS is the wait's deadline, or -1 if it is indefinite. A finite
	// deadline means something is expected to happen without any descriptor
	// becoming ready — a pending timer — which an idle prompt never has.
	TimeoutNS int64 `json:"timeout_ns,omitempty"`
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
	// Compared field by field rather than with ==, because the task states are a
	// slice and a struct containing one is not comparable. Timestamps are
	// excluded so a heartbeat does not read as a change.
	sameSemantics := func(a, b InternalConsoleObservation) bool {
		// Field by field: comparability is a property of the type, so clearing
		// the slice does not make the struct comparable.
		if a.Version != b.Version ||
			a.SessionEpoch != b.SessionEpoch ||
			a.Terminal != b.Terminal ||
			a.InputPendingBytes != b.InputPendingBytes ||
			a.InputReadable != b.InputReadable ||
			a.InputConsumedBytes != b.InputConsumedBytes ||
			a.ReplicaBlockedReaders != b.ReplicaBlockedReaders ||
			a.ReplicaReadWaiters != b.ReplicaReadWaiters ||
			a.Canonical != b.Canonical ||
			a.Echo != b.Echo ||
			a.TermiosGeneration != b.TermiosGeneration ||
			a.ForegroundPGID != b.ForegroundPGID ||
			a.ForegroundPGIDGeneration != b.ForegroundPGIDGeneration ||
			a.SessionLeaderPID != b.SessionLeaderPID ||
			a.OutputBytes != b.OutputBytes ||
			a.InputFlushGeneration != b.InputFlushGeneration ||
			a.ReplicaOpenCount != b.ReplicaOpenCount {
			return false
		}
		// Both sets, because both are in the payload and both are read. An
		// observation whose only change is a task elsewhere in the sandbox is
		// not the same observation: whether anything else can still run is
		// exactly the question the rest proof asks of that field.
		return sameTaskWaitSet(a.TaskWaits, b.TaskWaits) && sameTaskWaitSet(a.OtherTaskWaits, b.OtherTaskWaits)
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
			if sameSemantics(current, last) {
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
		if err == nil {
			_, err = reader.Impl().(*pipe.VFSPipeFD).SetPipeSize(maxInternalConsoleIO)
			if err != nil {
				reader.DecRef(ctx)
				writer.DecRef(ctx)
			}
		}
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
	// Most reads find little: one that fills a buffer of a few pages reads
	// on, into one of what it may return.
	buf := make([]byte, min(maxBytes, internalConsoleReadStart))
	n, err := file.Read(ctx, usermem.BytesIOSequence(buf), vfs.ReadOptions{})
	if err == nil && n == int64(len(buf)) && len(buf) < maxBytes {
		more := make([]byte, maxBytes)
		copy(more, buf)
		// What the second read finds, if anything, goes with the first's;
		// an end it finds, the next read finds again.
		if m, moreErr := file.Read(ctx, usermem.BytesIOSequence(more[n:]), vfs.ReadOptions{}); moreErr == nil {
			n += m
		}
		buf = more
	}
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

// CollectTaskWaits reports the scheduling state of every task in the given
// foreground process group, as seen from ns.
//
// Scoped to the foreground group rather than the whole session because that is
// the program the terminal is talking to. A backgrounded job still running is
// not a reason to say the foreground program is busy, and a shell's other
// children are not the shell.
func CollectTaskWaits(ns *PIDNamespace, pgid ProcessGroupID) []TaskWait {
	return collectTaskWaits(ns, pgid, false)
}

// CollectOtherTaskWaits reports every task outside the foreground group.
//
// The foreground group answers "is this program working". It cannot answer
// "will anything happen if nobody types", because a program can be waiting on
// another process in the same sandbox — a tool on its display server, a client
// on a helper. Those are not in the group, and without them a waiting program
// and a working one look the same.
//
// Reported separately rather than merged, so a reader that knows nothing about
// this field sees exactly what it saw before.
func CollectOtherTaskWaits(ns *PIDNamespace, pgid ProcessGroupID) []TaskWait {
	return collectTaskWaits(ns, pgid, true)
}

func collectTaskWaits(ns *PIDNamespace, pgid ProcessGroupID, others bool) []TaskWait {
	if ns == nil || pgid == 0 {
		return nil
	}
	// One lock, held across the whole walk. The exported ID helpers each take
	// this same lock, so calling them from inside would recursively acquire it
	// for reading — which deadlocks the moment a writer is queued between the
	// two acquisitions.
	ns.owner.mu.RLock()
	defer ns.owner.mu.RUnlock()

	var waits []TaskWait
	for tg, tgid := range ns.tgids {
		pg := tg.processGroup
		inGroup := pg != nil && ns.pgids[pg] == pgid
		if inGroup == others {
			continue
		}
		for t := tg.tasks.Front(); t != nil; t = t.Next() {
			wait := TaskWait{
				TID:     int32(ns.tids[t]),
				PID:     int32(tgid),
				State:   taskWaitState(t.TaskGoroutineState()),
				Syscall: taskWaitSyscall(t),
			}
			if info := t.WaitInfo(); info != nil {
				wait.Awaiting = info.Kind
				wait.AwaitedFDs = info.FDs
				wait.AwaitedKinds = info.FDKinds
				wait.TimeoutNS = info.TimeoutNS
			}
			waits = append(waits, wait)
		}
	}
	return waits
}

func taskWaitState(state TaskGoroutineState) string {
	switch state {
	case TaskGoroutineRunningApp:
		return "running-app"
	case TaskGoroutineRunningSys:
		return "running-sys"
	case TaskGoroutineBlockedInterruptible:
		return "blocked"
	case TaskGoroutineBlockedUninterruptible:
		return "blocked-uninterruptible"
	case TaskGoroutineStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

// taskWaitSyscall reports the syscall a task is executing, or -1.
//
// Only meaningful for a task that is not running application code: while it
// runs, the register holding the syscall number is the application's to use and
// says nothing.
func taskWaitSyscall(t *Task) int32 {
	if t.TaskGoroutineState() == TaskGoroutineRunningApp {
		return -1
	}
	arch := t.Arch()
	if arch == nil {
		return -1
	}
	return int32(arch.SyscallNo())
}

// TaskWaitInfo describes the wait a task is currently blocked in.
//
// The syscall number alone is not enough: epoll_wait and poll wait on a set of
// descriptors, so a program idle at its terminal and one with an outstanding
// socket read make the same call. What separates them is which descriptors are
// in the set, and whether the wait has a deadline — a pending timer makes it
// finite, an idle prompt leaves it indefinite.
type TaskWaitInfo struct {
	// Kind is "epoll", "poll", or "select".
	Kind string
	// FDs are the descriptor numbers being awaited, as the task numbers them.
	FDs []int32
	// FDKinds names what each of those descriptors is, in the same order.
	FDKinds []string
	// TimeoutNS is the wait's deadline in nanoseconds, or -1 if indefinite.
	TimeoutNS int64
}

// SetWaitInfo records what t is about to block on. Cleared by ClearWaitInfo.
func (t *Task) SetWaitInfo(kind string, fds []int32, fdKinds []string, timeoutNS int64) {
	t.waitInfoMu.Lock()
	t.waitInfo = &TaskWaitInfo{Kind: kind, FDs: fds, FDKinds: fdKinds, TimeoutNS: timeoutNS}
	t.waitInfoMu.Unlock()
}

// ClearWaitInfo forgets what t was blocked on.
func (t *Task) ClearWaitInfo() {
	t.waitInfoMu.Lock()
	t.waitInfo = nil
	t.waitInfoMu.Unlock()
}

// WaitInfo returns what t is blocked on, or nil.
func (t *Task) WaitInfo() *TaskWaitInfo {
	t.waitInfoMu.Lock()
	defer t.waitInfoMu.Unlock()
	return t.waitInfo
}

// sameTaskWait compares two task observations, including the awaited set.
//
// The descriptors are compared as a set: they come from a map, so their order
// is not stable between snapshots and comparing them in order would report a
// change on every sample.
func sameTaskWait(a, b TaskWait) bool {
	if a.TID != b.TID || a.PID != b.PID || a.State != b.State || a.Syscall != b.Syscall ||
		a.Awaiting != b.Awaiting || a.TimeoutNS != b.TimeoutNS || len(a.AwaitedFDs) != len(b.AwaitedFDs) {
		return false
	}
	seen := make(map[awaitedFD]int, len(a.AwaitedFDs))
	for i, fd := range a.AwaitedFDs {
		seen[awaitedFD{fd, awaitedKind(a, i)}]++
	}
	for i, fd := range b.AwaitedFDs {
		key := awaitedFD{fd, awaitedKind(b, i)}
		seen[key]--
		if seen[key] < 0 {
			return false
		}
	}
	return true
}

// sameTaskWaitSet compares two sets of task waits.
//
// By TID rather than by position: the order is the map iteration order of the
// PID namespace, which is not stable between snapshots, so a straight
// comparison reports a change every time nothing has happened.
func sameTaskWaitSet(a, b []TaskWait) bool {
	if len(a) != len(b) {
		return false
	}
	byTID := make(map[int32]TaskWait, len(a))
	for _, task := range a {
		byTID[task.TID] = task
	}
	for _, task := range b {
		other, ok := byTID[task.TID]
		if !ok || !sameTaskWait(other, task) {
			return false
		}
	}
	return true
}

// awaitedFD pairs a descriptor number with what it is, so that a number reused
// for something else counts as a change rather than as the same wait.
type awaitedFD struct {
	num  int32
	kind string
}

func awaitedKind(wait TaskWait, i int) string {
	if i < len(wait.AwaitedKinds) {
		return wait.AwaitedKinds[i]
	}
	return ""
}
