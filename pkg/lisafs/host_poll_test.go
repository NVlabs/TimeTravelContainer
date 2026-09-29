// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package lisafs

import (
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func cpuTime(t *testing.T) time.Duration {
	t.Helper()
	var usage unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &usage); err != nil {
		t.Fatal(err)
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}

// TestWaitForHangupIgnoresPendingData checks that unread bytes on the socket
// neither end the wait nor make it spin, and that the peer's close does.
func TestWaitForHangupIgnoresPendingData(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	if _, err := unix.Write(fds[1], []byte("unread response")); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- waitForHangup(fds[0]) }()

	const window = 300 * time.Millisecond
	before := cpuTime(t)
	select {
	case err := <-done:
		t.Fatalf("waitForHangup returned %v while the peer was open", err)
	case <-time.After(window):
	}
	if used := cpuTime(t) - before; used > window/2 {
		t.Errorf("waitForHangup used %v of CPU in %v with unread data pending", used, window)
	}

	unix.Close(fds[1])
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waitForHangup after peer close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waitForHangup did not return after the peer closed")
	}
}
