//go:build darwin

package time

import "golang.org/x/sys/unix"

// vdsoClockGettime preserves the Linux-oriented internal clock ABI while using
// Darwin's clock_gettime wrapper. Darwin assigns a different number to the
// monotonic clock, so ClockID must never be passed through directly.
func vdsoClockGettime(clockid ClockID, ts *unix.Timespec) int {
	var hostClock int32
	switch clockid {
	case Realtime:
		hostClock = unix.CLOCK_REALTIME
	case Monotonic:
		hostClock = unix.CLOCK_MONOTONIC
	default:
		return -int(unix.EINVAL)
	}
	if err := unix.ClockGettime(hostClock, ts); err != nil {
		if errno, ok := err.(unix.Errno); ok {
			return -int(errno)
		}
		return -int(unix.EIO)
	}
	return 0
}
