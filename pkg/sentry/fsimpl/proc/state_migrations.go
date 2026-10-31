// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package proc

import (
	"gvisor.dev/gvisor/pkg/state"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// How this package's types load from what builds before TTC's merge of
// upstream gVisor through 2026-10-28 (TTC's dailies through 20261023) saved;
// see pkg/state/migrate.go, and runsc/ttcd/schemas for their shapes.
func init() {
	// They serve only the creation of a procfs, which a loaded one does not
	// repeat.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/sentry/fsimpl/proc.InternalData",
		Fields: []string{"SaveTriggerEnabled", "FSCheckpointEnabled", "GVisorMarkerFile", "OverrideProcs", "Cgroups"},
		Zero:   []string{"ContainerName", "ContainerUID", "ContainerGID"},
	})
	// These inodes keep no reference count now.
	state.RegisterMigration(state.Migration{
		Type:    "pkg/sentry/fsimpl/proc.checkpointInode",
		Fields:  []string{"InodeAttrs", "InodeNoStatFS", "fdInfoDirInodeRefs", "InodeTemporary", "InodeNotAnonymous", "InodeNotDirectory", "InodeNotSymlink", "InodeWatches", "InodeFSOwned", "locks", "k", "rdevMajor", "saveTriggerEnabled"},
		Dropped: []string{"fdInfoDirInodeRefs", "InodeTemporary"},
	})
	// As checkpointInode.
	state.RegisterMigration(state.Migration{
		Type:    "pkg/sentry/fsimpl/proc.fsCheckpointInode",
		Fields:  []string{"InodeAttrs", "InodeNoStatFS", "fdInfoDirInodeRefs", "InodeTemporary", "InodeNotAnonymous", "InodeNotDirectory", "InodeNotSymlink", "InodeWatches", "InodeFSOwned", "locks", "k", "rdevMajor"},
		Dropped: []string{"fdInfoDirInodeRefs", "InodeTemporary"},
	})
	// Older builds had the file only for IPv4.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/sentry/fsimpl/proc.ipForwarding",
		Fields: []string{"DynamicBytesFile", "stack", "enabled"},
		Set:    []string{"protocol"},
		Fix: func(obj any, old map[string]any) {
			obj.(*ipForwarding).protocol = header.IPv4ProtocolNumber
		},
	})
	// Older builds kept every setting in the set, as sets that they made still
	// do.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/sentry/fsimpl/proc.netConfSet",
		Fields: []string{"StaticDirectory", "fs", "owner", "kind", "inos", "vals"},
		Zero:   []string{"linked"},
	})
	// The range is read from the stack now.
	state.RegisterMigration(state.Migration{
		Type:    "pkg/sentry/fsimpl/proc.portRange",
		Fields:  []string{"DynamicBytesFile", "stack", "start", "end"},
		Dropped: []string{"start", "end"},
	})
	// A procfs mounted before the upgrade has the environment of the sandbox's
	// only container, if it has one.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/sentry/fsimpl/proc.specEnvironData",
		Fields: []string{"dynamicBytesFileSetAttr", "k"},
		Set:    []string{"containerName"},
		FixAfterLoad: func(obj any, old map[string]any) {
			d := obj.(*specEnvironData)
			if names := d.k.ContainerNames(); len(names) == 1 {
				d.containerName = names[0]
			}
		},
	})
	// The setting is read from the stack now.
	state.RegisterMigration(state.Migration{
		Type:    "pkg/sentry/fsimpl/proc.tcpSackData",
		Fields:  []string{"DynamicBytesFile", "stack", "enabled"},
		Dropped: []string{"enabled"},
	})
}
