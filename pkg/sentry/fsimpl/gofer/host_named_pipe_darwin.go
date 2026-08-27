//go:build darwin

package gofer

import "golang.org/x/sys/unix"

func nonblockingPipeHasWriter(fd int32) (bool, error) {
	fds := []unix.PollFd{{Fd: fd, Events: unix.POLLIN}}
	if _, err := unix.Poll(fds, 0); err != nil {
		return false, err
	}
	// For an empty FIFO, Darwin reports POLLHUP exactly when no writer is
	// attached. POLLIN means buffered data and therefore also counts as a
	// writer having existed, matching the Linux tee-based behavior.
	return fds[0].Revents&unix.POLLHUP == 0, nil
}
