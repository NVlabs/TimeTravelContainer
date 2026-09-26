//go:build linux

package config

const (
	hostDefaultPlatform      = "systrap"
	hostDefaultIgnoreCgroups = false
	hostDefaultDirectFS      = true
	hostDefaultTTCNetworkNAT = false
)

var hostDefaultUnsafeNoHostIsolation = false

const hostDefaultNetwork = NetworkSandbox

func validateHostConfig(*Config) error { return nil }
