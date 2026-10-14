// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"gvisor.dev/gvisor/runsc/ttc/copyhelper"
	"gvisor.dev/gvisor/runsc/ttc/protocol"
)

// runCreateValueFlags are the options of run and create that take a value.
var runCreateValueFlags = map[string]bool{
	"--name": true, "--pull": true, "--workdir": true, "-w": true,
	"--user": true, "-u": true, "--hostname": true, "-h": true,
	"--network": true, "--net": true, "--ip": true, "--mac-address": true,
	"--interface-name": true,
	"--publish":        true, "-p": true, "--add-host": true, "--dns": true,
	"--dns-search": true, "--dns-option": true, "--dns-opt": true,
	"--entrypoint": true, "--stop-signal": true, "--stop-timeout": true,
	"--pids-limit": true, "--memory": true, "-m": true, "--memory-swap": true,
	"--cpus": true, "--cpu-shares": true, "--cpu-period": true, "--cpu-quota": true,
	"--shm-size": true, "--group-add": true, "--cap-add": true,
	"--cap-drop": true, "--ulimit": true, "--env": true, "-e": true,
	"--env-file": true, "--label": true, "-l": true, "--label-file": true,
	"--volume": true, "-v": true, "--mount": true, "--tmpfs": true,
	"--log-driver": true, "--log-opt": true, "--node": true, "--platform": true,
	"--gfx-fork": true, "--nvproxy-fork": true,
}

// readKeyValueFileOptions reads a run's or a create's --env-file and
// --label-file here, as Docker does, and passes what they say as --env and
// --label options, ahead of the command's own, which ttcd lets take
// precedence over a file's. ttcd would read them as its own user and in its
// own filesystem namespace, which a gateway's, or a daemon shared with a
// group, need not share with the client.
func readKeyValueFileOptions(args []string) ([]string, error) {
	command := 0
	if len(args) != 0 && args[0] == "container" {
		command = 1
	}
	if command >= len(args) || (args[command] != "run" && args[command] != "create") {
		return args, nil
	}
	var fileEnv, fileLabels, options []string
	i := command + 1
	for ; i < len(args); i++ {
		arg := args[i]
		if arg == "--" || arg == "-" || !strings.HasPrefix(arg, "-") {
			break
		}
		name, value, inline := strings.Cut(arg, "=")
		if name == "--env-file" || name == "--label-file" {
			if !inline {
				if i+1 == len(args) {
					return nil, fmt.Errorf("%s requires a file", name)
				}
				i++
				value = args[i]
			}
			kind := "environment"
			if name == "--label-file" {
				kind = "label"
			}
			values, err := readKeyValueFile(value, kind)
			if err != nil {
				return nil, err
			}
			if name == "--env-file" {
				fileEnv = append(fileEnv, values...)
			} else {
				fileLabels = append(fileLabels, values...)
			}
			continue
		}
		options = append(options, arg)
		if !inline && runCreateValueFlags[arg] && i+1 < len(args) {
			i++
			options = append(options, args[i])
		}
	}
	if len(fileEnv)+len(fileLabels) == 0 {
		return args, nil
	}
	result := append([]string(nil), args[:command+1]...)
	for _, value := range fileEnv {
		result = append(result, "--env", resolveEnvironmentImport(value))
	}
	for _, value := range fileLabels {
		result = append(result, "--label", value)
	}
	result = append(result, options...)
	return append(result, args[i:]...), nil
}

