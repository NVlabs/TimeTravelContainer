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
)

// maxErrno sizes both the host-to-Linux table and the Linux ABI backwards
// table in syserr.go. It must therefore cover Linux's errno namespace even
// though Darwin currently defines fewer host errno values.
const maxErrno = 134

var darwinHostTranslations [maxErrno]*Error

func init() {
	// Darwin reports a missing extended attribute as ENOATTR. Linux exposes
	// the same condition as ENODATA; lisafs carries host errno values across
	// its trusted gofer transport before converting them here.
	addHostTranslation(unix.ENOATTR, ErrNoDataAvailable)
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
