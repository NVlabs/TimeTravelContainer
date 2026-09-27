// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sandbox

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestFallbackControlSocketNameIncludesRoot(t *testing.T) {
	const id = "8da24af948821edd95e7f2943c935f5c74726fc6e83fa6f630404072aed87eb5"
	a := fallbackControlSocketName("/node-a/runsc", id)
	b := fallbackControlSocketName("/node-b/runsc", id)
	if a == b {
		t.Fatalf("different runsc roots produced the same fallback socket name %q", a)
	}
	if got := fallbackControlSocketName("/node-a/runsc", id); got != a {
		t.Fatalf("fallback socket name is not deterministic: got %q, want %q", got, a)
	}
	// Darwin sockaddr_un.sun_path is 104 bytes including its terminator.
	if path := filepath.Join("/tmp", a); len(path) >= 104 {
		t.Fatalf("fallback control socket path is too long for Darwin: %q (%d bytes)", path, len(path))
	}
}

func TestControlSocketIsInTTCRunDir(t *testing.T) {
	const id = "8da24af948821edd95e7f2943c935f5c74726fc6e83fa6f630404072aed87eb5"
	run, err := os.MkdirTemp("", "ttcrun-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(run) })
	t.Setenv(ttcRunDirEnv, run)
	path, fd, err := createControlSocket("/node-a/runsc", id)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if want := filepath.Join(run, ttcControlSocketName("/node-a/runsc", id)); path != want {
		t.Fatalf("control socket %q, want %q", path, want)
	}
	if info, err := os.Stat(path); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("control socket %q: info=%v err=%v", path, info, err)
	}
	if ttcControlSocketName("/node-b/runsc", id) == filepath.Base(path) {
		t.Fatal("sandboxes of different runsc roots have the same control socket name")
	}
}
