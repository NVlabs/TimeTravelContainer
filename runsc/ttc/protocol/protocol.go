// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package protocol contains the intentionally tiny TTC client/daemon wire
// contract shared by the thin client and the full daemon implementation.
package protocol

import (
	"strconv"
	"strings"
	"time"
)

// DaemonAPIVersion is the exact version of ttcd's API, which every request
// names and ttcd requires. Official TTC clients also require an exact build
// match; the SDKs (sdk/) name this version too, and must change with it.
const DaemonAPIVersion = 10

// StaleRouteRefusal begins a node's answer to a command routed to it by a
// placement that another command has changed since: it moved the command's
// branch, unplaced it, or claims it. The node refuses before it runs or
// journals the command, which may therefore go again under its request ID.
const StaleRouteRefusal = "ttcd: stale routed command:"

// StaleRouteWait bounds how long a client, or a node that routed a command,
// sends the command again while nodes refuse it as routed stale.
const StaleRouteWait = time.Minute

// StaleRouteAnswer reports whether stderr, all that a command's answer wrote
// there, is a node's refusal of a stale route and nothing else.
func StaleRouteAnswer(stderr string) bool {
	return strings.HasPrefix(stderr, StaleRouteRefusal) && strings.Count(stderr, "\n") == 1 && strings.HasSuffix(stderr, "\n")
}

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
