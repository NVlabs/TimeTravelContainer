// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package console

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// WriteWaitStatus atomically records a Linux wait status for a console
// follower. Both the follower and ttcd use this helper: a synchronous wait may
// observe process exit before the follower has installed its copy, but the two
// observations describe the same immutable terminal status.
func WriteWaitStatus(path string, status uint32) error {
	dir := filepath.Dir(path)
	temporary, err := os.CreateTemp(dir, ".wait-status-*")
	if err != nil {
		return fmt.Errorf("creating temporary console wait status in %q: %w", dir, err)
	}
	temporaryPath := temporary.Name()
	installed := false
	defer func() {
		_ = temporary.Close()
		if !installed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0600); err != nil {
		return fmt.Errorf("setting console wait status permissions: %w", err)
	}
	if _, err := temporary.WriteString(strconv.FormatUint(uint64(status), 10) + "\n"); err != nil {
		return fmt.Errorf("writing console wait status: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("closing console wait status: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("installing console wait status at %q: %w", path, err)
	}
	installed = true
	return nil
}

// ReadWaitStatus reads a status installed by WriteWaitStatus.
func ReadWaitStatus(path string) (uint32, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("reading console wait status: %w", err)
	}
	value := strings.TrimSpace(string(data))
	status, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return 0, false, fmt.Errorf("parsing console wait status %q: %w", value, err)
	}
	return uint32(status), true, nil
}
