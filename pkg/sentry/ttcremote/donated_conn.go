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

package ttcremote

import (
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type donatedTCPAddr string

func (donatedTCPAddr) Network() string  { return "tcp" }
func (a donatedTCPAddr) String() string { return string(a) }

type donatedTCPConn struct {
	file      *os.File
	closeOnce sync.Once
	closeErr  error
}

func (c *donatedTCPConn) Read(dst []byte) (int, error) {
	for {
		n, err := c.file.Read(dst)
		if n != 0 || !isWouldBlock(err) {
			return n, err
		}
		if err := waitDonatedFD(int(c.file.Fd()), unix.POLLIN); err != nil {
			return 0, err
		}
	}
}

func (c *donatedTCPConn) Write(src []byte) (int, error) {
	for {
		n, err := c.file.Write(src)
		if n != 0 || !isWouldBlock(err) {
			return n, err
		}
		if err := waitDonatedFD(int(c.file.Fd()), unix.POLLOUT); err != nil {
			return 0, err
		}
	}
}

func isWouldBlock(err error) bool {
	return errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK)
}

// waitDonatedFD restores blocking net.Conn semantics without changing the
// donated open file description. The descriptor may have O_NONBLOCK set by
// Go's network poller before it crosses the Sentry boundary, and dup(2)
// intentionally preserves that status flag. Changing it with fcntl would also
// change every duplicate and is unavailable under the isolated Sentry's
// syscall policy.
func waitDonatedFD(fd int, events int16) error {
	pollFDs := []unix.PollFd{{Fd: int32(fd), Events: events}}
	for {
		_, err := unix.Poll(pollFDs, -1)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return err
	}
}

func (c *donatedTCPConn) Close() error {
	c.closeOnce.Do(func() {
		// close(2) alone does not reliably wake poll(2) in another thread on
		// Linux. Shutdown first so a server session can synchronously wait for
		// its donated stream goroutine without deadlocking on an idle socket.
		fd := int(c.file.Fd())
		if err := unix.Shutdown(fd, unix.SHUT_RDWR); err != nil && !errors.Is(err, unix.ENOTCONN) && !errors.Is(err, unix.EBADF) {
			c.closeErr = err
		}
		if err := c.file.Close(); c.closeErr == nil {
			c.closeErr = err
		}
	})
	return c.closeErr
}
func (*donatedTCPConn) LocalAddr() net.Addr              { return donatedTCPAddr("donated-local") }
func (*donatedTCPConn) RemoteAddr() net.Addr             { return donatedTCPAddr("donated-remote") }
func (*donatedTCPConn) SetDeadline(time.Time) error      { return nil }
func (*donatedTCPConn) SetReadDeadline(time.Time) error  { return nil }
func (*donatedTCPConn) SetWriteDeadline(time.Time) error { return nil }

// NewDonatedTCPConn duplicates file and wraps it without getsockopt, fcntl,
// or other host socket discovery. This is suitable for an isolated Sentry
// whose seccomp policy permits byte I/O on donated descriptors but not host
// networking operations.
func NewDonatedTCPConn(file *os.File) (net.Conn, error) {
	if file == nil {
		return nil, errors.New("nil donated TCP file")
	}
	fd, err := unix.Dup(int(file.Fd()))
	if err != nil {
		return nil, err
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, unix.FD_CLOEXEC); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("setting donated TCP descriptor close-on-exec: %w", err)
	}
	duplicate := os.NewFile(uintptr(fd), "ttc-donated-tcp")
	if duplicate == nil {
		unix.Close(fd)
		return nil, errors.New("wrapping donated TCP descriptor")
	}
	return &donatedTCPConn{file: duplicate}, nil
}
