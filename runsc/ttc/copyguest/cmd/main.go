// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Binary copy_helper_guest is executed inside a Linux container by the
// Sentry.
package main

import (
	"os"

	"gvisor.dev/gvisor/runsc/ttc/copyhelper"
)

func main() {
	args := os.Args[1:]
	if len(args) != 0 && args[0] == "_cp-helper" {
		args = args[1:]
	}
	os.Exit(copyhelper.Run(args, os.Stdin, os.Stdout, os.Stderr))
}
