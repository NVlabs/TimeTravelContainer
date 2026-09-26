//go:build darwin

package boot

import "golang.org/x/sys/unix"

func remapFD(oldfd, newfd int) error {
	if err := unix.Dup2(oldfd, newfd); err != nil {
		return err
	}
	unix.CloseOnExec(newfd)
	return nil
}

func waitForGoferDisconnect(goferFD int) error {
	events := []unix.PollFd{{Fd: int32(goferFD), Events: unix.POLLHUP}}
	for {
		if _, err := unix.Poll(events, -1); err != nil {
			if err == unix.EINTR {
				continue
			}
			return err
		}
		return nil
	}
}
