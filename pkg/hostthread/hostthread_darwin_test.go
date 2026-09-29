// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package hostthread

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestSetSyscallsEndReturnsThePreviousSetting(t *testing.T) {
	done := make(chan error, 1)
	go func() {
		if previous := SetSyscallsEnd(true); previous {
			done <- fmt.Errorf("a new goroutine's system calls were declared to end")
			return
		}
		if previous := SetSyscallsEnd(false); !previous {
			done <- fmt.Errorf("SetSyscallsEnd(false) returned %t, want the true set before", previous)
			return
		}
		done <- nil
	}()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestFlockWaitsForAContendedLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	holder, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	waiter, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer waiter.Close()
	if err := Flock(int(holder.Fd()), unix.LOCK_EX); err != nil {
		t.Fatalf("taking an uncontended lock: %v", err)
	}
	acquired := make(chan error, 1)
	go func() {
		SetSyscallsEnd(true)
		if err := Flock(int(waiter.Fd()), unix.LOCK_SH); err != nil {
			acquired <- err
			return
		}
		// The contended wait withdrew the declaration only while it waited.
		if end := SetSyscallsEnd(true); !end {
			acquired <- fmt.Errorf("Flock left the goroutine's system calls undeclared")
			return
		}
		acquired <- nil
	}()
	select {
	case err := <-acquired:
		t.Fatalf("took a lock that another descriptor holds exclusively: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := Flock(int(holder.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := <-acquired; err != nil {
		t.Fatal(err)
	}
}
