// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// StateFileHeld reports whether a runtime command holds the state file of
// container id under rootDir, as one creating or deleting it does, without
// waiting for it. A container without a lock file is not held: unlike
// StateFile's lock, the probe never creates one, so it leaves none behind for
// a container deleted under it.
func StateFileHeld(rootDir string, id FullID) (bool, error) {
	s := StateFile{RootDir: rootDir, ID: id}
	path := s.lockPath()
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("opening lock file %q: %w", path, err)
	}
	defer f.Close()
	// StateFile's lock is flock(2)'s, exclusive.
	switch err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); {
	case err == unix.EWOULDBLOCK:
		return true, nil
	case err != nil:
		return false, fmt.Errorf("probing lock on %q: %w", path, err)
	}
	return false, unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
