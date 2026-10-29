// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pinring

import (
	"errors"
	"os"
)

var errNoIOURing = errors.New("macOS has no io_uring")

// NewDisabledIOURing reports that there is no ring to create: macOS has no
// io_uring, so its sandboxes release files synchronously.
func NewDisabledIOURing() (*os.File, error) {
	return nil, errNoIOURing
}

// registerFiles is never reached on macOS, where no ring exists.
func registerFiles(ring int, fds []int32) error {
	return errNoIOURing
}
