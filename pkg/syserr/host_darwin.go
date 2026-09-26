// Copyright 2021 The gVisor Authors.
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

//go:build darwin
// +build darwin

package syserr

import (
	"fmt"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux/errno"
	"gvisor.dev/gvisor/pkg/errors"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/log"
)

// maxErrno sizes both the host-to-Linux table and the Linux ABI backwards
// table in syserr.go. It must therefore cover Linux's errno namespace even
// though Darwin currently defines fewer host errno values.
const maxErrno = 134

var darwinHostTranslations [maxErrno]*Error

// ErrDeadlock is defined with the Linux-only errors on Linux hosts.
var ErrDeadlock = newWithHost("resource deadlock would occur", errno.EDEADLOCK, unix.EDEADLK)

func init() {
	// Darwin reports a missing extended attribute as ENOATTR. Linux exposes
	// the same condition as ENODATA; lisafs carries host errno values across
	// its trusted gofer transport before converting them here.
	addHostTranslation(unix.ENOATTR, ErrNoDataAvailable)
	// Darwin distinguishes ENOTSUP from EOPNOTSUPP; Linux defines them as
	// the same value.
	addHostTranslation(unix.ENOTSUP, ErrEndpointOperation)
}

// linuxBackwardsError returns the error that ToError produces for a Linux
// errno. It must not be a unix.Errno: Darwin numbers errno values
// differently, so FromError and host errno comparisons would read the Linux
// value as a Darwin one. Linux ENOTCONN (107) is not a Darwin errno at all,
// and Linux EAGAIN (11) is Darwin EDEADLK.
func linuxBackwardsError(e errno.Errno, message string) error {
	// linuxerr aliases ENONET to ENOENT, so its table has no ENONET entry.
	if e == errno.ENONET {
		return errors.New(e, message)
	}
	// Prefer linuxerr's canonical values, which callers compare by identity
	// (for example, EPIPE raises SIGPIPE). Despite its name, ErrorFromUnix
	// indexes linuxerr's table by Linux errno.
	return linuxerr.ErrorFromUnix(unix.Errno(e))
}

// HostToLinux returns the Linux ABI errno for a host errno. Darwin errno
// values without a Linux equivalent become EIO.
func HostToLinux(err unix.Errno) errno.Errno {
	if e := getHostTranslation(err); e != nil {
		return e.ToLinux()
	}
	log.Warningf("Host errno %d (%v) has no Linux equivalent; using EIO", int(err), err)
	return errno.EIO
}

func getHostTranslation(err unix.Errno) *Error {
	if uint64(err) >= uint64(len(darwinHostTranslations)) {
		return nil
	}
	return darwinHostTranslations[err]
}

// TODO(gvisor.dev/issue/1270): We currently only add translations for errors
// that exist both on Darwin and Linux.
func addHostTranslation(host unix.Errno, trans *Error) {
	if darwinHostTranslations[host] != nil {
		panic(fmt.Sprintf("duplicate translation for host errno %q (%d)", host.Error(), host))
	}
	darwinHostTranslations[host] = trans
}
