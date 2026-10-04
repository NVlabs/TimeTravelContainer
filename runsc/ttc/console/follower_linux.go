// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package console

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// FollowerNetwork is the socket type of a follower's socket: each message is
// one SOCK_SEQPACKET packet.
const FollowerNetwork = "unixpacket"

// SocketAddress returns a name for the socket at path that fits in a socket
// address however long path is, through the directory's descriptor, and a
// function to call once the socket is bound or connected.
func SocketAddress(path string) (string, func(), error) {
	dir, err := unix.Open(filepath.Dir(path), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", nil, &os.PathError{Op: "open", Path: filepath.Dir(path), Err: err}
	}
	return fmt.Sprintf("/proc/self/fd/%d/%s", dir, filepath.Base(path)), func() { _ = unix.Close(dir) }, nil
}

func sendFollowerPacket(conn *net.UnixConn, data, rights []byte) error {
	n, rightsn, err := conn.WriteMsgUnix(data, rights, nil)
	if err != nil {
		return err
	}
	if n != len(data) || rightsn != len(rights) {
		return io.ErrShortWrite
	}
	return nil
}

// recvFollowerPacket receives one packet into a buffer it makes the packet's
// size, once a packet has come, and rights. A connection waiting for a packet
// holds no buffer: a daemon kept one of maxFollowerMessage bytes for each
// family's follower while it waited, 6 MB for 100 families.
func recvFollowerPacket(conn *net.UnixConn, rights []byte) (data []byte, n, rightsn, flags int, err error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return nil, 0, 0, 0, err
	}
	// A peek of no bytes, with MSG_TRUNC, gives a packet's size: 0 once the
	// peer has closed, which the read below takes for the end.
	size := 0
	var peekErr error
	if err := raw.Read(func(fd uintptr) bool {
		size, _, peekErr = unix.Recvfrom(int(fd), nil, unix.MSG_PEEK|unix.MSG_TRUNC|unix.MSG_DONTWAIT)
		return peekErr != unix.EAGAIN
	}); err != nil {
		return nil, 0, 0, 0, err
	}
	if peekErr != nil {
		return nil, 0, 0, 0, peekErr
	}
	// A packet larger than any message reads truncated, which its flags tell.
	data = make([]byte, min(size, maxFollowerMessage))
	n, rightsn, flags, _, err = conn.ReadMsgUnix(data, rights)
	return data, n, rightsn, flags, err
}
