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

//go:build linux

package cluster

import (
	"os"

	"golang.org/x/sys/unix"
)

// openShared opens path with direct I/O, or without it where the filesystem
// does not allow it.
func openShared(path string, flags int) (int, error) {
	fd, err := unix.Open(path, flags|unix.O_DIRECT|unix.O_CLOEXEC, 0600)
	if err == unix.EINVAL {
		fd, err = unix.Open(path, flags|unix.O_CLOEXEC, 0600)
	}
	if err != nil {
		return -1, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return fd, nil
}
