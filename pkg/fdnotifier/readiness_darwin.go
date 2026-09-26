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
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/waiter"
)

// fdKind classifies a host FD for translating end-of-file conditions.
type fdKind uint8

const (
	kindOther fdKind = iota
	kindSocket
	kindFIFO
)

// fdClass describes a host FD for translating end-of-file conditions.
type fdClass struct {
	kind      fdKind
	readable  bool
	writeable bool
}

func classifyFD(fd int32) fdClass {
	class := fdClass{readable: true, writeable: true}
	var st unix.Stat_t
	if err := unix.Fstat(int(fd), &st); err == nil {
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFSOCK:
			class.kind = kindSocket
		case unix.S_IFIFO:
			class.kind = kindFIFO
		}
	}
	if flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0); err == nil {
		switch flags & unix.O_ACCMODE {
		case unix.O_RDONLY:
			class.writeable = false
		case unix.O_WRONLY:
			class.readable = false
		}
	}
	return class
}

// eofEvents translates end-of-file conditions on the read and write sides of
// an FD, as Darwin reports them, into the events that Linux reports.
//
// Darwin reports EOF, and POLLHUP from poll(2), as soon as a socket's peer
// shuts down writing. Linux reports that half-close as POLLRDHUP, and POLLHUP
// only once both directions are shut down, so that the socket can still be
// written. For a pipe, Linux reports a missing writer to the reader as
// POLLHUP, and a missing reader to the writer as POLLERR.
func eofEvents(class fdClass, readEOF, writeEOF bool) waiter.EventMask {
	readEOF = readEOF && class.readable
	writeEOF = writeEOF && class.writeable
	var ready waiter.EventMask
	switch class.kind {
	case kindSocket:
		if readEOF {
			ready |= waiter.ReadableEvents | waiter.EventRdHUp
		}
		if writeEOF {
			// Writes fail immediately.
			ready |= waiter.WritableEvents
		}
		if readEOF && writeEOF {
			ready |= waiter.EventHUp
		}
	case kindFIFO:
		if readEOF {
			ready |= waiter.EventHUp
		}
		if writeEOF {
			ready |= waiter.WritableEvents | waiter.EventErr
		}
	default:
		if readEOF || writeEOF {
			ready |= waiter.EventHUp
		}
	}
	return ready
}

// pollDirections polls the read and write sides of fd separately, without
// blocking. A single pollfd would not do: when the read side reaches EOF,
// Darwin's poll(2) reports POLLHUP instead of POLLOUT even if the FD can still
// be written.
func pollDirections(fd int32) (read, write int16, err error) {
	fds := [2]unix.PollFd{
		{Fd: fd, Events: unix.POLLIN | unix.POLLPRI},
		{Fd: fd, Events: unix.POLLOUT},
	}
	for {
		if _, err := unix.Poll(fds[:], 0); err != unix.EINTR {
			return fds[0].Revents, fds[1].Revents, err
		}
	}
}
