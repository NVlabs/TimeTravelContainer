//go:build darwin

package sandbox

import (
	"fmt"
	"os/exec"

	"golang.org/x/sys/unix"
)

func directIOFlag() int { return 0 }

func hostFDDirectory() string { return "/dev/fd" }

func hostSocketPair() ([2]int, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return fds, err
	}
	unix.CloseOnExec(fds[0])
	unix.CloseOnExec(fds[1])
	return fds, nil
}

func usableControlSocketPath(path string) (string, func(), error) {
	// sockaddr_un.sun_path is 104 bytes on Darwin, including the terminator.
	const darwinUnixPathMax = 104
	if len(path) >= darwinUnixPathMax {
		return "", nil, fmt.Errorf("control socket path is too long for macOS (%d >= %d): %q", len(path), darwinUnixPathMax, path)
	}
	return path, func() {}, nil
}

func unsupportedHostIsolation() error {
	return fmt.Errorf("Linux host credential and namespace setup is unavailable on native macOS: %w", unix.EOPNOTSUPP)
}

func disableSetgroupsForIDMapping(*exec.Cmd) error                { return unsupportedHostIsolation() }
func setCommandCredential(*exec.Cmd, uint32, uint32) error        { return unsupportedHostIsolation() }
func configureSandboxIDMapping(*exec.Cmd, uint32, int, int) error { return unsupportedHostIsolation() }
func addChrootAmbientCapabilities(*exec.Cmd, bool) error          { return unsupportedHostIsolation() }
func configureRootlessCommandCapabilities(*exec.Cmd) error        { return unsupportedHostIsolation() }
func setCommandParentDeathSignal(*exec.Cmd)                       {}
