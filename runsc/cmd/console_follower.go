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
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/urpc"
	"gvisor.dev/gvisor/runsc/boot"
	"gvisor.dev/gvisor/runsc/sandbox"
	ttcconsole "gvisor.dev/gvisor/runsc/ttc/console"
)

// familyFollowerIdle is how long a family's console follower outlives the
// last console it followed.
const familyFollowerIdle = 10 * time.Second

// familyFollower keeps the logs of a family's followed consoles: see
// ttcconsole.FollowerSocketName. The Sentry pushes each console's output
// through a pipe, which the Go runtime polls, so a quiet console costs the
// follower no thread.
type familyFollower struct {
	sandbox  *sandbox.Sandbox
	cid      string
	socket   string
	listener *net.UnixListener

	mu       sync.Mutex
	consoles map[string]*followedConsole
	conns    map[*net.UnixConn]struct{}
	idle     *time.Timer
	exiting  bool
	exited   chan struct{}
}

// followedConsole is one console being followed, keyed by its lock's path.
type followedConsole struct {
	follow ttcconsole.Follow
	lock   *os.File
	output *os.File
	log    *ttcconsole.RotatingWriter
	done   chan struct{}
}

// runFamilyConsoleFollower serves the consoles of the family whose Sentry is
// s on socket, from the family's console directory, until none has been
// followed for familyFollowerIdle. It writes ready once it listens.
func runFamilyConsoleFollower(s *sandbox.Sandbox, cid, socket string, ready *os.File) error {
	// The lock keeps one follower to a family. A follower exiting for want
	// of consoles removes its socket, then releases the lock: ttcd starts a
	// new one only when it cannot connect, and it waits for the old one.
	lock, err := os.OpenFile(filepath.Join(filepath.Dir(socket), ttcconsole.FollowerLockName), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("opening console follower lock: %w", err)
	}
	defer lock.Close()
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			return fmt.Errorf("locking console follower: %w", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing stale console follower socket: %w", err)
	}
	address, closeDir, err := ttcconsole.SocketAddress(socket)
	if err != nil {
		return err
	}
	listener, err := net.ListenUnix(ttcconsole.FollowerNetwork, &net.UnixAddr{Name: address, Net: ttcconsole.FollowerNetwork})
	closeDir()
	if err != nil {
		return fmt.Errorf("listening for ttcd: %w", err)
	}
	// The address goes through a descriptor closed since: exitIfIdle
	// removes the socket by its path.
	listener.SetUnlinkOnClose(false)
	f := &familyFollower{
		sandbox:  s,
		cid:      cid,
		socket:   socket,
		listener: listener,
		consoles: make(map[string]*followedConsole),
		conns:    make(map[*net.UnixConn]struct{}),
		exited:   make(chan struct{}),
	}
	f.idle = time.AfterFunc(familyFollowerIdle, f.exitIfIdle)
	if ready != nil {
		_, err := ready.Write([]byte{1})
		_ = ready.Close()
		if err != nil {
			return fmt.Errorf("signaling console follower readiness: %w", err)
		}
	}
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			f.mu.Lock()
			exiting := f.exiting
			f.mu.Unlock()
			if exiting {
				<-f.exited
				return nil
			}
			return fmt.Errorf("accepting ttcd: %w", err)
		}
		f.mu.Lock()
		if f.exiting {
			f.mu.Unlock()
			_ = conn.Close()
			continue
		}
		f.conns[conn] = struct{}{}
		f.mu.Unlock()
		go f.serve(conn)
	}
}

// exitIfIdle stops the follower if it follows no console: it stops taking
// requests, then lets the process exit, which releases the lock.
func (f *familyFollower) exitIfIdle() {
	f.mu.Lock()
	if f.exiting || len(f.consoles) != 0 {
		f.mu.Unlock()
		return
	}
	f.exiting = true
	_ = os.Remove(f.socket)
	_ = f.listener.Close()
	for conn := range f.conns {
		_ = conn.Close()
	}
	f.mu.Unlock()
	close(f.exited)
}

