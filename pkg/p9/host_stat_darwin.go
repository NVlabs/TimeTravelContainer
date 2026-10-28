// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package p9

import "syscall"

func fillHostStatAttr(attr *Attr, stat *syscall.Stat_t, req AttrMask) {
	if req.RDev {
		attr.RDev = uint64(stat.Rdev)
	}
	if req.ATime {
		attr.ATimeSeconds = uint64(stat.Atimespec.Sec)
		attr.ATimeNanoSeconds = uint64(stat.Atimespec.Nsec)
	}
	if req.MTime {
		attr.MTimeSeconds = uint64(stat.Mtimespec.Sec)
		attr.MTimeNanoSeconds = uint64(stat.Mtimespec.Nsec)
	}
	if req.CTime {
		attr.CTimeSeconds = uint64(stat.Ctimespec.Sec)
		attr.CTimeNanoSeconds = uint64(stat.Ctimespec.Nsec)
	}
}
