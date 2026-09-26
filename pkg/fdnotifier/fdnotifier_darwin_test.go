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

//go:build darwin

package fdnotifier

import (
	"net"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/waiter"
)

// tcpPair returns the FDs of both ends of a TCP connection.
func tcpPair(t *testing.T) (client, server int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	s := <-accepted
	if s == nil {
		t.Fatal("Accept failed")
	}
	fd := func(conn net.Conn) int {
		f, err := conn.(*net.TCPConn).File()
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
		t.Cleanup(func() { f.Close() })
		return int(f.Fd())
	}
	return fd(c), fd(s)
}

// pollUntil polls fd until it reports want, and returns what it reported.
func pollUntil(t *testing.T, fd int, mask, want waiter.EventMask) waiter.EventMask {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := NonBlockingPoll(int32(fd), mask)
		if got&want == want || time.Now().After(deadline) {
			return got
		}
		time.Sleep(time.Millisecond)
	}
}

func TestNonBlockingPollSocketHalfClose(t *testing.T) {
	client, server := tcpPair(t)
	if err := unix.Shutdown(client, unix.SHUT_WR); err != nil {
		t.Fatal(err)
	}
	// Like Linux, report the half-close as EventRdHUp, and the socket as
	// still writable rather than hung up.
	const mask = waiter.ReadableEvents | waiter.WritableEvents | waiter.EventRdHUp
	if got, want := pollUntil(t, server, mask, waiter.EventRdHUp), waiter.ReadableEvents|waiter.WritableEvents|waiter.EventRdHUp; got != want {
		t.Errorf("NonBlockingPoll after the peer's SHUT_WR = %#x, want %#x", got, want)
	}
	if got, want := NonBlockingPoll(int32(server), waiter.ReadableEvents|waiter.WritableEvents), waiter.ReadableEvents|waiter.WritableEvents; got != want {
		t.Errorf("NonBlockingPoll without EventRdHUp = %#x, want %#x", got, want)
	}
	// Once both directions are shut down, the socket is hung up.
	if err := unix.Shutdown(server, unix.SHUT_WR); err != nil {
		t.Fatal(err)
	}
	if got := pollUntil(t, server, mask, waiter.EventHUp); got&waiter.EventHUp == 0 {
		t.Errorf("NonBlockingPoll after both shut down = %#x, want EventHUp", got)
	}
}

func TestNonBlockingPollSocketLocalShutdown(t *testing.T) {
	_, server := tcpPair(t)
	if err := unix.Shutdown(server, unix.SHUT_WR); err != nil {
		t.Fatal(err)
	}
	// Linux reports writes as ready, since they fail immediately, and no
	// hangup while the peer can still send.
	if got, want := NonBlockingPoll(int32(server), waiter.ReadableEvents|waiter.WritableEvents), waiter.WritableEvents; got != want {
		t.Errorf("NonBlockingPoll after local SHUT_WR = %#x, want %#x", got, want)
	}
}

func TestNonBlockingPollPipe(t *testing.T) {
	var p [2]int
	if err := unix.Pipe(p[:]); err != nil {
		t.Fatal(err)
	}
	defer unix.Close(p[0])
	unix.Close(p[1])
	if got := NonBlockingPoll(int32(p[0]), waiter.ReadableEvents); got&waiter.EventHUp == 0 || got&waiter.EventErr != 0 {
		t.Errorf("NonBlockingPoll of a pipe without writers = %#x, want EventHUp and no EventErr", got)
	}

	var q [2]int
	if err := unix.Pipe(q[:]); err != nil {
		t.Fatal(err)
	}
	defer unix.Close(q[1])
	unix.Close(q[0])
	if got, want := NonBlockingPoll(int32(q[1]), waiter.WritableEvents), waiter.WritableEvents|waiter.EventErr; got != want {
		t.Errorf("NonBlockingPoll of a pipe without readers = %#x, want %#x", got, want)
	}
}

func TestNonBlockingPollDeviceWithoutPoll(t *testing.T) {
	f, err := os.OpenFile("/dev/null", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// Darwin's poll(2) reports POLLNVAL for /dev/null; Linux reports it
	// ready.
	if got, want := NonBlockingPoll(int32(f.Fd()), waiter.ReadableEvents|waiter.WritableEvents), waiter.ReadableEvents|waiter.WritableEvents; got != want {
		t.Errorf("NonBlockingPoll of /dev/null = %#x, want %#x", got, want)
	}
}

// waitNotification registers a waiter for mask on fd and returns a channel
// that receives the events it is notified of.
func waitNotification(t *testing.T, fd int, mask waiter.EventMask) <-chan waiter.EventMask {
	t.Helper()
	var q waiter.Queue
	if err := AddFD(int32(fd), &q); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { RemoveFD(int32(fd)) })
	notified := make(chan waiter.EventMask, 16)
	e := waiter.NewFunctionEntry(mask, func(m waiter.EventMask) {
		select {
		case notified <- m:
		default:
		}
	})
	q.EventRegister(&e)
	t.Cleanup(func() { q.EventUnregister(&e) })
	if err := UpdateFD(int32(fd)); err != nil {
		t.Fatal(err)
	}
	return notified
}

func TestNotifierReportsHalfCloseToRdHUpWaiter(t *testing.T) {
	client, server := tcpPair(t)
	notified := waitNotification(t, server, waiter.EventRdHUp)
	if err := unix.Shutdown(client, unix.SHUT_WR); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-notified:
		if got&waiter.EventRdHUp == 0 || got&waiter.EventHUp != 0 {
			t.Errorf("notified of %#x after the peer's SHUT_WR, want EventRdHUp without EventHUp", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a waiter for EventRdHUp alone wasn't notified of the half-close")
	}
}

func TestNotifierReportsHangupToHUpWaiter(t *testing.T) {
	var p [2]int
	if err := unix.Pipe(p[:]); err != nil {
		t.Fatal(err)
	}
	defer unix.Close(p[0])
	notified := waitNotification(t, p[0], waiter.EventHUp)
	unix.Close(p[1])
	select {
	case got := <-notified:
		if got&waiter.EventHUp == 0 {
			t.Errorf("notified of %#x after the writer closed, want EventHUp", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a waiter for EventHUp alone wasn't notified that the writer closed")
	}
}