func (f *familyFollower) serve(conn *net.UnixConn) {
	defer func() {
		f.mu.Lock()
		delete(f.conns, conn)
		f.mu.Unlock()
		_ = conn.Close()
	}()
	for {
		message, files, err := ttcconsole.ReadFollowerMessage(conn)
		if err != nil {
			return
		}
		var result error
		switch message.Op {
		case ttcconsole.FollowerFollow:
			result = f.follow(message.Follow, files)
			files = nil
		case ttcconsole.FollowerStop:
			f.stop(message.Lock)
		case ttcconsole.FollowerObserve:
			f.mu.Lock()
			_, followed := f.consoles[message.Lock]
			f.mu.Unlock()
			if !followed {
				_ = ttcconsole.WriteFollowerMessage(conn, ttcconsole.FollowerMessage{Done: message.Lock})
			}
		default:
			result = fmt.Errorf("unknown console follower operation %q", message.Op)
		}
		for _, file := range files {
			_ = file.Close()
		}
		reply := ttcconsole.FollowerMessage{ID: message.ID}
		if result != nil {
			reply.Error = result.Error()
		}
		if err := ttcconsole.WriteFollowerMessage(conn, reply); err != nil {
			return
		}
	}
}

// follow has the Sentry push the console request describes, and copies it to
// its log in the background. It holds the console's lock, files[0], until it
// stops following it.
func (f *familyFollower) follow(request *ttcconsole.Follow, files []*os.File) error {
	if request == nil || request.Lock == "" || len(files) != 1 {
		for _, file := range files {
			_ = file.Close()
		}
		return errors.New("a console follow request needs a console and its lock")
	}
	console := &followedConsole{follow: *request, lock: files[0], done: make(chan struct{})}
	input, err := console.open()
	if err != nil {
		console.close()
		return err
	}
	f.mu.Lock()
	if f.exiting {
		err = errors.New("the console follower is exiting")
	} else if _, followed := f.consoles[request.Lock]; followed {
		err = fmt.Errorf("console %q is followed already", request.Lock)
	}
	if err != nil {
		f.mu.Unlock()
		_ = input.Close()
		console.close()
		return err
	}
	f.consoles[request.Lock] = console
	f.idle.Stop()
	f.mu.Unlock()
	err = f.sandbox.PushConsole(&boot.ConsolePushArgs{
		FilePayload: urpc.FilePayload{Files: []*os.File{input}},
		BranchID:    request.Branch,
		CID:         f.cid,
		ExecID:      request.ExecID,
		ConsoleID:   request.ConsoleID,
	})
	_ = input.Close()
	if err != nil {
		f.release(console, false)
		return err
	}
	go func() {
		if err := copyPushedConsole(console); err != nil {
			recordFollowError(request.Error, err)
		}
		f.release(console, true)
	}()
	return nil
}

// open opens the console's log, and the pipe the Sentry pushes it through,
// whose end for the Sentry it returns.
func (console *followedConsole) open() (*os.File, error) {
	var err error
	request := &console.follow
	if console.log, err = ttcconsole.NewRotatingWriterWithStats(request.Log, request.LogMaxBytes, request.LogMaxFiles, request.RetentionStats); err != nil {
		return nil, err
	}
	// pipe(2) and close-on-exec under ForkLock, as os.Pipe does where there
	// is no pipe2(2): a concurrent exec must not inherit either end.
	var fds [2]int
	syscall.ForkLock.RLock()
	err = unix.Pipe(fds[:])
	if err == nil {
		unix.CloseOnExec(fds[0])
		unix.CloseOnExec(fds[1])
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, fmt.Errorf("creating console push pipe: %w", err)
	}
	// The read end is nonblocking, so that the Go runtime polls it; the
	// Sentry's end blocks.
	if err := unix.SetNonblock(fds[0], true); err != nil {
		_ = unix.Close(fds[0])
		_ = unix.Close(fds[1])
		return nil, fmt.Errorf("creating console push pipe: %w", err)
	}
	console.output = os.NewFile(uintptr(fds[0]), "console-push")
	return os.NewFile(uintptr(fds[1]), "console-push-sentry"), nil
}

