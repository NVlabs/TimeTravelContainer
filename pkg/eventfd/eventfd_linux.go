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

//go:build linux

package eventfd

import (
	"fmt"
	"io"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/rawfile"
)

// Create returns an initialized Linux eventfd.
func Create() (Eventfd, error) {
	fd, _, errno := unix.RawSyscall(unix.SYS_EVENTFD2, 0, 0, 0)
	if errno != 0 {
		return Eventfd{}, fmt.Errorf("failed to create eventfd: %v", error(errno))
	}
	if err := unix.SetNonblock(int(fd), true); err != nil {
		unix.Close(int(fd))
		return Eventfd{}, err
	}
	return Eventfd{fd: int(fd), writeFD: int(fd)}, nil
}

// Read blocks until eventfd is non-zero and returns its counter.
func (ev Eventfd) Read() (uint64, error) {
	var tmp [sizeofUint64]byte
	n, errno := rawfile.BlockingRead(ev.fd, tmp[:])
	if errno != 0 {
		return 0, errno
	}
	if n == 0 {
		return 0, io.EOF
	}
	if n != sizeofUint64 {
		panic(fmt.Sprintf("short read from eventfd: got %d bytes, wanted %d", n, sizeofUint64))
	}
	return hostarch.ByteOrder.Uint64(tmp[:]), nil
}
