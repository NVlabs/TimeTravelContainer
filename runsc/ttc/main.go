// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Binary ttc is the TimeTravelContainer container manager command-line client.
package main

import (
	"os"

	"gvisor.dev/gvisor/runsc/ttc/client"
)

func main() {
	os.Exit(client.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
