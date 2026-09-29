// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package specutils

import (
	"os"
	"path/filepath"
)

func hostExecutablePath() string {
	path, err := os.Executable()
	if err != nil {
		panic("resolving current executable on macOS: " + err.Error())
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return path
}
