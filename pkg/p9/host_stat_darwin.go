// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build darwin

package p9

import "syscall"

func fillHostStatAttr(attr *Attr, stat *syscall.Stat_t, req AttrMask) {
	if req.RDev {
		attr.RDev = uint64(stat.Dev)
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
