// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package processutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// PIDs returns the PIDs of the host's processes.
func PIDs() ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	pids := make([]int, 0, len(entries))
	for _, entry := range entries {
		if pid, err := strconv.Atoi(entry.Name()); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

// statFields returns the fields of /proc/pid/stat after the command name,
// which is in parentheses and may hold spaces and parentheses itself; the
// first is the state.
func statFields(pid int) ([]string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil, err
	}
	end := strings.LastIndex(string(data), ") ")
	if end < 0 {
		return nil, errors.New("malformed process stat")
	}
	return strings.Fields(string(data)[end+2:]), nil
}

// ParentPID returns the PID of pid's parent.
func ParentPID(pid int) (int, error) {
	fields, err := statFields(pid)
	if err != nil {
		return 0, err
	}
	if len(fields) < 2 {
		return 0, errors.New("process stat omits parent")
	}
	return strconv.Atoi(fields[1])
}

// StartTime returns pid's start time.
func StartTime(pid int) (string, error) {
	fields, err := statFields(pid)
	if err != nil {
		return "", err
	}
	if len(fields) <= 19 {
		return "", errors.New("process stat omits start time")
	}
	return fields[19], nil
}

// State returns pid's state.
func State(pid int) (byte, error) {
	fields, err := statFields(pid)
	if err != nil {
		return 0, err
	}
	if len(fields) == 0 || len(fields[0]) != 1 {
		return 0, errors.New("process stat omits state")
	}
	return fields[0][0], nil
}

// Arguments returns pid's argument vector.
func Arguments(pid int) ([]string, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	return strings.Split(strings.TrimRight(string(data), "\x00"), "\x00"), nil
}

// Executable returns the path of pid's executable.
func Executable(pid int) (string, error) {
	return os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
}