// close closes the console's files. Releasing its lock is what ends
// following it, for anyone who waits on the lock: its log and status are
// complete by then.
func (console *followedConsole) close() {
	if console.output != nil {
		_ = console.output.Close()
	}
	if console.log != nil {
		_ = console.log.Close()
	}
	_ = console.lock.Close()
}

// copyPushedConsole copies a pushed console's frames to its log and records
// its wait status, until the Sentry ends the push or it is stopped.
func copyPushedConsole(console *followedConsole) error {
	reader := bufio.NewReaderSize(console.output, 64<<10)
	eof := map[byte]bool{}
	for {
		header := make([]byte, ttcconsole.HeaderSize)
		if _, err := io.ReadFull(reader, header); err != nil {
			if errors.Is(err, io.EOF) {
				// The Sentry is gone without having ended the push: end the
				// log's streams, as the process has.
				for _, stream := range []byte{ttcconsole.StreamStdout, ttcconsole.StreamStderr} {
					if !eof[stream] {
						if err := ttcconsole.WriteFrame(console.log, stream, nil); err != nil {
							return err
						}
					}
				}
				return errors.New("the Sentry stopped pushing the console before its process ended")
			}
			return err
		}
		size := int(binary.BigEndian.Uint32(header[4:ttcconsole.HeaderSize]))
		if size > ttcconsole.MaxPayload+8 {
			return fmt.Errorf("pushed console frame of %d bytes", size)
		}
		frame := make([]byte, ttcconsole.HeaderSize+size)
		copy(frame, header)
		if _, err := io.ReadFull(reader, frame[ttcconsole.HeaderSize:]); err != nil {
			return fmt.Errorf("reading pushed console frame: %w", err)
		}
		switch stream := header[0]; stream {
		case boot.ConsolePushExit:
			if size != 4 {
				return fmt.Errorf("pushed wait status of %d bytes", size)
			}
			return ttcconsole.WriteWaitStatus(console.follow.WaitStatus, binary.BigEndian.Uint32(frame[ttcconsole.HeaderSize:]))
		case boot.ConsolePushError:
			return errors.New(string(frame[ttcconsole.HeaderSize:]))
		default:
			decoded, _, complete, err := ttcconsole.DecodeFrame(frame)
			if err != nil {
				return fmt.Errorf("decoding pushed console frame: %w", err)
			}
			if !complete {
				return errors.New("incomplete pushed console frame")
			}
			// One write keeps the frame in one log generation.
			if _, err := console.log.Write(frame); err != nil {
				return fmt.Errorf("writing console log: %w", err)
			}
			if decoded.EOF {
				eof[stream] = true
			}
		}
	}
}

// recordFollowError appends why following a console failed to its error file.
func recordFollowError(path string, cause error) {
	if path == "" {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(file, "following console: %v\n", cause)
	_ = file.Close()
}

// stop stops following the console whose lock is key, or all consoles if key
// is empty, and waits until each is released.
func (f *familyFollower) stop(key string) {
	f.mu.Lock()
	var consoles []*followedConsole
	for lock, console := range f.consoles {
		if key == "" || lock == key {
			consoles = append(consoles, console)
		}
	}
	f.mu.Unlock()
	for _, console := range consoles {
		// Closing the pipe ends the copy, and the Sentry's next write.
		_ = console.output.Close()
		<-console.done
	}
}

// release releases a console and, if it was being copied, tells every
// connection that it is no longer followed. The console leaves the follower
// first: a daemon that takes the lock once it is released may follow it anew.
func (f *familyFollower) release(console *followedConsole, notify bool) {
	f.mu.Lock()
	if f.consoles[console.follow.Lock] == console {
		delete(f.consoles, console.follow.Lock)
	}
	if len(f.consoles) == 0 && !f.exiting {
		f.idle.Reset(familyFollowerIdle)
	}
	conns := make([]*net.UnixConn, 0, len(f.conns))
	for conn := range f.conns {
		conns = append(conns, conn)
	}
	f.mu.Unlock()
	console.close()
	close(console.done)
	if notify {
		for _, conn := range conns {
			_ = ttcconsole.WriteFollowerMessage(conn, ttcconsole.FollowerMessage{Done: console.follow.Lock})
		}
	}
}
