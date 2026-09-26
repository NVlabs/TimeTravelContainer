//go:build linux

package hostinet

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

func newHostSocket(family, stype, protocol int) (int, error) {
	return unix.Socket(family, stype|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, protocol)
}

func hostAccept4(fd int, addr *byte, addrlen *uint32, flags int) (int, error) {
	afd, _, errno := unix.Syscall6(unix.SYS_ACCEPT4, uintptr(fd), uintptr(unsafe.Pointer(addr)), uintptr(unsafe.Pointer(addrlen)), uintptr(flags), 0, 0)
	if errno != 0 {
		return 0, translateIOSyscallError(errno)
	}
	return int(afd), nil
}
