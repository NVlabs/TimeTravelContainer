//go:build darwin

package abi

// Host is the ABI implemented by the Sentry. On native macOS the execution
// host is Darwin, but the userspace kernel and traced applications still use
// the Linux ABI.
const Host = Linux
