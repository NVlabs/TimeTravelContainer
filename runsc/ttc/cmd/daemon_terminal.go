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

package cmd

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"
)

const (
	daemonTerminalSizeBytes = 8
	daemonStdinWindowBytes  = 1 << 20
)

type daemonStdinQueue struct {
	writer *io.PipeWriter
	ack    func(int) error
	mu     sync.Mutex
	chunks [][]byte
	bytes  int
	closed bool
	err    error
	notify chan struct{}
}

func newDaemonStdinQueue(writer *io.PipeWriter, ack func(int) error) *daemonStdinQueue {
	queue := &daemonStdinQueue{writer: writer, ack: ack, notify: make(chan struct{}, 1)}
	go queue.pump()
	return queue
}

func (queue *daemonStdinQueue) enqueue(data []byte) bool {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if queue.closed || len(data) > daemonStdinWindowBytes-queue.bytes {
		return false
	}
	queue.chunks = append(queue.chunks, append([]byte(nil), data...))
	queue.bytes += len(data)
	select {
	case queue.notify <- struct{}{}:
	default:
	}
	return true
}

func (queue *daemonStdinQueue) closeWithError(err error) {
	queue.mu.Lock()
	if !queue.closed {
		queue.closed = true
		queue.err = err
	}
	queue.mu.Unlock()
	select {
	case queue.notify <- struct{}{}:
	default:
	}
}

func (queue *daemonStdinQueue) pump() {
	for {
		queue.mu.Lock()
		if len(queue.chunks) != 0 {
			chunk := queue.chunks[0]
			queue.chunks[0] = nil
			queue.chunks = queue.chunks[1:]
			queue.mu.Unlock()
			if _, err := queue.writer.Write(chunk); err != nil {
				queue.writer.CloseWithError(err)
				return
			}
			queue.mu.Lock()
			queue.bytes -= len(chunk)
			queue.mu.Unlock()
			if queue.ack != nil {
				if err := queue.ack(len(chunk)); err != nil {
					queue.writer.CloseWithError(err)
					return
				}
			}
			continue
		}
		closed, err := queue.closed, queue.err
		queue.mu.Unlock()
		if closed {
			if err != nil {
				queue.writer.CloseWithError(err)
			} else {
				queue.writer.Close()
			}
			return
		}
		<-queue.notify
	}
}

type daemonStdinFlow struct {
	mu          sync.Mutex
	outstanding int
	closed      bool
	notify      chan struct{}
}

func newDaemonStdinFlow() *daemonStdinFlow {
	return &daemonStdinFlow{notify: make(chan struct{}, 1)}
}

func (flow *daemonStdinFlow) reserve(bytes int) error {
	if bytes < 0 || bytes > daemonStdinWindowBytes {
		return fmt.Errorf("daemon stdin reservation %d exceeds the %d-byte window", bytes, daemonStdinWindowBytes)
	}
	for {
		flow.mu.Lock()
		if flow.closed {
			flow.mu.Unlock()
			return io.ErrClosedPipe
		}
		if bytes <= daemonStdinWindowBytes-flow.outstanding {
			flow.outstanding += bytes
			flow.mu.Unlock()
			return nil
		}
		flow.mu.Unlock()
		<-flow.notify
	}
}

func (flow *daemonStdinFlow) acknowledge(bytes int) error {
	flow.mu.Lock()
	defer flow.mu.Unlock()
	if bytes < 0 || bytes > flow.outstanding {
		return fmt.Errorf("invalid daemon stdin acknowledgement %d with %d bytes outstanding", bytes, flow.outstanding)
	}
	flow.outstanding -= bytes
	select {
	case flow.notify <- struct{}{}:
	default:
	}
	return nil
}

func (flow *daemonStdinFlow) close() {
	flow.mu.Lock()
	flow.closed = true
	flow.mu.Unlock()
	select {
	case flow.notify <- struct{}{}:
	default:
	}
}

func encodeDaemonStdinAcknowledgement(bytes int) []byte {
	var payload [4]byte
	binary.BigEndian.PutUint32(payload[:], uint32(bytes))
	return payload[:]
}

func decodeDaemonStdinAcknowledgement(payload []byte) (int, error) {
	if len(payload) != 4 {
		return 0, fmt.Errorf("invalid daemon stdin acknowledgement length %d", len(payload))
	}
	return int(binary.BigEndian.Uint32(payload)), nil
}

type daemonTerminalInput struct {
	reader  io.Reader
	conn    net.Conn
	writeMu *sync.Mutex

	sizeMu         sync.Mutex
	cols           int
	rows           int
	sizeReceived   bool
	sizeVersion    uint64
	sizeReady      chan struct{}
	resizeEvents   chan struct{}
	resizeWatching bool

	// gone is closed once the client's connection breaks.
	gone     chan struct{}
	goneOnce sync.Once
}

func newDaemonTerminalInput(reader io.Reader, conn net.Conn, writeMu *sync.Mutex) *daemonTerminalInput {
	return &daemonTerminalInput{
		reader: reader, conn: conn, writeMu: writeMu,
		sizeReady: make(chan struct{}, 1), resizeEvents: make(chan struct{}, 1),
		gone: make(chan struct{}),
	}
}

// clientGone is closed once the client's connection breaks.
func (input *daemonTerminalInput) clientGone() <-chan struct{} {
	return input.gone
}

func (input *daemonTerminalInput) markGone() {
	input.goneOnce.Do(func() { close(input.gone) })
}

