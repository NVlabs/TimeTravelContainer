// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Program hello is a static Linux/ARM64 payload for native-HVF integration
// smoke tests. It intentionally depends only on the Go runtime and writes one
// line before exiting.
package main

import "os"

func main() {
	_, _ = os.Stdout.WriteString("hello from linux arm64 under hvf\n")
}
