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

package hostfd

import (
	"io"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/safemem"
)

const (
	MaxReadWriteIov   = 1024
	MaxSendRecvMsgIov = 1024
)

func blockSlices(blocks safemem.BlockSeq, limit int) ([][]byte, uint64) {
	slices := make([][]byte, 0, min(blocks.NumBlocks(), limit))
	var bytes uint64
	for !blocks.IsEmpty() && len(slices) < limit {
		block := blocks.Head()
		slices = append(slices, block.ToSlice())
		bytes += uint64(block.Len())
		blocks = blocks.Tail()
	}
	return slices, bytes
}

// Preadv2 emulates flags-zero Linux preadv2 using Darwin readv/preadv.
func Preadv2(fd int32, dsts safemem.BlockSeq, offset int64, flags uint32) (uint64, error) {
	if flags != 0 {
		return 0, unix.EOPNOTSUPP
	}
	var total uint64
	for !dsts.IsEmpty() {
		slices, requested := blockSlices(dsts, MaxReadWriteIov)
		var (
			n   int
			err error
		)
		if offset < 0 {
			n, err = unix.Readv(int(fd), slices)
		} else {
			n, err = unix.Preadv(int(fd), slices, offset+int64(total))
		}
		if n > 0 {
			total += uint64(n)
			dsts = dsts.DropFirst(n)
		}
		if err != nil {
			return total, err
		}
		if n == 0 {
			if total == 0 {
				return 0, io.EOF
			}
			return total, nil
		}
		if uint64(n) < requested {
			return total, nil
		}
	}
	return total, nil
}

// Pwritev2 emulates flags-zero Linux pwritev2 using Darwin writev/pwritev.
func Pwritev2(fd int32, srcs safemem.BlockSeq, offset int64, flags uint32) (uint64, error) {
	if flags != 0 {
		return 0, unix.EOPNOTSUPP
	}
	var total uint64
	for !srcs.IsEmpty() {
		slices, requested := blockSlices(srcs, MaxReadWriteIov)
		var (
			n   int
			err error
		)
		if offset < 0 {
			n, err = unix.Writev(int(fd), slices)
		} else {
			n, err = unix.Pwritev(int(fd), slices, offset+int64(total))
		}
		if n > 0 {
			total += uint64(n)
			srcs = srcs.DropFirst(n)
		}
		if err != nil {
			return total, err
		}
		if uint64(n) < requested {
			return total, nil
		}
	}
	return total, nil
}
