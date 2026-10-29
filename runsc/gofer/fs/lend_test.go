// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package fs

import (
	"testing"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
)

// A read of a regular file borrows the descriptor already open on it.
//
// This is the case that matters: a Sentry caching a dentry keeps its control
// FD, and the read handle beside it used to keep a second descriptor on the
// same file for as long as the dentry lived. It was 3503 of one dead sandbox's
// 8146.
func TestAReadOnRegularFileBorrowsTheDescriptorAlreadyOpenOnIt(t *testing.T) {
	readable := &controlFDLisa{openedFlags: unix.O_RDONLY | unix.O_NONBLOCK | openFlags}
	if !readable.lendableFor(unix.O_RDONLY, unix.S_IFREG) {
		t.Fatal("a read-only open of a regular file did not borrow")
	}
}

// Everything else opens its own, and behaves exactly as it did.
func TestOnlyTheCaseThatIsCertainlySafeBorrows(t *testing.T) {
	readable := &controlFDLisa{openedFlags: unix.O_RDONLY | unix.O_NONBLOCK | openFlags}
	path := &controlFDLisa{openedFlags: pathOpenFlagMask | openFlags}

	for _, one := range []struct {
		why   string
		fd    *controlFDLisa
		flags uint32
		ftype linux.FileMode
	}{
		// A directory's OpenFD seeks its descriptor in Getdent64, so sharing
		// one description would have two readers moving each other's position.
		{"a directory", readable, unix.O_RDONLY, unix.S_IFDIR},
		// Nothing to read from: symlinks and sockets get an O_PATH control FD.
		{"a symlink", path, unix.O_RDONLY, unix.S_IFLNK},
		{"a socket", path, unix.O_RDONLY, unix.S_IFSOCK},
		{"a fifo", readable, unix.O_RDONLY, unix.S_IFIFO},
		// A writable handle needs a writable description, which this is not.
		{"a write", readable, unix.O_WRONLY, unix.S_IFREG},
		{"a read-write", readable, unix.O_RDWR, unix.S_IFREG},
		// Anything else the Sentry asks for goes the long way round rather
		// than being quietly dropped.
		{"a truncating open", readable, unix.O_RDONLY | unix.O_TRUNC, unix.S_IFREG},
		{"a direct open", readable, unix.O_RDONLY | linux.O_DIRECT, unix.S_IFREG},
	} {
		if one.fd.lendableFor(one.flags, one.ftype) {
			t.Errorf("%s borrowed a descriptor it cannot use", one.why)
		}
	}
}

// A borrowed descriptor is not closed by the FD that borrowed it.
//
// The control FD still needs it, and closing a descriptor another object is
// using is the kind of mistake that shows up later as reads from an unrelated
// file.
func TestClosingABorrowedOpenFDLeavesTheDescriptorAlone(t *testing.T) {
	// A real descriptor, so a wrongful close is observable.
	fds := make([]int, 2)
	if err := unix.Pipe(fds); err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])

	borrowed := &openFDLisa{hostFD: fds[0], borrowed: true}
	borrowed.Close()
	if borrowed.hostFD != -1 {
		t.Fatal("the borrowed FD still claims a descriptor after being closed")
	}
	// Still open: fstat on a closed descriptor would be EBADF.
	var stat unix.Stat_t
	if err := unix.Fstat(fds[0], &stat); err != nil {
		t.Fatalf("closing a borrowed OpenFD closed the control FD's descriptor: %v", err)
	}

	// And one that owns its descriptor still closes it.
	owned := &openFDLisa{hostFD: fds[1]}
	owned.Close()
	if err := unix.Fstat(fds[1], &stat); err == nil {
		t.Fatal("an owned descriptor outlived the FD holding it")
	}
}
