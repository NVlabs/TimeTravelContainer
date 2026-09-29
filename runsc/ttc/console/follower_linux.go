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

func recvFollowerPacket(conn *net.UnixConn, data, rights []byte) (n, rightsn, flags int, err error) {
	n, rightsn, flags, _, err = conn.ReadMsgUnix(data, rights)
	return n, rightsn, flags, err
}
