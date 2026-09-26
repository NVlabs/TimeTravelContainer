//go:build linux

package specutils

func hostExecutablePath() string { return "/proc/self/exe" }
