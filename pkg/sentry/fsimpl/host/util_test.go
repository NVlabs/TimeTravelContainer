// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"testing"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
)

func TestIsBlockErrorAcceptsHostAndLinuxErrnos(t *testing.T) {
	for _, err := range []error{unix.EAGAIN, unix.EWOULDBLOCK, linuxerr.EAGAIN, linuxerr.EWOULDBLOCK} {
		if !isBlockError(err) {
			t.Errorf("isBlockError(%T(%v)) = false", err, err)
		}
	}
	for _, err := range []error{nil, unix.EINVAL, linuxerr.EINVAL} {
		if isBlockError(err) {
			t.Errorf("isBlockError(%T(%v)) = true", err, err)
		}
	}
}
