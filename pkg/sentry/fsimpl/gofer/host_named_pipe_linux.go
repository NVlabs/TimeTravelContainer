//go:build linux

package gofer

import (
	"fmt"
	"sync"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
)

var (
	tempPipeMu      sync.Mutex
	tempPipeReadFD  int
	tempPipeWriteFD int
	tempPipeBuf     [1]byte
)

func init() {
	var pipeFDs [2]int
	if err := unix.Pipe(pipeFDs[:]); err != nil {
		panic(fmt.Sprintf("failed to create pipe for gofer.blockUntilNonblockingPipeHasWriter: %v", err))
	}
	tempPipeReadFD = pipeFDs[0]
	tempPipeWriteFD = pipeFDs[1]
}

func nonblockingPipeHasWriter(fd int32) (bool, error) {
	tempPipeMu.Lock()
	defer tempPipeMu.Unlock()
	n, err := unix.Tee(int(fd), tempPipeWriteFD, 1, unix.SPLICE_F_NONBLOCK)
	if linuxerr.Equals(linuxerr.EAGAIN, err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}
	if n, err := unix.Read(tempPipeReadFD, tempPipeBuf[:]); err != nil || n != 1 {
		panic(fmt.Sprintf("failed to drain pipe for gofer.blockUntilNonblockingPipeHasWriter: got (%d, %v), wanted (1, nil)", n, err))
	}
	return true, nil
}
