// Copyright 2018 The gVisor Authors.
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

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pgalloc

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
)

// Preconditions: The FileRange represented by c is a superset of fr.
func (c *chunkInfo) sliceAt(fr memmap.FileRange) []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(c.mapping+uintptr(fr.Start&chunkMask))), fr.Length())
}

func blockSeqFromBackingFrames(fr memmap.FileRange, alignedStart uint64, frames []BackingFrame) (safemem.BlockSeq, error) {
	want := (fr.End + uint64(hostarch.PageSize-1) - alignedStart) / uint64(hostarch.PageSize)
	if uint64(len(frames)) != want {
		return safemem.BlockSeq{}, fmt.Errorf("platform backing returned %d frames for mapping %v, want %d", len(frames), fr, want)
	}
	blocks := make([]safemem.Block, 0, len(frames))
	for i, frame := range frames {
		frameStart := alignedStart + uint64(i)*uint64(hostarch.PageSize)
		start := max(fr.Start, frameStart)
		end := min(fr.End, frameStart+uint64(hostarch.PageSize))
		address := frame.HostAddress + uintptr(start-frameStart)
		bytes := unsafe.Slice((*byte)(unsafe.Pointer(address)), end-start)
		blocks = append(blocks, safemem.BlockFromSafeSlice(bytes))
	}
	return safemem.BlockSeqFromSlice(blocks), nil
}

func mincore(s []byte, buf []byte) error {
	if _, _, errno := unix.RawSyscall(
		unix.SYS_MINCORE,
		uintptr(unsafe.Pointer(&s[0])),
		uintptr(len(s)),
		uintptr(unsafe.Pointer(&buf[0]))); errno != 0 {
		return errno
	}
	return nil
}

func canMergeIovecAndSlice(iov unix.Iovec, bs []byte) bool {
	return uintptr(unsafe.Pointer(iov.Base))+uintptr(iov.Len) == uintptr(unsafe.Pointer(unsafe.SliceData(bs)))
}
