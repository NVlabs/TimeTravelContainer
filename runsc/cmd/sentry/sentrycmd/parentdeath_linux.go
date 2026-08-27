//go:build linux

package sentrycmd

import "golang.org/x/sys/unix"

func armParentDeathSignal() error {
	return unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(unix.SIGKILL), 0, 0, 0)
}
