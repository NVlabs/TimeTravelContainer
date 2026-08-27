// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

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
