// Copyright 2026 The gVisor Authors.
// SPDX-License-Identifier: Apache-2.0

// page_worker is the pure-Go equivalent of page_worker.c. It exists so hosts
// without a static Linux C cross-toolchain can still run TTC's distributed
// memory qualification against a native Linux guest workload.
package main

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

const pageSize = 4096

func failUsage(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}

func writeMarker(path string, value uint64) {
	if err := os.WriteFile(path, []byte(strconv.FormatUint(value, 10)+"\n"), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
		os.Exit(1)
	}
}

func main() {
	if len(os.Args) != 5 {
		failUsage("usage: %s MEMORY_MIB sequential|random TOUCH_FRACTION ROUNDS", os.Args[0])
	}
	memoryMiB, err := strconv.ParseUint(os.Args[1], 10, 64)
	if err != nil || memoryMiB == 0 || memoryMiB > uint64(^uint(0)>>1)/(1024*1024) {
		failUsage("invalid memory size")
	}
	randomAccess := os.Args[2] == "random"
	if !randomAccess && os.Args[2] != "sequential" {
		failUsage("invalid access pattern")
	}
	fraction, err := strconv.ParseFloat(os.Args[3], 64)
	if err != nil || fraction <= 0 || fraction > 1 {
		failUsage("invalid touch fraction")
	}
	rounds, err := strconv.ParseUint(os.Args[4], 10, 64)
	if err != nil || rounds == 0 {
		failUsage("invalid round count")
	}

	memory := make([]byte, int(memoryMiB)*1024*1024)
	pages := len(memory) / pageSize
	for page := 0; page < pages; page++ {
		memory[page*pageSize] = byte(page*131 + 17)
	}
	writeMarker("/tmp/ttc-bench-ready", uint64(os.Getpid()))
	if err := syscall.Kill(os.Getpid(), syscall.SIGSTOP); err != nil {
		fmt.Fprintf(os.Stderr, "SIGSTOP: %v\n", err)
		os.Exit(1)
	}

	accesses := uint64(float64(pages) * fraction)
	if accesses == 0 {
		accesses = 1
	}
	state := uint64(0x9e3779b97f4a7c15)
	var sum uint64
	for round := uint64(0); round < rounds; round++ {
		for access := uint64(0); access < accesses; access++ {
			page := access % uint64(pages)
			if randomAccess {
				state ^= state << 13
				state ^= state >> 7
				state ^= state << 17
				page = state % uint64(pages)
			}
			sum += uint64(memory[int(page)*pageSize])
		}
	}
	writeMarker("/tmp/ttc-bench-done", sum)
	runtime.KeepAlive(memory)
	for {
		time.Sleep(time.Hour)
	}
}
