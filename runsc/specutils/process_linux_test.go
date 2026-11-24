// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package specutils

import (
	"os"
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"
)

func TestIsProcessRunning(t *testing.T) {
	for _, tc := range []struct {
		pid  int
		want bool
	}{
		{pid: 0, want: false},
		{pid: -1, want: false},
		{pid: os.Getpid(), want: true},
	} {
		if got, err := IsProcessRunning(tc.pid, 0); err != nil || got != tc.want {
			t.Errorf("IsProcessRunning(%d) = (%t, %v), want (%t, nil)", tc.pid, got, err, tc.want)
		}
	}

	cmd := exec.Command("/bin/true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	pid := cmd.Process.Pid
	// Wait for exit without reaping the child so the zombie remains observable.
	_, _, err := RetryEintr(func() (uintptr, uintptr, error) {
		_, _, errno := unix.Syscall6(unix.SYS_WAITID, unix.P_PID, uintptr(pid), 0, unix.WEXITED|unix.WNOWAIT, 0, 0)
		if errno != 0 {
			return 0, 0, errno
		}
		return 0, 0, nil
	})
	if err != nil {
		t.Fatalf("waitid(%d): %v", pid, err)
	}
	if got, err := IsProcessRunning(pid, 0); err != nil || got {
		t.Errorf("IsProcessRunning(%d) for zombie = (%t, %v), want (false, nil)", pid, got, err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if got, err := IsProcessRunning(pid, 0); err != nil || got {
		t.Errorf("IsProcessRunning(%d) after reaping = (%t, %v), want (false, nil)", pid, got, err)
	}
}

// TestProcessIdentity checks that process identities tell processes apart, and
// that KillProcess doesn't kill a process with a different identity but the
// same PID.
func TestProcessIdentity(t *testing.T) {
	var procs [2]*exec.Cmd
	var identities [2]uint64
	for i := range procs {
		procs[i] = exec.Command("/bin/sleep", "1000")
		if err := procs[i].Start(); err != nil {
			t.Fatalf("cmd.Start() failed: %v", err)
		}
		defer procs[i].Process.Kill()
		var err error
		if identities[i], err = ProcessIdentity(procs[i].Process.Pid); err != nil {
			t.Fatalf("ProcessIdentity failed: %v", err)
		}
		if identities[i] == 0 {
			t.Skip("host has no pidfs")
		}
	}
	if identities[0] == identities[1] {
		t.Errorf("two processes have the same identity %d", identities[0])
	}
	pid := procs[0].Process.Pid
	pidfd, err := OpenProcess(pid, identities[0])
	if err != nil {
		t.Errorf("OpenProcess with the process identity failed: %v", err)
	} else {
		unix.Close(pidfd)
	}
	if _, err := OpenProcess(pid, identities[1]); err != unix.ESRCH {
		t.Errorf("OpenProcess with another process identity got %v, expected ESRCH", err)
	}
	if err := KillProcess(pid, identities[1]); err != nil {
		t.Errorf("KillProcess with another process identity failed: %v", err)
	}
	if err := procs[0].Process.Signal(unix.Signal(0)); err != nil {
		t.Errorf("KillProcess with another process identity killed the process: %v", err)
	}
	if err := KillProcess(pid, identities[0]); err != nil {
		t.Errorf("KillProcess failed: %v", err)
	}
	if err := procs[0].Wait(); err == nil {
		t.Errorf("process exited cleanly, expected it to be killed")
	}
}
