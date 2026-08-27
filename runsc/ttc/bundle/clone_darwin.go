//go:build darwin

package bundle

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// cloneOpenFile replaces destination with an APFS clone of source. clonefile
// requires a nonexistent destination, so callers must adopt the returned file
// even when cloneErr is non-nil: in that case it is a newly-created empty file
// ready for the portable whole-file-copy fallback.
func cloneOpenFile(destination, source *os.File, destinationPath string) (*os.File, error) {
	if err := destination.Close(); err != nil {
		return nil, err
	}
	if err := unix.Unlink(destinationPath); err != nil {
		return nil, err
	}
	cloneErr := unix.Fclonefileat(int(source.Fd()), unix.AT_FDCWD, destinationPath, unix.CLONE_NOFOLLOW|unix.CLONE_NOOWNERCOPY)
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW
	if cloneErr != nil {
		// Preserve the cross-filesystem and non-APFS fallback contract.
		flags |= unix.O_CREAT | unix.O_EXCL
	}
	fd, openErr := unix.Open(destinationPath, flags, 0600)
	if openErr != nil {
		return nil, errors.Join(cloneErr, openErr)
	}
	return os.NewFile(uintptr(fd), destinationPath), cloneErr
}
