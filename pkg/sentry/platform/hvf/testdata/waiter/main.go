// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "write":
			if len(os.Args) != 4 {
				fmt.Fprintln(os.Stderr, "usage: waiter write PATH DATA")
				os.Exit(2)
			}
			if err := os.WriteFile(os.Args[2], []byte(os.Args[3]), 0644); err != nil {
				fmt.Fprintf(os.Stderr, "write: %v\n", err)
				os.Exit(1)
			}
		case "read":
			if len(os.Args) != 3 {
				fmt.Fprintln(os.Stderr, "usage: waiter read PATH")
				os.Exit(2)
			}
			data, err := os.ReadFile(os.Args[2])
			if err != nil {
				fmt.Fprintf(os.Stderr, "read: %v\n", err)
				os.Exit(1)
			}
			fmt.Println(string(data))
		default:
			fmt.Println(strings.Join(os.Args[1:], " "))
		}
		return
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGUSR1)
	fmt.Println("waiter ready")
	for {
		if sig := <-signals; sig == syscall.SIGTERM {
			return
		}
	}
}
