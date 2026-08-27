//go:build linux

package config

import "golang.org/x/sys/unix"

func hostHotSyscalls() []uintptr { return []uintptr{unix.SYS_FUTEX} }
