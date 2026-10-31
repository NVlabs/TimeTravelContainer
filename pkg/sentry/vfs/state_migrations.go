// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package vfs

import (
	"gvisor.dev/gvisor/pkg/state"
)

// How this package's types load from what builds before TTC's merge of
// upstream gVisor through 2026-10-28 (TTC's dailies through 20261023) saved;
// see pkg/state/migrate.go, and runsc/ttcd/schemas for their shapes.
func init() {
	// The ready list is rebuilt as an epoll loads.
	state.RegisterMigration(state.Migration{
		Type:    "pkg/sentry/vfs.EpollInstance",
		Fields:  []string{"vfsfd", "FileDescriptionDefaultImpl", "DentryMetadataFileDescriptionImpl", "NoLockFD", "q", "interest", "ready", "readySeq"},
		Dropped: []string{"ready"},
	})
	// As EpollInstance.
	state.RegisterMigration(state.Migration{
		Type:    "pkg/sentry/vfs.epollInterest",
		Fields:  []string{"epoll", "key", "waiter", "mask", "ready", "epollInterestEntry", "readySeq", "userData"},
		Dropped: []string{"ready", "epollInterestEntry", "readySeq"},
	})
	// Only path walks in flight hold one, and none is at a save.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/sentry/vfs.PathOperation",
		Fields: []string{"Root", "Start", "Path", "FollowFinalSymlink"},
		Zero:   []string{"ResolveFlags"},
	})
	// As PathOperation.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/sentry/vfs.ResolvingPath",
		Fields: []string{"vfs", "root", "mount", "start", "pit", "flags", "mustBeDir", "symlinks", "curPart", "creds", "nextMount", "nextStart", "absSymlinkTarget", "parts"},
		Zero:   []string{"mountSeq", "renameState"},
	})
	// MountMax is the default: zero would refuse every mount. /proc/sys/fs/file-
	// nr does not count files opened before the upgrade, and never reads below
	// 0. No rename is in flight at a save. Older builds shared no dynamic device
	// major.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/sentry/vfs.VirtualFilesystem",
		Fields: []string{"mounts", "mountpoints", "lastMountID", "lastMountNamespaceID", "anonMount", "devices", "dynCharDevMajorUsed", "anonBlockDevMinorNext", "anonBlockDevMinor", "fsTypes", "filesystems", "groupIDBitmap", "mountPromises", "toDecRef"},
		Zero:   []string{"numFiles", "renameState", "dynCharDevMajorShared"},
		Set:    []string{"MountMax"},
		Fix: func(obj any, old map[string]any) {
			obj.(*VirtualFilesystem).MountMax.Store(DefaultMountMax)
		},
	})
}
