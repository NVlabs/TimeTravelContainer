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

package memutil

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// CreateMemFD emulates Linux memfd_create with an unlinked temporary file.
// The returned descriptor is the caller's responsibility.
func CreateMemFD(name string, flags int) (int, error) {
	file, err := os.CreateTemp("", "gvisor_"+name+"_")
	if err != nil {
		return -1, fmt.Errorf("creating anonymous backing file: %w", err)
	}
	path := file.Name()
	fd, dupErr := unix.Dup(int(file.Fd()))
	closeErr := file.Close()
	removeErr := os.Remove(path)
	if dupErr != nil {
		return -1, fmt.Errorf("duplicating anonymous backing file: %w", dupErr)
	}
	if closeErr != nil {
		unix.Close(fd)
		return -1, closeErr
	}
	if removeErr != nil {
		unix.Close(fd)
		return -1, removeErr
	}
	// Linux MFD_CLOEXEC is bit 0. Darwin cannot emulate seals, but it can
	// preserve descriptor inheritance semantics.
	if flags&1 != 0 {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, unix.FD_CLOEXEC); err != nil {
			unix.Close(fd)
			return -1, err
		}
	}
	return fd, nil
}
