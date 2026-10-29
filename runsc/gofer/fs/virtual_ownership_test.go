// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package fs

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/runsc/ttc/ownership"
)

func TestConnectionStatScopesVirtualOwnership(t *testing.T) {
	name := t.TempDir() + "/file"
	if err := os.WriteFile(name, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	hostFD, err := unix.Open(name, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(hostFD)
	raw, err := fstatTo(hostFD)
	if err != nil {
		t.Fatal(err)
	}
	manifest := ownership.NewManifest()
	manifest.Entries["."] = ownership.Owner{}
	manifest.Entries["file"] = ownership.Owner{UID: 1234, GID: 5678}
	manifest.Modes["file"] = 0
	manifest.Xattrs["file"] = map[string][]byte{"security.capability": []byte("virtual-cap")}
	virtual := &connectionImpl{config: &Config{VirtualOwners: manifest}}
	got, err := virtual.stat(hostFD, "file")
	if err != nil {
		t.Fatal(err)
	}
	if got.UID != 1234 || got.GID != 5678 {
		t.Fatalf("virtual owner=%d:%d, want 1234:5678", got.UID, got.GID)
	}
	if got.Ino != raw.Ino || got.Mode != raw.Mode&unix.S_IFMT || got.Size != raw.Size {
		t.Fatalf("virtual stat changed non-owner fields: got=%+v raw=%+v", got, raw)
	}
	unmapped, err := virtual.stat(hostFD, "other")
	if err != nil {
		t.Fatal(err)
	}
	if unmapped.UID != raw.UID || unmapped.GID != raw.GID {
		t.Fatalf("unmapped owner=%d:%d, want host %d:%d", unmapped.UID, unmapped.GID, raw.UID, raw.GID)
	}
	hostBind := &connectionImpl{config: &Config{}}
	unscoped, err := hostBind.stat(hostFD, "file")
	if err != nil {
		t.Fatal(err)
	}
	if unscoped.UID != raw.UID || unscoped.GID != raw.GID {
		t.Fatalf("host-bind owner=%d:%d, want host %d:%d", unscoped.UID, unscoped.GID, raw.UID, raw.GID)
	}
	if value, ok := virtual.virtualXattr("file", "security.capability"); !ok || string(value) != "virtual-cap" {
		t.Fatalf("virtual xattr=%q, ok=%t", value, ok)
	}
	if names := virtual.virtualXattrNames("file"); len(names) != 1 || names[0] != "security.capability" {
		t.Fatalf("virtual xattr names=%v", names)
	}
	if _, ok := hostBind.virtualXattr("file", "security.capability"); ok {
		t.Fatal("host bind unexpectedly received image virtual xattr")
	}
}
