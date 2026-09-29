// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package images

import (
	"errors"

	"golang.org/x/sys/unix"
)

func isMissingXattr(err error) bool {
	return errors.Is(err, unix.ENODATA)
}
