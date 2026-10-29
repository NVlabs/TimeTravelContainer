// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package fs

import (
	"testing"

	"gvisor.dev/gvisor/pkg/lisafs"
)

func TestTrustedRootOwnershipMapping(t *testing.T) {
	const (
		ruid = 501
		euid = 502
		rgid = 20
		egid = 21
	)
	config := &Config{
		RUID:                 ruid,
		EUID:                 euid,
		RGID:                 rgid,
		EGID:                 egid,
		TrustedRootOwnership: true,
	}
	for _, tc := range []struct {
		name         string
		hostUID      uint32
		hostGID      uint32
		guestUIDWant uint32
		guestGIDWant uint32
	}{
		{name: "real IDs", hostUID: ruid, hostGID: rgid, guestUIDWant: 0, guestGIDWant: 0},
		{name: "effective IDs", hostUID: euid, hostGID: egid, guestUIDWant: 0, guestGIDWant: 0},
		{name: "unrelated IDs", hostUID: 1234, hostGID: 5678, guestUIDWant: 1234, guestGIDWant: 5678},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uid, gid := mapHostOwnerToGuest(config, tc.hostUID, tc.hostGID)
			if uid != tc.guestUIDWant || gid != tc.guestGIDWant {
				t.Fatalf("mapHostOwnerToGuest(%d:%d)=%d:%d, want %d:%d", tc.hostUID, tc.hostGID, uid, gid, tc.guestUIDWant, tc.guestGIDWant)
			}
		})
	}

	uid, gid := mapGuestOwnerToHost(config, 0, 0)
	if uid != ruid || gid != rgid {
		t.Fatalf("mapGuestOwnerToHost(0:0)=%d:%d, want %d:%d", uid, gid, ruid, rgid)
	}
	uid, gid = mapGuestOwnerToHost(config, lisafs.NoUID, lisafs.NoGID)
	if uid != lisafs.NoUID || gid != lisafs.NoGID {
		t.Fatalf("sentinels changed to %d:%d", uid, gid)
	}
	uid, gid = mapGuestOwnerToHost(config, 1234, 5678)
	if uid != 1234 || gid != 5678 {
		t.Fatalf("unrelated guest owner changed to %d:%d", uid, gid)
	}
}
