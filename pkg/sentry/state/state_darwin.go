// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package state

import (
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// CPUTime returns process user plus system CPU time. Darwin does not expose
// Linux's clock_gettime syscall number through x/sys, but getrusage provides
// the same process-CPU accounting needed for checkpoint metrics.
func CPUTime() (time.Duration, error) {
	var usage unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &usage); err != nil {
		return 0, fmt.Errorf("getrusage(RUSAGE_SELF): %w", err)
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano()), nil
}
