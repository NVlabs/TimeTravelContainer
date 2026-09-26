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

package fuse

import (
	"testing"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/abi/linux/errno"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
)

// TestResponseErrorIsLinuxErrno checks that a FUSE server's error is read as
// a Linux errno whatever the host, and that invalid ones become EINVAL rather
// than panicking.
func TestResponseErrorIsLinuxErrno(t *testing.T) {
	for _, tc := range []struct {
		errno int32
		want  error
	}{
		{errno: 0, want: nil},
		{errno: -int32(errno.ENOSYS), want: linuxerr.ENOSYS},
		{errno: -int32(errno.EAGAIN), want: linuxerr.EAGAIN},
		{errno: -int32(errno.ENOTCONN), want: linuxerr.ENOTCONN},
		{errno: -int32(errno.ESTALE), want: linuxerr.ESTALE},
		// 41 and 58 are unused by Linux but valid on Darwin.
		{errno: -41, want: linuxerr.EINVAL},
		{errno: -58, want: linuxerr.EINVAL},
		{errno: -1000, want: linuxerr.EINVAL},
		{errno: -1 << 31, want: linuxerr.EINVAL},
	} {
		r := Response{hdr: linux.FUSEHeaderOut{Error: tc.errno}}
		if got := r.Error(); got != tc.want {
			t.Errorf("Response{Error: %d}.Error() = %v, want %v", tc.errno, got, tc.want)
		}
	}
}
