//go:build darwin

package container

import (
	"fmt"
	"os/exec"

	"golang.org/x/sys/unix"
)

func containerHostSocketPair() ([2]int, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return fds, err
	}
	unix.CloseOnExec(fds[0])
	unix.CloseOnExec(fds[1])
	return fds, nil
}

func setGoferParentDeathSignal(*exec.Cmd) {}

func goferHostIsolationUnsupported() error {
	return fmt.Errorf("Linux gofer credential isolation is unavailable on native macOS: %w", unix.EOPNOTSUPP)
}

func setGoferCommandCredential(*exec.Cmd, uint32, uint32) error {
	return goferHostIsolationUnsupported()
}
func disableGoferSetgroupsForIDMapping(*exec.Cmd) error {
	return goferHostIsolationUnsupported()
}

func hostSetOOMScoreAdj(int, int) error { return nil }
