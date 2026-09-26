//go:build darwin

package hostinet

import (
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/sentry/inet"
)

func queryInterfaceFeatures(interfaces map[int32]inet.Interface) error {
	return unix.EOPNOTSUPP
}
