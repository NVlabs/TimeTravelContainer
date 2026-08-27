//go:build darwin

package console

import (
	"os"

	"golang.org/x/sys/unix"
)

// StdioIsPty returns true if all standard streams are Darwin terminals.
func StdioIsPty() bool {
	for _, f := range []*os.File{os.Stdin, os.Stdout, os.Stderr} {
		if _, err := unix.IoctlGetTermios(int(f.Fd()), unix.TIOCGETA); err != nil {
			return false
		}
	}
	return true
}
