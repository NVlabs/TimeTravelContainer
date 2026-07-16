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

package ownership

import (
	"path/filepath"
	"testing"
)

func TestManifestRoundTrip(t *testing.T) {
	name := filepath.Join(t.TempDir(), ManifestName)
	want := NewManifest()
	want.Entries["."] = Owner{UID: 0, GID: 0}
	want.Entries["var/lib/app"] = Owner{UID: 123, GID: 456}
	if err := Write(name, want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Read(name)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if owner, ok := got.Lookup("./var/lib/app"); !ok || owner != (Owner{UID: 123, GID: 456}) {
		t.Fatalf("Lookup: owner=%+v, ok=%t", owner, ok)
	}
}

func TestCleanPathRejectsEscape(t *testing.T) {
	for _, name := range []string{"../etc/passwd", "/etc/passwd", "a/../../b"} {
		if _, err := CleanPath(name); err == nil {
			t.Errorf("CleanPath(%q) succeeded", name)
		}
	}
}
