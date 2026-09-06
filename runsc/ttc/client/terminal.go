// Copyright 2026 The gVisor Authors.
// Licensed under the Apache License, Version 2.0 (the "License");

package client

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"golang.org/x/term"
)

type stdinFlow struct {
	mu          sync.Mutex
	outstanding int
	closed      bool
	notify      chan struct{}
}

func newStdinFlow() *stdinFlow { return &stdinFlow{notify: make(chan struct{}, 1)} }

func (f *stdinFlow) reserve(n int) error {
	for {
		f.mu.Lock()
		if f.closed {
			f.mu.Unlock()
			return io.ErrClosedPipe
		}
		if n >= 0 && n <= daemonStdinWindow-f.outstanding {
			f.outstanding += n
			f.mu.Unlock()
			return nil
		}
		f.mu.Unlock()
		<-f.notify
	}
}

func (f *stdinFlow) ack(n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n < 0 || n > f.outstanding {
		return errors.New("invalid daemon stdin acknowledgement")
	}
	f.outstanding -= n
	select {
	case f.notify <- struct{}{}:
	default:
	}
	return nil
}

func (f *stdinFlow) close() {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	select {
	case f.notify <- struct{}{}:
	default:
	}
}

type clientTerminal struct {
	write    func(byte, []byte) error
	fd       int
	usable   bool
	sizeSent bool
	raw      *term.State
	resize   chan os.Signal
	done     chan struct{}
	once     sync.Once
}

func newClientTerminal(input io.Reader, write func(byte, []byte) error) *clientTerminal {
	t := &clientTerminal{write: write, done: make(chan struct{})}
	if file, ok := input.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		t.fd, t.usable = int(file.Fd()), true
	}
	return t
}

func (t *clientTerminal) configure(payload []byte) error {
	if len(payload) != 1 || payload[0] > 1 {
		return fmt.Errorf("invalid terminal control payload")
	}
	wantRaw := payload[0] == 1
	if t.usable && wantRaw && t.raw == nil {
		state, err := term.MakeRaw(t.fd)
		if err != nil {
			return err
		}
		t.raw = state
	}
	if t.usable && !wantRaw && t.raw != nil {
		if err := term.Restore(t.fd, t.raw); err != nil {
			return err
		}
		t.raw = nil
	}
	if t.usable && t.resize == nil {
		t.resize = make(chan os.Signal, 1)
		signal.Notify(t.resize, syscall.SIGWINCH)
		go func() {
			for {
				select {
				case <-t.resize:
					_ = t.sendSize()
				case <-t.done:
					return
				}
			}
		}()
	}
	if !t.sizeSent {
		if err := t.sendSize(); err != nil {
			return err
		}
		t.sizeSent = true
	}
	return nil
}

func (t *clientTerminal) sendSize() error {
	rows, cols := 0, 0
	if t.usable {
		var err error
		cols, rows, err = term.GetSize(t.fd)
		if err != nil {
			return err
		}
	}
	var payload [daemonTerminalSizeLen]byte
	binary.BigEndian.PutUint32(payload[:4], uint32(rows))
	binary.BigEndian.PutUint32(payload[4:], uint32(cols))
	return t.write(daemonFrameResize, payload[:])
}

func (t *clientTerminal) close() {
	t.once.Do(func() {
		if t.resize != nil {
			signal.Stop(t.resize)
		}
		close(t.done)
		if t.raw != nil {
			_ = term.Restore(t.fd, t.raw)
			t.raw = nil
		}
	})
}
