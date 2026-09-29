// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package lisafs

import (
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux/errno"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/log"
)

func protocolError(e uint32) error {
	if e != 0 && !linuxerr.IsValid(errno.Errno(e)) {
		log.Warningf("lisafs: server reported invalid Linux errno %d", e)
		return linuxerr.EIO
	}
	return linuxerr.ErrorFromUnix(unix.Errno(e))
}