// readKeyValueFile reads the lines of an environment or a label file, as
// ttcd's readKeyValueFiles does: but blank ones and comments.
func readKeyValueFile(name, kind string) ([]string, error) {
	file, err := os.Open(name)
	if err != nil {
		return nil, fmt.Errorf("opening %s file %q: %w", kind, name, err)
	}
	defer file.Close()
	var values []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if lineNumber == 1 {
			line = strings.TrimPrefix(line, "\ufeff")
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		values = append(values, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading %s file %q: %w", kind, name, err)
	}
	return values, nil
}

// resolveEnvironmentImport gives a variable named without a value the value
// this process has, if any.
func resolveEnvironmentImport(value string) string {
	if strings.Contains(value, "=") {
		return value
	}
	if hostValue, ok := os.LookupEnv(value); ok {
		return value + "=" + hostValue
	}
	return value
}

// sharedDaemonMember reports whether this process reaches the daemon at
// socket as a member of the group another user shares it with.
func sharedDaemonMember(socket string) bool {
	info, err := os.Stat(socket)
	if err != nil {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) != os.Geteuid()
}

// copyContainerOperand reports whether a cp operand names a container's path,
// as ttcd tells one: CONTAINER@BRANCH:PATH, with no slash before the colon.
func copyContainerOperand(value string) bool {
	colon := strings.IndexByte(value, ':')
	return colon > 0 && !strings.ContainsRune(value[:colon], filepath.Separator)
}

// prepareMemberCopy has ttc, for a member of the group a daemon is shared
// with, copy to or from a host path itself, as ttcd opens none for a member:
// it writes the archive of a host source, as ttcd would, and passes it as a
// cp from '-', or extracts at a host destination the archive that a cp to '-'
// writes. finish, if not nil, waits for its side of the copy and returns the
// command's status, with its own error.
func prepareMemberCopy(args []string, stdin io.Reader, stdout, stderr io.Writer) ([]string, io.Reader, io.Writer, func(int) int, error) {
	if len(args) == 0 || args[0] != "cp" {
		return args, stdin, stdout, nil, nil
	}
	var archive, followLink bool
	i := 1
flags:
	for ; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			i++
			break
		}
		switch {
		case arg == "-a" || arg == "--archive":
			archive = true
		case arg == "-L" || arg == "--follow-link":
			followLink = true
		case len(arg) > 2 && arg[0] == '-' && arg[1] != '-' && strings.Trim(arg[1:], "aL") == "":
			archive = archive || strings.ContainsRune(arg, 'a')
			followLink = followLink || strings.ContainsRune(arg, 'L')
		default:
			break flags
		}
	}
	// Anything else, ttcd answers as it would.
	if len(args)-i != 2 {
		return args, stdin, stdout, nil, nil
	}
	source, destination := args[i], args[i+1]
	if copyContainerOperand(source) == copyContainerOperand(destination) {
		return args, stdin, stdout, nil, nil
	}
	if !copyContainerOperand(source) {
		if source == "-" {
			return args, stdin, stdout, nil, nil
		}
		physical := filepath.Clean(strings.TrimSuffix(source, string(filepath.Separator)+"."))
		if _, err := os.Lstat(physical); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("copying %s: %w", source, err)
		}
		reader, writer := io.Pipe()
		written := make(chan error, 1)
		go func() {
			err := copyhelper.WriteArchive(source, followLink, writer)
			_ = writer.CloseWithError(err)
			written <- err
		}()
		finish := func(status int) int {
			_ = reader.Close()
			if err := <-written; err != nil && status == 0 {
				fmt.Fprintf(stderr, "ttc: copying %s: %v\n", source, err)
				return 1
			}
			return status
		}
		result := []string{"cp", protocol.CopyPreparedInput}
		if archive {
			result = append(result, "--archive")
		}
		return append(result, "--", "-", destination), reader, stdout, finish, nil
	}
	if destination == "-" {
		return args, stdin, stdout, nil, nil
	}
	reader, writer := io.Pipe()
	extracted := make(chan error, 1)
	go func() {
		err := copyhelper.ExtractArchive(reader, destination, archive, os.Geteuid(), os.Getegid(), false)
		// Once extraction stops, the rest of the archive is read away, so
		// that the command still ends with ttcd's status.
		_, _ = io.Copy(io.Discard, reader)
		extracted <- err
	}()
	finish := func(status int) int {
		_ = writer.Close()
		if err := <-extracted; err != nil && status == 0 {
			fmt.Fprintf(stderr, "ttc: copying to %s: %v\n", destination, err)
			return 1
		}
		return status
	}
	result := []string{"cp"}
	if followLink {
		result = append(result, "--follow-link")
	}
	// The command reads nothing from the client.
	return append(result, "--", source, "-"), strings.NewReader(""), writer, finish, nil
}
