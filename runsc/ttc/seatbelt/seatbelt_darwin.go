// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package seatbelt

/*
#cgo LDFLAGS: -framework CoreFoundation
#include <CoreFoundation/CoreFoundation.h>
#include <stdint.h>
#include <stdlib.h>
#include <unistd.h>

// <sandbox.h> marks sandbox_init deprecated, but it remains the interface for
// applying a profile of one's own; declare it without the mark.
int sandbox_init(const char *profile, uint64_t flags, char **errorbuf);
void sandbox_free_error(char *errorbuf);

// <sandbox.h> does not declare sandbox_check, which libsystem_sandbox
// exports. Given no operation, it returns 1 for a confined process.
int sandbox_check(pid_t pid, const char *operation, int type, ...);

static int ttc_sandbox_confined(void) {
	return sandbox_check(getpid(), NULL, 0);
}
*/
import "C"

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unsafe"
)

// Apply confines this process, and every process it starts from then on, to
// profile, which is in the Sandbox Profile Language. Nothing lifts the
// confinement. Profiles do not stack: in a process that is confined already,
// Apply fails for another profile, and on macOS 15 for the same one too.
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

// Confined reports whether this process is confined to a profile: one that it
// applied, or one that a process that started it was confined to.
func Confined() bool {
	return C.ttc_sandbox_confined() == 1
}

// BundleCacheDir returns the directory of the caches that Apple's frameworks
// keep for this process, such as Metal's compiled shaders and its compiler's
// precompiled headers: the user's cache directory (confstr's
// _CS_DARWIN_USER_CACHE_DIR) and the identifier of the process's main
// bundle, which an executable's embedded Info.plist gives it. It returns ""
// of a process of no identifier, whose caches the frameworks keep in the
// directories that they share among the user's programs of none.
func BundleCacheDir() (string, error) {
	ref := C.CFBundleGetIdentifier(C.CFBundleGetMainBundle())
	if ref == 0 {
		return "", nil
	}
	var buf [256]C.char
	if C.CFStringGetCString(ref, &buf[0], C.CFIndex(len(buf)), C.kCFStringEncodingUTF8) == 0 {
		return "", errors.New("the bundle identifier is not a string of 255 bytes or less")
	}
	id := C.GoString(&buf[0])
	if id == "" || strings.ContainsAny(id, "/\x00") || strings.HasPrefix(id, ".") {
		return "", fmt.Errorf("the bundle identifier %q names no directory", id)
	}
	n := C.confstr(C._CS_DARWIN_USER_CACHE_DIR, nil, 0)
	if n == 0 {
		return "", errors.New("the user has no cache directory")
	}
	dir := make([]byte, n)
	C.confstr(C._CS_DARWIN_USER_CACHE_DIR, (*C.char)(unsafe.Pointer(&dir[0])), n)
	return filepath.Join(string(dir[:n-1]), id), nil
}
