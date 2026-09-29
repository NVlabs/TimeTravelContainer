// Copyright 2021 The gVisor Authors.
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

package eventfd

import (
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestReadWrite(t *testing.T) {
	efd, err := Create()
	if err != nil {
		t.Fatalf("failed to Create(): %v", err)
	}
	defer efd.Close()

	// Make sure we can read actual values
	const want = 343
	if err := efd.Write(want); err != nil {
		t.Fatalf("failed to write value: %d", want)
	}

	got, err := efd.Read()
	if err != nil {
		t.Fatalf("failed to read value: %v", err)
	}
	if got != want {
		t.Fatalf("Read(): got %d, but wanted %d", got, want)
	}
}

func TestWait(t *testing.T) {
	efd, err := Create()
	if err != nil {
		t.Fatalf("failed to Create(): %v", err)
	}
	defer efd.Close()

	// There's no way to test with certainty that Wait() blocks indefinitely, but
	// as a best-effort we can wait a bit on it.
	errCh := make(chan error)
	go func() {
		errCh <- efd.Wait()
	}()
	select {
	case err := <-errCh:
		t.Fatalf("Wait() returned without a call to Notify(): %v", err)
	case <-time.After(500 * time.Millisecond):
	}

	// Notify and check that Wait() returned.
	if err := efd.Notify(); err != nil {
		t.Fatalf("Notify() failed: %v", err)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Read() failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Read() did not return after Notify()")
	}
}

// TestWritesAccumulate checks that writes add up until a read returns and
// resets their sum, however many there are.
func TestWritesAccumulate(t *testing.T) {
	efd, err := Create()
	if err != nil {
		t.Fatalf("failed to Create(): %v", err)
	}
	defer efd.Close()

	// More writes than a pipe could hold as separate records.
	const writes = 100000
	for i := 0; i < writes; i++ {
		if err := efd.Notify(); err != nil {
			t.Fatalf("Notify() failed: %v", err)
		}
	}
	if err := efd.Write(5); err != nil {
		t.Fatalf("Write(5) failed: %v", err)
	}
	if got, err := efd.Read(); err != nil || got != writes+5 {
		t.Fatalf("Read() = %d, %v, want %d, nil", got, err, writes+5)
	}

	// The read consumed every write, so the eventfd isn't readable.
	if readable(t, efd) {
		t.Fatalf("eventfd is readable after its value was read")
	}
	if err := efd.Notify(); err != nil {
		t.Fatalf("Notify() failed: %v", err)
	}
	if !readable(t, efd) {
		t.Fatalf("eventfd isn't readable after Notify()")
	}
	if got, err := efd.Read(); err != nil || got != 1 {
		t.Fatalf("Read() = %d, %v, want 1, nil", got, err)
	}
}

// TestDupSharesValue checks that a duplicate observes writes to the original.
func TestDupSharesValue(t *testing.T) {
	efd, err := Create()
	if err != nil {
		t.Fatalf("failed to Create(): %v", err)
	}
	defer efd.Close()
	dup, err := efd.Dup()
	if err != nil {
		t.Fatalf("Dup() failed: %v", err)
	}
	defer dup.Close()
	if err := efd.Write(3); err != nil {
		t.Fatalf("Write(3) failed: %v", err)
	}
	if err := dup.Write(4); err != nil {
		t.Fatalf("Write(4) failed: %v", err)
	}
	if got, err := dup.Read(); err != nil || got != 7 {
		t.Fatalf("Read() = %d, %v, want 7, nil", got, err)
	}
}

func readable(t *testing.T, efd Eventfd) bool {
	t.Helper()
	fds := []unix.PollFd{{Fd: int32(efd.FD()), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, 0)
	if err != nil {
		t.Fatalf("poll failed: %v", err)
	}
	return n == 1 && fds[0].Revents&unix.POLLIN != 0
}
