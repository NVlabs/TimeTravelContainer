// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Program netprobe is a static Linux/ARM64 payload for end-to-end TTC NAT
// smoke tests. The host test listener sends one short banner and closes.
package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: netprobe HOST:PORT")
		os.Exit(2)
	}
	conn, err := net.DialTimeout("tcp4", os.Args[1], 5*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		fmt.Fprintf(os.Stderr, "deadline: %v\n", err)
		os.Exit(1)
	}
	banner, err := io.ReadAll(conn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("NAT:%s\n", banner)
}
