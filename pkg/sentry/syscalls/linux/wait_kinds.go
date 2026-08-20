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

package linux

import (
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/devpts"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/eventfd"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/timerfd"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/pipe"
	"gvisor.dev/gvisor/pkg/sentry/socket"
	unixsocket "gvisor.dev/gvisor/pkg/sentry/socket/unix"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

// Kinds of thing a task can be waiting on, reported alongside the descriptor
// numbers in a task's wait information.
//
// The distinction that matters to an observer is not what the descriptor is
// called but who can make it ready. A terminal becomes ready when a person
// types. A self-pipe or an eventfd becomes ready only when a thread of this
// same process writes to it, so if no such thread can run, it never will. A
// socket becomes ready because the far end acted, entirely on its own schedule.
// The first two are compatible with a program that has finished its work; the
// last is not.
const (
	waitKindTerminal  = "tty"
	waitKindPTYMaster = "pty-master"
	waitKindEventFD   = "eventfd"
	waitKindSelfPipe  = "self-pipe"
	waitKindPipe      = "pipe"
	waitKindTimerFD   = "timerfd"
	waitKindEpoll     = "epoll"
	waitKindSocket    = "socket"
	// waitKindUnixSocket is a unix-domain socket whose peer is inside this
	// sandbox: an X client's display connection, a tool's helper process.
	//
	// Worth telling apart from a network socket, because the two differ in who
	// can make them ready. A socket to a licence server is the outside world,
	// acting on its own schedule, and a program waiting on one has work in
	// flight. A unix socket ends at another process in this sandbox — one an
	// observer can, in principle, see the state of — so it is the same kind of
	// claim as a pipe: not proof of rest, but not evidence of work either.
	waitKindUnixSocket = "unix-socket"
	// waitKindSocketLoopback is a socket connected to this sandbox's own
	// address: the peer is a process in here, so it is the same sort of claim
	// as a pipe rather than the outside world.
	waitKindSocketLoopback = "socket-loopback"
	// waitKindSocketListening is a socket with no peer at all, waiting for a
	// connection nobody has made. Nothing is in flight and nothing is at rest;
	// it is a program holding a door open.
	waitKindSocketListening = "socket-listening"
	waitKindOther           = "other"
	waitKindClosed          = "closed"
)

// classifyWaitFDs names what each awaited descriptor is.
//
// files, when non-nil, are the descriptions the numbers refer to and must be
// in the same order — epoll holds them already, so re-resolving the numbers
// would be both wasteful and wrong, since a number can be reused after the
// registration that named it. poll and select pass nil and are resolved here.
//
// matchSelfPipes asks whether a pipe should be checked for a local write end,
// which costs a walk of the descriptor table. Worth it for a multi-descriptor
// wait, where a runtime's wakeup pipe is the difference between a provable rest
// and an unprovable one, and not worth it for a single blocking read, which is
// hot and where a pipe is external either way.
func classifyWaitFDs(t *kernel.Task, nums []int32, files []*vfs.FileDescription, matchSelfPipes bool) []string {
	if len(nums) == 0 {
		return nil
	}
	kinds := make([]string, len(nums))
	var resolved []*vfs.FileDescription
	if files == nil {
		files = make([]*vfs.FileDescription, len(nums))
		for i, num := range nums {
			file, _ := t.FDTable().Get(num)
			if file == nil {
				continue
			}
			files[i] = file
			resolved = append(resolved, file)
		}
		defer func() {
			for _, file := range resolved {
				file.DecRef(t)
			}
		}()
	}

	// Only pay for the descriptor-table walk when there is a pipe to explain.
	var writable map[*vfs.Dentry]bool
	for i, file := range files {
		if file == nil {
			kinds[i] = waitKindClosed
			continue
		}
		switch impl := file.Impl().(type) {
		case *eventfd.EventFileDescription:
			kinds[i] = waitKindEventFD
		case *timerfd.TimerFileDescription:
			kinds[i] = waitKindTimerFD
		case *vfs.EpollInstance:
			kinds[i] = waitKindEpoll
		case *pipe.VFSPipeFD:
			if !matchSelfPipes {
				kinds[i] = waitKindPipe
				continue
			}
			if writable == nil {
				writable = writablePipeEnds(t)
			}
			if writable[file.Dentry()] {
				kinds[i] = waitKindSelfPipe
			} else {
				kinds[i] = waitKindPipe
			}
		default:
			switch {
			case devpts.TerminalFDKind(file) == waitKindTerminal:
				kinds[i] = waitKindTerminal
			case devpts.TerminalFDKind(file) == waitKindPTYMaster:
				kinds[i] = waitKindPTYMaster
			default:
				if _, ok := impl.(*unixsocket.Socket); ok {
					kinds[i] = waitKindUnixSocket
				} else if sock, ok := impl.(socket.Socket); ok {
					kinds[i] = classifySocket(t, sock)
				} else {
					kinds[i] = waitKindOther
				}
			}
		}
	}
	return kinds
}

// writablePipeEnds collects the pipes this process can itself write to.
//
// A pipe the process holds both ends of is a wakeup channel: libuv builds one
// per event loop so that a signal handler or a worker thread can interrupt the
// loop. Nothing outside the process can make it ready. A pipe with no local
// write end is the opposite — it is fed by someone else, on their schedule, and
// waiting on it is waiting for the outside world.
func writablePipeEnds(t *kernel.Task) map[*vfs.Dentry]bool {
	ends := make(map[*vfs.Dentry]bool)
	fdt := t.FDTable()
	if fdt == nil {
		return ends
	}
	for _, num := range fdt.GetFDs(t) {
		file, _ := fdt.Get(num)
		if file == nil {
			continue
		}
		if _, ok := file.Impl().(*pipe.VFSPipeFD); ok && file.IsWritable() {
			ends[file.Dentry()] = true
		}
		file.DecRef(t)
	}
	return ends
}

// classifySocket says who is on the other end of a network socket.
//
// Three answers, and they are three different things to a caller waiting for a
// turn to end. A connected socket to somewhere else is work in flight. A
// connected socket to this sandbox's own address is a peer an observer could in
// principle account for, like a pipe. A socket with no peer at all is waiting
// for a connection that may never come.
func classifySocket(t *kernel.Task, sock socket.Socket) string {
	addr, _, err := sock.GetPeerName(t)
	if err != nil || addr == nil {
		return waitKindSocketListening
	}
	switch peer := addr.(type) {
	case *linux.SockAddrInet:
		if peer.Addr[0] == 127 {
			return waitKindSocketLoopback
		}
	case *linux.SockAddrInet6:
		loopback := true
		for i, b := range peer.Addr {
			if (i < 15 && b != 0) || (i == 15 && b != 1) {
				loopback = false
				break
			}
		}
		if loopback {
			return waitKindSocketLoopback
		}
	}
	return waitKindSocket
}
