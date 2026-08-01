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
	"net"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

type donatedTCPAddr string

func (donatedTCPAddr) Network() string  { return "tcp" }
func (a donatedTCPAddr) String() string { return string(a) }

type donatedTCPConn struct {
	file *os.File
}

func (c *donatedTCPConn) Read(dst []byte) (int, error)   { return c.file.Read(dst) }
func (c *donatedTCPConn) Write(src []byte) (int, error)  { return c.file.Write(src) }
func (c *donatedTCPConn) Close() error                   { return c.file.Close() }
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
	duplicate := os.NewFile(uintptr(fd), "ttc-donated-tcp")
	if duplicate == nil {
		unix.Close(fd)
		return nil, errors.New("wrapping donated TCP descriptor")
	}
	return &donatedTCPConn{file: duplicate}, nil
}
