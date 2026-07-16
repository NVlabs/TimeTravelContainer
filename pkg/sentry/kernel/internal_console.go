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

// InternalConsole owns either the control-side peers of three Sentry-internal
// pipes or the master of a Sentry-internal PTY. The process-side endpoints live
// in its FDTable. Both sides are serialized as part of Kernel state, so live
// fork duplicates queued data and connectivity.
//
// +stateify savable
type InternalConsole struct {
	mu sync.Mutex `state:"nosave"`

	terminal    bool
	stdinClosed bool
	master      *vfs.FileDescription

	stdin  *vfs.FileDescription
	stdout *vfs.FileDescription
	stderr *vfs.FileDescription
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
	console := &InternalConsole{
		stdin:  pairs[0].writer,
		stdout: pairs[1].reader,
		stderr: pairs[2].reader,
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
	console := &InternalConsole{terminal: true, master: master}
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

func (k *Kernel) releaseInternalConsoles(ctx context.Context) {
	k.internalConsolesMu.Lock()
	consoles := k.internalConsoles
	k.internalConsoles = nil
	k.internalConsolesMu.Unlock()
	for _, console := range consoles {
		if console.terminal {
			console.master.DecRef(ctx)
			continue
		}
		if console.stdin != nil {
			console.stdin.DecRef(ctx)
		}
		console.stdout.DecRef(ctx)
		console.stderr.DecRef(ctx)
	}
}
