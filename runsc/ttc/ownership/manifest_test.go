// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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
	want.Modes["var/lib/app"] = 0
	want.Xattrs["var/lib/app"] = map[string][]byte{"security.capability": {0, 1, 2, 0xff}}
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
	if mode, ok := got.LookupMode("./var/lib/app"); !ok || mode != 0 {
		t.Fatalf("LookupMode: mode=%#o, ok=%t", mode, ok)
	}
	if value, ok := got.LookupXattr("./var/lib/app", "security.capability"); !ok || string(value) != string([]byte{0, 1, 2, 0xff}) {
		t.Fatalf("LookupXattr: value=%v, ok=%t", value, ok)
	}
	if names := got.ListXattrs("var/lib/app"); len(names) != 1 || names[0] != "security.capability" {
		t.Fatalf("ListXattrs: %v", names)
	}
}

func TestCleanPathRejectsEscape(t *testing.T) {
	for _, name := range []string{"../etc/passwd", "/etc/passwd", "a/../../b"} {
		if _, err := CleanPath(name); err == nil {
			t.Errorf("CleanPath(%q) succeeded", name)
		}
	}
}
