// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package p9

import "syscall"

func fillHostStatAttr(attr *Attr, stat *syscall.Stat_t, req AttrMask) {
	if req.RDev {
		attr.RDev = stat.Rdev
	}
	if req.ATime {
		attr.ATimeSeconds = uint64(stat.Atim.Sec)
		attr.ATimeNanoSeconds = uint64(stat.Atim.Nsec)
	}
	if req.MTime {
		attr.MTimeSeconds = uint64(stat.Mtim.Sec)
		attr.MTimeNanoSeconds = uint64(stat.Mtim.Nsec)
	}
	if req.CTime {
		attr.CTimeSeconds = uint64(stat.Ctim.Sec)
		attr.CTimeNanoSeconds = uint64(stat.Ctim.Nsec)
	}
}
