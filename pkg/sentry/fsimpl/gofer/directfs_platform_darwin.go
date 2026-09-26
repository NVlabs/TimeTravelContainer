//go:build darwin

package gofer

import "golang.org/x/sys/unix"

func directfsSupported() bool { return false }

func directfsFallocate(fd int, mode uint32, offset, length int64) error {
	return unix.EOPNOTSUPP
}

func goferDataSync(fd int) error { return unix.Fsync(fd) }

func dupToCloseOnExec(oldfd, newfd int) error {
	if err := unix.Dup2(oldfd, newfd); err != nil {
		return err
	}
	unix.CloseOnExec(newfd)
	return nil
}
