// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

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
