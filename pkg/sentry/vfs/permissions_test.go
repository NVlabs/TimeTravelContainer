// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package vfs

import (
	"testing"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
)

// The "security" namespace is CAP_SYS_ADMIN's to write, as in Linux without
// a security module, but for security.capability, and anyone's to read.
func TestCheckXattrPermissionsSecurity(t *testing.T) {
	userns := auth.NewRootUserNamespace()
	root := auth.NewRootCredentials(userns)
	user := auth.NewUserCredentials(1000, 1000, nil, nil, userns)
	const mode = linux.ModeRegular | 0644
	for _, test := range []struct {
		name  string
		creds *auth.Credentials
		ats   AccessTypes
		xattr string
		want  error
	}{
		{"root writes selinux", root, MayWrite, "security.selinux", nil},
		{"user writes selinux", user, MayWrite, "security.selinux", linuxerr.EPERM},
		{"user reads selinux", user, MayRead, "security.selinux", nil},
		{"user writes capability", user, MayWrite, linux.XATTR_SECURITY_CAPABILITY, nil},
	} {
		if err := CheckXattrPermissions(test.creds, test.ats, mode, 0, test.xattr); err != test.want {
			t.Errorf("%s: CheckXattrPermissions = %v, want %v", test.name, err, test.want)
		}
	}
}
