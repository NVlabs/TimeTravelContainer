// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package fs

import (
	"testing"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/fdbudget"
)

// BenchmarkAcquireRelease measures what every gofer operation now pays.
func BenchmarkAcquireRelease(b *testing.B) {
	fds := make([]int, 2)
	if err := unix.Pipe(fds); err != nil {
		b.Fatal(err)
	}
	defer unix.Close(fds[1])
	fd := &controlFDLisa{hostFD: fds[0]}
	fd.budgeted.pinned = true
	for i := 0; i < b.N; i++ {
		if err := fd.acquire(); err != nil {
			b.Fatal(err)
		}
		fd.release()
	}
	unix.Close(fds[0])
}

// BenchmarkRegisterClosed measures what a control FD's life now costs.
func BenchmarkRegisterClosed(b *testing.B) {
	var e fdbudget.Entry
	fd := &controlFDLisa{hostFD: 3}
	for i := 0; i < b.N; i++ {
		fdbudget.Opened(&e, fd, 3)
		fdbudget.Closed(&e)
	}
}
