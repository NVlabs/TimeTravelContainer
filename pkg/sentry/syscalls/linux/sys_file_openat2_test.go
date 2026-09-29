// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package linux

import (
	"testing"

	abi "gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
)

func TestParseOpenHow(t *testing.T) {
	buf := make([]byte, openHowSize)
	wantFlags := uint64(abi.O_PATH | abi.O_DIRECTORY | abi.O_NOFOLLOW | abi.O_CLOEXEC)
	hostarch.ByteOrder.PutUint64(buf[0:8], wantFlags)
	flags, mode, err := parseOpenHow(buf)
	if err != nil {
		t.Fatal(err)
	}
	if flags != uint32(wantFlags) || mode != 0 {
		t.Fatalf("parseOpenHow: got flags=%#x mode=%#x, want flags=%#x mode=0", flags, mode, wantFlags)
	}
}

func TestParseOpenHowRejectsUnsupportedOrMalformedExtensions(t *testing.T) {
	resolve := make([]byte, openHowSize)
	hostarch.ByteOrder.PutUint64(resolve[16:24], 1)
	unknownFlags := make([]byte, openHowSize)
	hostarch.ByteOrder.PutUint64(unknownFlags[0:8], 1<<63)
	modeWithoutCreate := make([]byte, openHowSize)
	hostarch.ByteOrder.PutUint64(modeWithoutCreate[8:16], 0600)
	invalidMode := make([]byte, openHowSize)
	hostarch.ByteOrder.PutUint64(invalidMode[0:8], abi.O_CREAT)
	hostarch.ByteOrder.PutUint64(invalidMode[8:16], 1<<32)
	tests := []struct {
		name string
		buf  []byte
		want error
	}{
		{name: "short", buf: make([]byte, openHowSize-1), want: linuxerr.EINVAL},
		{name: "nonzero extension", buf: append(make([]byte, openHowSize), 1), want: linuxerr.E2BIG},
		{name: "resolve restriction", buf: resolve, want: linuxerr.EINVAL},
		{name: "unknown flags", buf: unknownFlags, want: linuxerr.EINVAL},
		{name: "mode without create", buf: modeWithoutCreate, want: linuxerr.EINVAL},
		{name: "invalid mode", buf: invalidMode, want: linuxerr.EINVAL},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := parseOpenHow(test.buf); err != test.want {
				t.Fatalf("parseOpenHow error: got %v, want %v", err, test.want)
			}
		})
	}
}

func TestParseOpenHowAcceptsZeroedExtension(t *testing.T) {
	if _, _, err := parseOpenHow(make([]byte, openHowSize+8)); err != nil {
		t.Fatalf("parseOpenHow with zeroed extension: %v", err)
	}
}
