// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package protocol contains the intentionally tiny TTC client/daemon wire
// contract shared by the thin client and the full daemon implementation.
package protocol

import (
	"strconv"
	"strings"
)

// DaemonAPIVersion is the exact native CLI protocol version. Official TTC
// clients also require an exact build match; SDKs negotiate their own API.
const DaemonAPIVersion = 10

// CommandUsesTerminalControl reports whether a CLI command can ask the client
// for terminal mode or size updates. This is part of the wire contract rather
// than a client implementation detail: every daemon relay must keep the
// request stream open for these commands even when guest stdin is detached.
// Keeping the classifier here prevents the lightweight client and ttcd's
// internal client from silently drifting apart.
func CommandUsesTerminalControl(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if args[0] == "container" {
		args = args[1:]
		if len(args) == 0 {
			return false
		}
	}
	switch args[0] {
	case "attach":
		// Whether the selected console is a TTY is known only by ttcd.
		return true
	case "start":
		return commandBoolFlag(args[1:], 'a', "--attach")
	case "run", "exec":
		if args[0] == "exec" && len(args) > 1 && (args[1] == "ls" || args[1] == "wait" || args[1] == "kill") {
			return false
		}
		return commandBoolFlag(args[1:], 't', "--tty")
	default:
		return false
	}
}

func commandBoolFlag(args []string, short byte, long string) bool {
	value := false
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == long {
			value = true
			continue
		}
		if strings.HasPrefix(arg, long+"=") {
			if parsed, err := strconv.ParseBool(strings.TrimPrefix(arg, long+"=")); err == nil {
				value = parsed
			}
			continue
		}
		if len(arg) > 1 && arg[0] == '-' && arg[1] != '-' && strings.ContainsRune(arg[1:], rune(short)) {
			value = true
		}
	}
	return value
}
