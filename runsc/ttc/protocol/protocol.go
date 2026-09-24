// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package protocol contains the intentionally tiny TTC client/daemon wire
// contract shared by the thin client and the full daemon implementation.
package protocol

import (
	"strconv"
	"strings"
	"time"
)

// DaemonAPIVersion is the exact native CLI protocol version. Official TTC
// clients also require an exact build match; SDKs negotiate their own API.
const DaemonAPIVersion = 10

// DaemonQuorumWait is the bounded pre-admission status published by a daemon
// that has opened its persistent Raft transport but cannot yet regain quorum.
type DaemonQuorumWait struct {
	PID        int    `json:"pid"`
	APIVersion int    `json:"api_version"`
	BuildID    string `json:"build_id"`
}

// DaemonQuorumWaitPath returns the advisory local-startup marker used while a
// persistent Raft member is alive but cannot yet regain quorum. The marker is
// deliberately outside the RPC stream: the full daemon request loop remains
// unavailable until the node has recovered execution authority.
func DaemonQuorumWaitPath(socket string) string {
	return socket + ".quorum-wait"
}

// DaemonQuorumWaitPatience is how long a request waits for a daemon waiting
// for quorum to begin serving: long enough for a starting cluster to elect
// its leader, where a cluster that is away may keep the daemon waiting for as
// long as it lasts.
const DaemonQuorumWaitPatience = 30 * time.Second

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
