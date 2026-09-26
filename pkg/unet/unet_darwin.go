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

package unet

import (
	"io"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Darwin rejects a nonblocking AF_UNIX stream sendmsg whose payload exceeds
// the socket's current send high-water mark with EMSGSIZE instead of accepting
// a partial write. Keep each send below the minimum high-water mark used by
// supported macOS releases. SocketWriter callers already retry partial writes.
const maxDarwinSendmsgPayload = 4096

func limitDarwinSendmsgBuffers(bufs [][]byte) [][]byte {
	remaining := maxDarwinSendmsgPayload
	limited := make([][]byte, 0, len(bufs))
	for _, buf := range bufs {
		if len(buf) == 0 {
			continue
		}
		if len(buf) > remaining {
			limited = append(limited, buf[:remaining])
			return limited
		}
		limited = append(limited, buf)
		remaining -= len(buf)
		if remaining == 0 {
			return limited
		}
	}
	// Preserve an empty iovec list for control-message-only sends.
	if len(limited) == 0 {
		return bufs
	}
	return limited
}

func (s *Socket) wait(write bool) error {
	for {
		fd := s.fd.Load()
		if fd < 0 {
			return errClosing
		}
		events := []unix.PollFd{
			{Fd: fd, Events: unix.POLLIN},
			{Fd: int32(s.efd.FD()), Events: unix.POLLIN},
		}
		if write {
			events[0].Events = unix.POLLOUT
		}
		if _, err := unix.Poll(events, -1); err == unix.EINTR {
			continue
		} else if err != nil {
			return err
		}
		if events[1].Revents&unix.POLLIN != 0 {
			return errClosing
		}
		return nil
	}
}

func (r *SocketReader) ReadVec(bufs [][]byte) (int, error) {
	length := 0
	for _, buf := range bufs {
		length += len(buf)
	}
	fd, ok := r.socket.enterFD()
	if !ok {
		return 0, unix.EBADF
	}
	defer r.socket.gate.Leave()
	fdCapacity := controlFDCapacity(len(r.ControlMessage))
	// Darwin may install SCM_RIGHTS descriptors even when the caller's
	// ancillary buffer is too small, without closing the truncated tail. Use
	// the platform maximum so we can explicitly close everything not exposed
	// to the caller.
	recvControl := make([]byte, unix.CmsgSpace(253*4))
	for {
		// Darwin has no MSG_CMSG_CLOEXEC. Hold ForkLock from the nonblocking
		// receive until retainReceivedFDs makes the descriptors close-on-exec,
		// so a concurrent fork cannot inherit them.
		syscall.ForkLock.RLock()
		n, oobn, recvFlags, _, err := unix.RecvmsgBuffers(fd, bufs, recvControl, unix.MSG_DONTWAIT|unix.MSG_TRUNC)
		var retainErr error
		if err == nil {
			retainErr = r.retainReceivedFDs(recvControl[:oobn], fdCapacity, recvFlags)
		}
		syscall.ForkLock.RUnlock()
		if err == nil {
			if retainErr != nil {
				return 0, retainErr
			}
			if n == 0 {
				return 0, io.EOF
			}
			if r.race != nil {
				r.race.Add(1)
			}
			if n > length {
				return length, errMessageTruncated
			}
			return n, nil
		}
		if err == unix.EINTR {
			continue
		}
		if !r.blocking || (err != unix.EAGAIN && err != unix.EWOULDBLOCK) {
			return 0, err
		}
		if err := r.socket.wait(false); err != nil {
			if err == errClosing {
				return 0, unix.EBADF
			}
			return 0, err
		}
	}
}

func controlFDCapacity(controlBytes int) int {
	capacity := 0
	for count := 1; count <= 253 && unix.CmsgSpace(count*4) <= controlBytes; count++ {
		capacity = count
	}
	return capacity
}

func (r *SocketReader) retainReceivedFDs(control []byte, capacity int, recvFlags int) error {
	r.ControlMessage = nil
	if len(control) == 0 {
		return nil
	}
	messages, err := unix.ParseSocketControlMessage(control)
	if err != nil {
		return err
	}
	var received []int
	for _, message := range messages {
		fds, err := unix.ParseUnixRights(&message)
		if err != nil {
			for _, fd := range received {
				unix.Close(fd)
			}
			return err
		}
		received = append(received, fds...)
	}
	for _, fd := range received {
		unix.CloseOnExec(fd)
	}
	keep := len(received)
	if keep > capacity {
		keep = capacity
	}
	for _, fd := range received[keep:] {
		unix.Close(fd)
	}
	if keep != 0 {
		r.ControlMessage = ControlMessage(unix.UnixRights(received[:keep]...))
	}
	if recvFlags&unix.MSG_CTRUNC != 0 {
		for _, fd := range received[:keep] {
			unix.Close(fd)
		}
		r.ControlMessage = nil
		return errMessageTruncated
	}
	return nil
}

func (w *SocketWriter) WriteVec(bufs [][]byte) (int, error) {
	if w.race != nil {
		w.race.Add(1)
	}
	fd, ok := w.socket.enterFD()
	if !ok {
		return 0, unix.EBADF
	}
	defer w.socket.gate.Leave()
	bufs = limitDarwinSendmsgBuffers(bufs)
	for {
		n, err := unix.SendmsgBuffers(fd, bufs, w.ControlMessage, nil, unix.MSG_DONTWAIT|unix.MSG_NOSIGNAL)
		if err == nil {
			return n, nil
		}
		if err == unix.EINTR {
			continue
		}
		// Darwin may report EMSGSIZE, rather than EAGAIN, when a stream
		// socket's remaining send space cannot hold this bounded chunk.
		if !w.blocking || (err != unix.EAGAIN && err != unix.EWOULDBLOCK && err != unix.EMSGSIZE) {
			return 0, err
		}
		if err := w.socket.wait(true); err != nil {
			if err == errClosing {
				return 0, unix.EBADF
			}
			return 0, err
		}
	}
}

func getsockopt(fd int, level int, optname int, buf []byte) (uint32, error) {
	length := uint32(len(buf))
	if len(buf) == 0 {
		return 0, unix.EINVAL
	}
	_, _, errno := unix.RawSyscall6(unix.SYS_GETSOCKOPT, uintptr(fd), uintptr(level), uintptr(optname), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&length)), 0)
	if errno != 0 {
		return 0, errno
	}
	return length, nil
}

func setsockopt(fd int, level int, optname int, buf []byte) error {
	if len(buf) == 0 {
		return unix.EINVAL
	}
	_, _, errno := unix.RawSyscall6(unix.SYS_SETSOCKOPT, uintptr(fd), uintptr(level), uintptr(optname), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func getsockname(fd int, buf []byte) (uint32, error) {
	length := uint32(len(buf))
	_, _, errno := unix.RawSyscall(unix.SYS_GETSOCKNAME, uintptr(fd), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&length)))
	if errno != 0 {
		return 0, errno
	}
	return length, nil
}

func getpeername(fd int, buf []byte) (uint32, error) {
	length := uint32(len(buf))
	_, _, errno := unix.RawSyscall(unix.SYS_GETPEERNAME, uintptr(fd), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&length)))
	if errno != 0 {
		return 0, errno
	}
	return length, nil
}
