//go:build linux

package bundle

import (
	"os"

	"golang.org/x/sys/unix"
)

func cloneOpenFile(destination, source *os.File) (*os.File, error) {
	return destination, unix.IoctlFileClone(int(destination.Fd()), int(source.Fd()))
}
