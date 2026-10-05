// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package console

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// A family's console follower is one process that keeps the logs of all the
// family's consoles that ttcd follows, those of branch inits and of detached
// exec sessions: their output, which the Sentry pushes, and their processes'
// wait statuses. ttcd talks to it over a SOCK_SEQPACKET socket in the family's
// console directory, one JSON message a packet. A console's lock comes with
// the request to follow it, and the follower holds it for as long as it does:
// the lock tells whether a console is followed, whoever follows it.

// The names start with a dot, which no branch's does.
const (
	// FollowerSocketName is the name of the follower's socket in the
	// family's console directory.
	FollowerSocketName = ".follower.sock"
	// FollowerLockName is the name of the lock the follower holds while it
	// runs, which keeps it one to a family.
	FollowerLockName = ".follower.lock"
)

// Operations ttcd asks of a follower.
const (
	// FollowerFollow follows the console Follow describes, with its lock
	// attached; the reply comes once the Sentry pushes the console.
	FollowerFollow = "follow"
	// FollowerStop stops following the console whose lock is Lock, or every
	// console if Lock is empty; the reply comes once each is released.
	FollowerStop = "stop"
	// FollowerObserve asks for a Done message once the console whose lock is
	// Lock is no longer followed, at once if it is not.
	FollowerObserve = "observe"
	// FollowerRetire stops following every console, as FollowerStop with no
	// Lock does, and has the follower exit once it has replied: its family's
	// runtime is gone, and the follower can follow only that runtime's
	// consoles.
	FollowerRetire = "retire"
)

// Follow describes a console to follow and the files it goes to.
type Follow struct {
	Branch    string `json:"branch"`
	ExecID    string `json:"exec_id,omitempty"`
	ConsoleID string `json:"console_id"`
	// Lock is the path of the console's lock, which identifies it.
	Lock           string `json:"lock"`
	Log            string `json:"log"`
	LogMaxBytes    int64  `json:"log_max_bytes"`
	LogMaxFiles    int    `json:"log_max_files"`
	RetentionStats string `json:"retention_stats"`
	WaitStatus     string `json:"wait_status"`
	// Error is where the follower records why following failed.
	Error string `json:"error"`
}

// FollowerMessage is one message between ttcd and a follower: a request
// with an Op and an ID, the reply to the request of that ID, or a Done
// notice, which the follower sends every connection when a console it
// followed is released.
type FollowerMessage struct {
	ID     uint64  `json:"id,omitempty"`
	Op     string  `json:"op,omitempty"`
	Follow *Follow `json:"follow,omitempty"`
	Lock   string  `json:"lock,omitempty"`
	Error  string  `json:"error,omitempty"`
	Done   string  `json:"done,omitempty"`
}

const (
	maxFollowerMessage = 64 << 10
	maxFollowerFiles   = 4
)

// WriteFollowerMessage sends message on conn, with files.
func WriteFollowerMessage(conn *net.UnixConn, message FollowerMessage, files ...*os.File) error {
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if len(data) > maxFollowerMessage || len(files) > maxFollowerFiles {
		return fmt.Errorf("console follower message of %d bytes and %d files is too large", len(data), len(files))
	}
	var rights []byte
	if len(files) != 0 {
		fds := make([]int, len(files))
		for i, file := range files {
			fds[i] = int(file.Fd())
		}
		rights = unix.UnixRights(fds...)
	}
	return sendFollowerPacket(conn, data, rights)
}

// ReadFollowerMessage receives one message from conn, and the files sent with
// it, which the caller owns. It returns io.EOF once the peer has closed.
func ReadFollowerMessage(conn *net.UnixConn) (FollowerMessage, []*os.File, error) {
	rights := make([]byte, unix.CmsgSpace(4*maxFollowerFiles))
	data, n, rightsn, flags, err := recvFollowerPacket(conn, rights)
	var files []*os.File
	if rightsn != 0 {
		controls, parseErr := unix.ParseSocketControlMessage(rights[:rightsn])
		for i := range controls {
			fds, rightsErr := unix.ParseUnixRights(&controls[i])
			for _, fd := range fds {
				files = append(files, os.NewFile(uintptr(fd), "console-follower-file"))
			}
			parseErr = errors.Join(parseErr, rightsErr)
		}
		if parseErr != nil && err == nil {
			err = fmt.Errorf("parsing console follower message rights: %w", parseErr)
		}
	}
	if err == nil && flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 {
		err = errors.New("console follower message was truncated")
	}
	var message FollowerMessage
	if err == nil {
		err = json.Unmarshal(data[:n], &message)
	}
	if err != nil {
		for _, file := range files {
			_ = file.Close()
		}
		return FollowerMessage{}, nil, err
	}
	return message, files, nil
}
