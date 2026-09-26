// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build darwin

package console

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// FollowerNetwork is the socket type of a follower's socket. Darwin has no
// AF_UNIX SOCK_SEQPACKET, so each message is a length-prefixed frame on a
// stream, with its files attached to the frame's first segment.
const FollowerNetwork = "unix"

// darwinUnixPathMax is the size of sockaddr_un.sun_path, terminator included.
const darwinUnixPathMax = 104

// SocketAddress returns a name for the socket at path that fits in a socket
// address however long path is, and a function to call once the socket is
// bound or connected. Darwin has no /proc/self/fd, so a long path is reached
// through a short, private symbolic link to its directory.
func SocketAddress(path string) (string, func(), error) {
	if len(path) < darwinUnixPathMax {
		return path, func() {}, nil
	}
	links := filepath.Join(os.TempDir(), "ttc-socket-links")
	if err := os.MkdirAll(links, 0700); err != nil {
		return "", nil, err
	}
	var name [8]byte
	if _, err := rand.Read(name[:]); err != nil {
		return "", nil, err
	}
	link := filepath.Join(links, hex.EncodeToString(name[:]))
	address := filepath.Join(link, filepath.Base(path))
	if len(address) >= darwinUnixPathMax {
		return "", nil, fmt.Errorf("socket path %q is too long for macOS even through %q", path, link)
	}
	if err := os.Symlink(filepath.Dir(path), link); err != nil {
		return "", nil, err
	}
	return address, func() { _ = os.Remove(link) }, nil
}

// followerWriteLocks serializes frames written to one stream. Writers are
// concurrent (requests and Done notices), and a frame larger than the socket
// buffer is written in several calls. Striping avoids per-connection state.
var followerWriteLocks [64]sync.Mutex

func followerWriteLock(conn *net.UnixConn) *sync.Mutex {
	return &followerWriteLocks[(uintptr(unsafe.Pointer(conn))>>4)%uintptr(len(followerWriteLocks))]
}

func sendFollowerPacket(conn *net.UnixConn, data, rights []byte) error {
	frame := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(frame, uint32(len(data)))
	copy(frame[4:], data)
	mu := followerWriteLock(conn)
	mu.Lock()
	defer mu.Unlock()
	n, rightsn, err := conn.WriteMsgUnix(frame, rights, nil)
	if err != nil {
		return err
	}
	if rightsn != len(rights) {
		return io.ErrShortWrite
	}
	for n < len(frame) {
		m, err := conn.Write(frame[n:])
		if err != nil {
			return err
		}
		n += m
	}
	return nil
}

// recvFollowerPacket reads one frame into data. Files may arrive with any
// segment of the frame; they are collected into rights.
func recvFollowerPacket(conn *net.UnixConn, data, rights []byte) (n, rightsn, flags int, err error) {
	var header [4]byte
	readFull := func(buf []byte, atStart bool) error {
		for read := 0; read < len(buf); {
			m, oobn, recvFlags, _, err := conn.ReadMsgUnix(buf[read:], rights[rightsn:])
			rightsn += oobn
			flags |= recvFlags
			read += m
			if err != nil {
				if errors.Is(err, io.EOF) && (read != 0 || !atStart) {
					return io.ErrUnexpectedEOF
				}
				return err
			}
			if m == 0 {
				if read != 0 || !atStart {
					return io.ErrUnexpectedEOF
				}
				return io.EOF
			}
		}
		return nil
	}
	if err := readFull(header[:], true); err != nil {
		return 0, rightsn, flags, err
	}
	length := int(binary.BigEndian.Uint32(header[:]))
	if length > len(data) {
		return 0, rightsn, flags | unix.MSG_TRUNC, nil
	}
	if err := readFull(data[:length], false); err != nil {
		return 0, rightsn, flags, err
	}
	return length, rightsn, flags, nil
}
