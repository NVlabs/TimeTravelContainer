// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"gvisor.dev/gvisor/pkg/state"
)

// How this package's types load from what builds before TTC's merge of
// upstream gVisor through 2026-10-28 (TTC's dailies through 20261023) saved;
// see pkg/state/migrate.go, and runsc/ttcd/schemas for their shapes.
func init() {
	// Older builds had no setfsuid: the filesystem IDs were the effective ones.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/sentry/kernel/auth.Credentials",
		Fields: []string{"RealKUID", "EffectiveKUID", "SavedKUID", "RealKGID", "EffectiveKGID", "SavedKGID", "ExtraKGIDs", "PermittedCaps", "InheritableCaps", "EffectiveCaps", "BoundingCaps", "AmbientCaps", "KeepCaps", "SecureBits", "UserNamespace"},
		Set:    []string{"FilesystemKUID", "FilesystemKGID"},
		Fix: func(obj any, old map[string]any) {
			c := obj.(*Credentials)
			c.FilesystemKUID = c.EffectiveKUID
			c.FilesystemKGID = c.EffectiveKGID
		},
	})
	// Older builds had no max_user_namespaces: the limit is the default, and
	// after load each namespace charges its ancestors, as its creation now does.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/sentry/kernel/auth.UserNamespace",
		Fields: []string{"parent", "owner", "Keys", "uidMapFromParent", "uidMapToParent", "gidMapFromParent", "gidMapToParent", "parentHadSetfcap", "setgroupsAllowed", "inode"},
		Set:    []string{"maxUserNamespaces", "numUserNamespaces"},
		Fix: func(obj any, old map[string]any) {
			obj.(*UserNamespace).maxUserNamespaces.Store(defaultMaxUserNamespaces)
		},
		FixAfterLoad: func(obj any, old map[string]any) {
			for cur := obj.(*UserNamespace).parent; cur != nil; cur = cur.parent {
				cur.numUserNamespaces.Add(1)
			}
		},
	})
}
