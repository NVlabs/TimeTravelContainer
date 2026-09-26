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

package ttcremote

import (
	"io"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestDonatedTCPConnWaitsForNonblockingRead(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	source := os.NewFile(uintptr(fds[0]), "donated-source")
	peer := os.NewFile(uintptr(fds[1]), "donated-peer")
	t.Cleanup(func() {
		source.Close()
		peer.Close()
	})
	conn, err := NewDonatedTCPConn(source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	donated := conn.(*donatedTCPConn)
	flags, err := unix.FcntlInt(donated.file.Fd(), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Fatal("donated connection is not close-on-exec")
	}

	readDone := make(chan error, 1)
	go func() {
		got := make([]byte, 4)
		_, err := io.ReadFull(conn, got)
		if err == nil && string(got) != "data" {
			err = &unexpectedDonatedData{got: string(got)}
		}
		readDone <- err
	}()

	select {
	case err := <-readDone:
		t.Fatalf("read returned before data was available: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if _, err := peer.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("read did not wake after data became available")
	}

	readDone = make(chan error, 1)
	go func() {
		_, err := conn.Read(make([]byte, 1))
		readDone <- err
	}()
	select {
	case err := <-readDone:
		t.Fatalf("second read returned before close: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("blocked read returned nil after close")
		}
	case <-time.After(time.Second):
		t.Fatal("close did not wake blocked donated read")
	}
}

type unexpectedDonatedData struct {
	got string
}

func (e *unexpectedDonatedData) Error() string {
	return "unexpected donated connection data: " + e.got
}
