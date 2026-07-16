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

// InternalConsole owns the control-side peers of three Sentry-internal pipes.
// The process-side endpoints live in its FDTable. Both sides are serialized as
// part of Kernel state, so live fork duplicates queued data and connectivity.
//
// +stateify savable
type InternalConsole struct {
	mu sync.Mutex `state:"nosave"`

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
		file = console.stdout
	case InternalConsoleStderr:
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
		return nil, false, false, err
	}
	return buf[:n], n == 0, false, nil
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
	written, err := console.stdin.Write(ctx, usermem.BytesIOSequence(data), vfs.WriteOptions{})
	if linuxerr.Equals(linuxerr.ErrWouldBlock, err) {
		return int(written), true, nil
	}
	return int(written), false, err
}

func (k *Kernel) releaseInternalConsoles(ctx context.Context) {
	k.internalConsolesMu.Lock()
	consoles := k.internalConsoles
	k.internalConsoles = nil
	k.internalConsolesMu.Unlock()
	for _, console := range consoles {
		console.stdin.DecRef(ctx)
		console.stdout.DecRef(ctx)
		console.stderr.DecRef(ctx)
	}
}
