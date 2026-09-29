// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package host

import (
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
)

func setTimestamps(fd int, atime linux.StatxTimestamp, omitAtime bool, mtime linux.StatxTimestamp, omitMtime bool) error {
	if omitAtime || omitMtime {
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			return err
		}
		if omitAtime {
			atime = linux.StatxTimestamp{Sec: st.Atim.Sec, Nsec: uint32(st.Atim.Nsec)}
		}
		if omitMtime {
			mtime = linux.StatxTimestamp{Sec: st.Mtim.Sec, Nsec: uint32(st.Mtim.Nsec)}
		}
	}
	// futimes is the baseline Darwin FD-relative interface. APFS timestamps
	// retain microsecond precision through this compatibility path.
	return unix.Futimes(fd, []unix.Timeval{
		{Sec: atime.Sec, Usec: int32(atime.Nsec / 1000)},
		{Sec: mtime.Sec, Usec: int32(mtime.Nsec / 1000)},
	})
}
