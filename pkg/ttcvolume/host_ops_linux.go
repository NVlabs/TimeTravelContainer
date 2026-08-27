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

package ttcvolume

import (
	"os"

	"golang.org/x/sys/unix"
)

func cloneFile(source, destination *os.File, _ int, _ string) (*os.File, error) {
	if err := unix.IoctlFileClone(int(destination.Fd()), int(source.Fd())); err != nil {
		return destination, err
	}
	return destination, nil
}

func syncFileData(file *os.File) error {
	return unix.Fdatasync(int(file.Fd()))
}

func allocateFile(file *os.File, offset, length int64) error {
	return unix.Fallocate(int(file.Fd()), 0, offset, length)
}
