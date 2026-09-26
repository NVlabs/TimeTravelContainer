//go:build darwin

package sandboxsetup

import (
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/runsc/config"
)

func pivotRoot(root string) error { return unix.EOPNOTSUPP }

// AdjustMountOptions adds Darwin-filesystem-specific gofer mount options.
func AdjustMountOptions(_ *config.Config, path string, opts []string) ([]string, error) {
	rv := append([]string(nil), opts...)
	var statfs unix.Statfs_t
	if err := unix.Statfs(path, &statfs); err != nil {
		return nil, err
	}
	name := make([]byte, 0, len(statfs.Fstypename))
	for _, c := range statfs.Fstypename {
		if c == 0 {
			break
		}
		name = append(name, byte(c))
	}
	switch string(name) {
	case "nfs", "osxfuse", "macfuse":
		rv = append(rv, "disable_file_handle_sharing")
	}
	return rv, nil
}
