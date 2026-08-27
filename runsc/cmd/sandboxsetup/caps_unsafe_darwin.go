//go:build darwin

package sandboxsetup

import specs "github.com/opencontainers/runtime-spec/specs-go"

func ApplyCapsAllThreads(caps *specs.LinuxCapabilities) error { return nil }
