// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build darwin

package cluster

import (
	"os"

	"golang.org/x/sys/unix"
)

// openShared opens path uncached, with F_NOCACHE, Darwin's direct I/O, or
// cached where the filesystem does not allow it.
func openShared(path string, flags int) (int, error) {
	fd, err := unix.Open(path, flags|unix.O_CLOEXEC, 0600)
	if err != nil {
		return -1, &os.PathError{Op: "open", Path: path, Err: err}
	}
	_, _ = unix.FcntlInt(uintptr(fd), unix.F_NOCACHE, 1)
	return fd, nil
}
