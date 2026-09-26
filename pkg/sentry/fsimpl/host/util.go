// Copyright 2020 The gVisor Authors.
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

package host

import (
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
)

// isBlockError checks if an error is EAGAIN or EWOULDBLOCK.
// If so, they can be transformed into linuxerr.ErrWouldBlock.
func isBlockError(err error) bool {
	// unix.Errno is a host ABI value. Darwin's EAGAIN/EWOULDBLOCK is 35,
	// which Linux interprets as EDEADLK; comparing it only through linuxerr
	// therefore leaked "resource deadlock avoided" into the guest instead of
	// waiting for an imported host pipe to become readable.
	return err == unix.EAGAIN || err == unix.EWOULDBLOCK ||
		linuxerr.Equals(linuxerr.EAGAIN, err) || linuxerr.Equals(linuxerr.EWOULDBLOCK, err)
}
