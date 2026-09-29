// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package lisafs

import (
	"testing"

	"golang.org/x/sys/unix"
)

// Running out of descriptors is the one server error the client must act on.
func TestOnlyADescriptorShortageIsWorthReclaimingFor(t *testing.T) {
	for _, e := range []unix.Errno{unix.EMFILE, unix.ENFILE} {
		if !starvedErrno(e) {
			t.Errorf("%v is not treated as the server running out of descriptors", e)
		}
	}
	for _, e := range []unix.Errno{unix.ENOENT, unix.EACCES, unix.EIO, unix.ENOSPC, unix.EPERM, 0} {
		if starvedErrno(e) {
			t.Errorf("%v provoked a reclaim it has nothing to do with", e)
		}
	}
}

// A client with nobody listening does not fall over.
//
// The hook is optional: the tests in this package build clients directly, and
// so does anything embedding lisafs that has no cache to give back.
func TestAClientWithNoListenerIsFine(t *testing.T) {
	var client Client
	if client.OnStarved != nil {
		t.Fatal("a client invented a listener nobody installed")
	}
	// What SndRcvMessage does with the errno, without a server to get one from.
	if starvedErrno(unix.EMFILE) && client.OnStarved != nil {
		client.OnStarved()
	}
}
