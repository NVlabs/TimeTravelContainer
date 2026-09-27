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

package seatbelt

/*
#include <stdint.h>
#include <stdlib.h>

// <sandbox.h> marks sandbox_init deprecated, but it remains the interface for
// applying a profile of one's own; declare it without the mark.
int sandbox_init(const char *profile, uint64_t flags, char **errorbuf);
void sandbox_free_error(char *errorbuf);
*/
import "C"

import (
	"errors"
	"unsafe"
)

// Apply confines this process, and every process it starts from then on, to
// profile, which is in the Sandbox Profile Language. Nothing lifts the
// confinement.
func Apply(profile string) error {
	text := C.CString(profile)
	defer C.free(unsafe.Pointer(text))
	var message *C.char
	if C.sandbox_init(text, 0, &message) == 0 {
		return nil
	}
	if message == nil {
		return errors.New("sandbox_init failed")
	}
	defer C.sandbox_free_error(message)
	return errors.New(C.GoString(message))
}
