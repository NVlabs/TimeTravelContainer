//go:build linux

package specutils

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cenkalti/backoff"
	"golang.org/x/sys/unix"
)

func waitForNonChildExit(pid int, timeout time.Duration) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pid: %d", pid)
	}
	pidfd, err := unix.PidfdOpen(pid, 0)
	if err == unix.ESRCH {
		return nil
	}
	if err != nil {
		b := backoff.WithMaxRetries(backoff.NewConstantBackOff(10*time.Millisecond), uint64(timeout/(10*time.Millisecond)))
		return backoff.Retry(func() error {
			if err := unix.Kill(pid, 0); err == nil {
				return fmt.Errorf("process %d is still running", pid)
			}
			return nil
		}, b)
	}
	defer unix.Close(pidfd)
	deadline := time.Now().Add(timeout)
	for {
		timeoutMS := int(time.Until(deadline).Milliseconds())
		if timeoutMS <= 0 {
			return fmt.Errorf("process %d is still running after %v", pid, timeout)
		}
		pfds := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
		n, err := unix.Poll(pfds, timeoutMS)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return fmt.Errorf("polling pidfd for process %d: %v", pid, err)
		}
		if n > 0 {
			return nil
		}
	}
}

func safeMount(src, dst, fstype string, flags uintptr, data, procPath string) error {
	fd, err := unix.Open(dst, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("failed to safely mount: Open(%s, _, _): %w", dst, err)
	}
	defer unix.Close(fd)
	if procPath == "" {
		procPath = "/proc"
	}
	safePath := filepath.Join(procPath, "self/fd", strconv.Itoa(fd))
	target, err := os.Readlink(safePath)
	if err != nil {
		return fmt.Errorf("failed to safely mount: Readlink(%s): %w", safePath, err)
	}
	if dst != target {
		return &ErrSymlinkMount{fmt.Errorf("failed to safely mount: expected to open %s, but found %s", dst, target)}
	}
	if err := unix.Mount(src, safePath, fstype, flags, data); err != nil {
		return fmt.Errorf("mount(%q, %q, %q, %#x, %q) failed: %w", src, safePath, fstype, flags, data, err)
	}
	return nil
}

func hostOOMScoreAdj(pid int) (int, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/oom_score_adj", pid))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}
