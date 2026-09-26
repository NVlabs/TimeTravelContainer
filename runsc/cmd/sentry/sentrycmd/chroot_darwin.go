//go:build darwin

package sentrycmd

import (
	"fmt"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/runsc/config"
)

// setUpChroot is intentionally a no-op in native macOS trusted mode. The
// Sentry and gofers retain the invoking user's host filesystem view.
func setUpChroot(_ *specs.Spec, conf *config.Config) error {
	if !conf.UnsafeNoHostIsolation {
		return fmt.Errorf("native macOS execution requires --unsafe-no-host-isolation")
	}
	return nil
}
