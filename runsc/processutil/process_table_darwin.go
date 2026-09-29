// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package processutil

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// PIDs returns the PIDs of the host's processes.
func PIDs() ([]int, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	pids := make([]int, 0, len(procs))
	for i := range procs {
		if pid := int(procs[i].Proc.P_pid); pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func kinfoProc(pid int) (*unix.KinfoProc, error) {
	proc, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		// A PID with no process yields an empty record, which x/sys reports
		// as EIO.
		if errors.Is(err, unix.EIO) || errors.Is(err, unix.ESRCH) || errors.Is(err, unix.ENOENT) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	if proc == nil || proc.Proc.P_pid != int32(pid) {
		return nil, os.ErrNotExist
	}
	return proc, nil
}

// ParentPID returns the PID of pid's parent.
func ParentPID(pid int) (int, error) {
	proc, err := kinfoProc(pid)
	if err != nil {
		return 0, err
	}
	return int(proc.Eproc.Ppid), nil
}

// StartTime returns pid's start time.
func StartTime(pid int) (string, error) {
	proc, err := kinfoProc(pid)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d.%06d", proc.Proc.P_starttime.Sec, proc.Proc.P_starttime.Usec), nil
}

// Darwin process states from <sys/proc.h>; SZOMB is darwinSZomb.
const (
	darwinSIdl  = 1
	darwinSRun  = 2
	darwinSSlp  = 3
	darwinSStop = 4
)

// State returns pid's state.
func State(pid int) (byte, error) {
	proc, err := kinfoProc(pid)
	if err != nil {
		return 0, err
	}
	switch proc.Proc.P_stat {
	case darwinSZomb:
		return 'Z', nil
	case darwinSStop:
		return 'T', nil
	case darwinSRun:
		return 'R', nil
	case darwinSSlp:
		return 'S', nil
	case darwinSIdl:
		return 'I', nil
	default:
		return '?', nil
	}
}

// procArgs returns pid's executable path and argument vector, which Darwin
// reports through KERN_PROCARGS2: argc, the executable path, padding, then
// the arguments and the environment, each NUL-terminated.
func procArgs(pid int) (string, []string, error) {
	data, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		if _, statErr := kinfoProc(pid); errors.Is(statErr, os.ErrNotExist) {
			return "", nil, os.ErrNotExist
		}
		return "", nil, err
	}
	if len(data) < 4 {
		return "", nil, errors.New("malformed process arguments")
	}
	argc := int(binary.LittleEndian.Uint32(data))
	data = data[4:]
	end := bytes.IndexByte(data, 0)
	if end < 0 {
		return "", nil, errors.New("malformed process arguments")
	}
	executable := string(data[:end])
	data = data[end:]
	for len(data) != 0 && data[0] == 0 {
		data = data[1:]
	}
	arguments := make([]string, 0, argc)
	for len(arguments) < argc && len(data) != 0 {
		end := bytes.IndexByte(data, 0)
		if end < 0 {
			end = len(data)
		}
		arguments = append(arguments, string(data[:end]))
		data = data[min(end+1, len(data)):]
	}
	return executable, arguments, nil
}

// Arguments returns pid's argument vector.
func Arguments(pid int) ([]string, error) {
	_, arguments, err := procArgs(pid)
	return arguments, err
}

// Executable returns the path of pid's executable.
func Executable(pid int) (string, error) {
	executable, _, err := procArgs(pid)
	return executable, err
}