func (input *daemonTerminalInput) Read(data []byte) (int, error) {
	return input.reader.Read(data)
}

func (input *daemonTerminalInput) requestTerminal(raw bool) error {
	value := byte(0)
	if raw {
		value = 1
	}
	return writeDaemonFrame(input.conn, input.writeMu, daemonFrameTerminal, []byte{value})
}

func (input *daemonTerminalInput) setRawTerminal(raw bool) error {
	if err := input.requestTerminal(raw); err != nil {
		return fmt.Errorf("configuring attached client terminal: %w", err)
	}
	return nil
}

func (input *daemonTerminalInput) terminalSize() (cols, rows int, ok bool, err error) {
	input.sizeMu.Lock()
	if input.sizeReceived {
		cols, rows = input.cols, input.rows
		input.sizeMu.Unlock()
		return cols, rows, cols > 0 && rows > 0, nil
	}
	version := input.sizeVersion
	input.sizeMu.Unlock()
	if err := input.requestTerminal(false); err != nil {
		return 0, 0, false, fmt.Errorf("requesting attached client terminal size: %w", err)
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-input.sizeReady:
			input.sizeMu.Lock()
			if input.sizeVersion > version {
				cols, rows = input.cols, input.rows
				input.sizeMu.Unlock()
				return cols, rows, cols > 0 && rows > 0, nil
			}
			input.sizeMu.Unlock()
		case <-timer.C:
			return 0, 0, false, fmt.Errorf("timed out waiting for attached client terminal size")
		}
	}
}

func (input *daemonTerminalInput) terminalResizeEvents() <-chan struct{} {
	input.sizeMu.Lock()
	input.resizeWatching = true
	for {
		select {
		case <-input.resizeEvents:
		default:
			input.sizeMu.Unlock()
			return input.resizeEvents
		}
	}
}

func (input *daemonTerminalInput) updateSize(rows, cols int) {
	input.sizeMu.Lock()
	input.rows = rows
	input.cols = cols
	input.sizeReceived = true
	input.sizeVersion++
	publishResize := input.resizeWatching
	input.sizeMu.Unlock()
	select {
	case input.sizeReady <- struct{}{}:
	default:
	}
	if publishResize {
		select {
		case input.resizeEvents <- struct{}{}:
		default:
		}
	}
}

type daemonClientTerminal struct {
	write    func(byte, []byte) error
	fd       int
	usable   bool
	sizeSent bool
	rawState *term.State

	resizeSignals chan os.Signal
	done          chan struct{}
	closeOnce     sync.Once
}

func newDaemonClientTerminal(input io.Reader, write func(byte, []byte) error) *daemonClientTerminal {
	session := &daemonClientTerminal{write: write, done: make(chan struct{})}
	file, ok := input.(*os.File)
	if ok && term.IsTerminal(int(file.Fd())) {
		session.fd = int(file.Fd())
		session.usable = true
	}
	return session
}

func (session *daemonClientTerminal) configure(payload []byte) error {
	if len(payload) != 1 || payload[0] > 1 {
		return fmt.Errorf("invalid daemon terminal control payload")
	}
	raw := payload[0] == 1
	if session.usable {
		if raw && session.rawState == nil {
			state, err := term.MakeRaw(session.fd)
			if err != nil {
				return fmt.Errorf("making attached stdin terminal raw: %w", err)
			}
			session.rawState = state
		}
		if !raw && session.rawState != nil {
			if err := term.Restore(session.fd, session.rawState); err != nil {
				return fmt.Errorf("restoring attached stdin terminal: %w", err)
			}
			session.rawState = nil
		}
		session.startResizeWatcher()
	}
	if !session.sizeSent {
		if err := session.sendSize(); err != nil {
			return err
		}
		session.sizeSent = true
	}
	return nil
}

func (session *daemonClientTerminal) sendSize() error {
	rows, cols := 0, 0
	if session.usable {
		var err error
		cols, rows, err = term.GetSize(session.fd)
		if err != nil {
			return fmt.Errorf("reading attached terminal size: %w", err)
		}
	}
	var payload [daemonTerminalSizeBytes]byte
	binary.BigEndian.PutUint32(payload[0:4], uint32(rows))
	binary.BigEndian.PutUint32(payload[4:8], uint32(cols))
	return session.write(daemonFrameResize, payload[:])
}

func (session *daemonClientTerminal) startResizeWatcher() {
	if session.resizeSignals != nil {
		return
	}
	session.resizeSignals = make(chan os.Signal, 1)
	signal.Notify(session.resizeSignals, syscall.SIGWINCH)
	go func() {
		for {
			select {
			case <-session.resizeSignals:
				_ = session.sendSize()
			case <-session.done:
				return
			}
		}
	}()
}

func (session *daemonClientTerminal) Close() error {
	var err error
	session.closeOnce.Do(func() {
		if session.resizeSignals != nil {
			signal.Stop(session.resizeSignals)
		}
		close(session.done)
		if session.rawState != nil {
			err = term.Restore(session.fd, session.rawState)
			session.rawState = nil
		}
	})
	return err
}

func decodeDaemonTerminalSize(payload []byte) (rows, cols int, error error) {
	if len(payload) != daemonTerminalSizeBytes {
		return 0, 0, fmt.Errorf("invalid daemon terminal-size payload length %d", len(payload))
	}
	rows = int(binary.BigEndian.Uint32(payload[0:4]))
	cols = int(binary.BigEndian.Uint32(payload[4:8]))
	return rows, cols, nil
}
