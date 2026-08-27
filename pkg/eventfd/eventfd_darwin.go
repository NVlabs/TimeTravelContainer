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

package eventfd

import (
	"fmt"
	"io"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
)

// Create returns a local event notification pipe. The Darwin adapter keeps
// the read and write descriptors together; protocols that transfer Eventfd.FD
// alone must use a Darwin-specific two-FD transport instead.
func Create() (Eventfd, error) {
	var fds [2]int
	if err := unix.Pipe(fds[:]); err != nil {
		return Eventfd{}, fmt.Errorf("creating event pipe: %w", err)
	}
	unix.CloseOnExec(fds[0])
	unix.CloseOnExec(fds[1])
	if err := unix.SetNonblock(fds[1], true); err != nil {
		_ = unix.Close(fds[0])
		_ = unix.Close(fds[1])
		return Eventfd{}, err
	}
	return Eventfd{fd: fds[0], writeFD: fds[1]}, nil
}

// Read blocks until a complete notification value is available.
func (ev Eventfd) Read() (uint64, error) {
	var tmp [sizeofUint64]byte
	read := 0
	for read != len(tmp) {
		n, err := unix.Read(ev.fd, tmp[read:])
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, io.EOF
		}
		read += n
	}
	return hostarch.ByteOrder.Uint64(tmp[:]), nil
}
