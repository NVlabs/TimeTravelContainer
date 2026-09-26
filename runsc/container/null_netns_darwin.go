//go:build darwin

package container

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/specutils"
)

func nullNetNSPath(conf *config.Config) string {
	return filepath.Join(conf.SharedRoot(), specutils.NullNetNSFilename)
}

func openNullNetNS(string) (*os.File, error) { return nil, unix.EOPNOTSUPP }

func pinNullNetNS(*config.Config, int) error {
	return fmt.Errorf("network namespaces are unavailable on native macOS: %w", unix.EOPNOTSUPP)
}
