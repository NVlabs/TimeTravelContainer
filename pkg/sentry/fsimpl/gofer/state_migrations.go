// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package gofer

import (
	"gvisor.dev/gvisor/pkg/state"
)

// How this package's types load from what builds before TTC's merge of
// upstream gVisor through 2026-10-28 (TTC's dailies through 20261023) saved;
// see pkg/state/migrate.go, and runsc/ttcd/schemas for their shapes.
func init() {
	// The default policy: device files open as the Sentry's own devices.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/sentry/fsimpl/gofer.filesystemOptions",
		Fields: []string{"fd", "aname", "interop", "dfltuid", "dfltgid", "dcache", "forcePageCache", "limitHostFDTranslation", "overlayfsStaleRead", "regularFilesUseSpecialFileFD", "disableFifoOpen", "directfs"},
		Zero:   []string{"charDevicePolicy"},
	})
	// Older builds kept no device numbers: a device file that a mount held
	// across the upgrade opens as device 0:0, which fails with ENXIO, until it
	// is looked up again. Writers open across the upgrade are not counted: a
	// file that they hold open for writing can be executed meanwhile.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/sentry/fsimpl/gofer.inode",
		Fields: []string{"fs", "inoKey", "ino", "mode", "uid", "gid", "blockSize", "atime", "mtime", "ctime", "btime", "btimeValid", "size", "atimeDirty", "mtimeDirty", "nlink", "mappings", "cache", "dirty", "savedDeletedData", "mmapFile", "haveTarget", "target", "endpoint", "pipe", "locks", "watches", "refs", "impl"},
		Zero:   []string{"rdevMajor", "rdevMinor", "writeCount"},
	})
}
