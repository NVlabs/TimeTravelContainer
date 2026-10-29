// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gvisor.dev/gvisor/runsc/container"
)

// applicationHugePages returns how much memory the applications of the
// sandbox of physical container id have mapped, and how much of it the host
// maps with huge pages: what the host's smaps say of the sandbox's stub
// processes, whose address spaces are the applications'. Each is a child of
// the Sentry, whose own children share its address space and are not counted
// again; the Sentry's own mapping of the sandbox's memory, which covers what
// it has touched, is left out. ok is false if the sandbox does not run on
// this host.
func (b *runtimeBackend) applicationHugePages(id string) (mapped, huge uint64, ok bool) {
	if !embeddedRuntime(b.runsc) {
		return 0, 0, false
	}
	full := container.FullID{SandboxID: id, ContainerID: id}
	if held, err := container.StateFileHeld(b.root, full); err != nil || held {
		return 0, 0, false
	}
	c, err := container.Load(b.root, full, container.LoadOpts{Exact: true, SkipCheck: true, ReadOnly: true})
	if err != nil || c.Status == container.Stopped || c.SandboxPid() <= 0 {
		return 0, 0, false
	}
	for _, stub := range processChildren()[c.SandboxPid()] {
		rss, pmd, err := smapsRollup(stub)
		if err != nil {
			continue
		}
		mapped += rss
		huge += pmd
		ok = true
	}
	return mapped, huge, ok
}

// processChildren returns the host's processes by their parents.
func processChildren() map[int][]int {
	children := make(map[int][]int)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return children
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if err != nil {
			continue
		}
		// The parent follows the state, after the command name.
		end := strings.LastIndexByte(string(stat), ')')
		if end < 0 {
			continue
		}
		fields := strings.Fields(string(stat[end+1:]))
		if len(fields) < 2 {
			continue
		}
		if ppid, err := strconv.Atoi(fields[1]); err == nil {
			children[ppid] = append(children[ppid], pid)
		}
	}
	return children
}

// smapsRollup returns the resident memory of process pid, and how much of it
// the host maps with huge pages, from /proc/PID/smaps_rollup.
func smapsRollup(pid int) (rss, huge uint64, err error) {
	file, err := os.Open(filepath.Join("/proc", strconv.Itoa(pid), "smaps_rollup"))
	if err != nil {
		return 0, 0, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, found := strings.Cut(scanner.Text(), ":")
		if !found {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			continue
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "Rss":
			rss = kb << 10
		case "AnonHugePages", "ShmemPmdMapped", "FilePmdMapped":
			huge += kb << 10
		}
	}
	return rss, huge, scanner.Err()
}
