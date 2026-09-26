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

package syserr

import (
	"testing"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux/errno"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
)

func TestFromErrorUsesLinuxErrnoDomain(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want errno.Errno
	}{
		{name: "again", err: linuxerr.EAGAIN, want: errno.EAGAIN},
		{name: "deadlock", err: linuxerr.EDEADLK, want: errno.EDEADLK},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := FromError(test.err).ToLinux(); got != test.want {
				t.Fatalf("FromError(%v).ToLinux() = %d, want %d", test.err, got, test.want)
			}
		})
	}
}

// TestToErrorRoundTrip checks that every registered Error survives the
// ToError/FromError round trip that socket and filesystem code performs. On
// Darwin, ToError once produced Linux errno values typed as host errno
// values, so FromError panicked on Linux ENOTCONN (107), which Darwin lacks.
func TestToErrorRoundTrip(t *testing.T) {
	for i, translation := range linuxBackwardsTranslations {
		if !translation.ok || i == errno.NOERRNO {
			continue
		}
		want := errno.Errno(i)
		if got := FromError(translation.err).ToLinux(); got != want {
			t.Errorf("FromError(%v).ToLinux() = %d, want %d", translation.err, got, want)
		}
	}
	if got := FromError(ErrNotConnected.ToError()); got.ToLinux() != errno.ENOTCONN {
		t.Errorf("FromError(ErrNotConnected.ToError()) = %v, want ENOTCONN", got)
	}
	if got := ErrWouldBlock.ToError(); got != linuxerr.ErrWouldBlock {
		t.Errorf("ErrWouldBlock.ToError() = %v, want linuxerr.ErrWouldBlock", got)
	}
}

func TestHostToLinux(t *testing.T) {
	for _, test := range []struct {
		host unix.Errno
		want errno.Errno
	}{
		{host: unix.EINVAL, want: errno.EINVAL},
		{host: unix.EAGAIN, want: errno.EAGAIN},
		{host: unix.EDEADLK, want: errno.EDEADLK},
		{host: unix.ENOTCONN, want: errno.ENOTCONN},
		{host: unix.EOVERFLOW, want: errno.EOVERFLOW},
		{host: unix.EOPNOTSUPP, want: errno.EOPNOTSUPP},
		{host: unix.ENOTSUP, want: errno.EOPNOTSUPP},
	} {
		if got := HostToLinux(test.host); got != test.want {
			t.Errorf("HostToLinux(%d (%v)) = %d, want %d", int(test.host), test.host, got, test.want)
		}
	}
}
