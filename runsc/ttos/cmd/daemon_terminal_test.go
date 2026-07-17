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

package cmd

import (
	"bufio"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDaemonTerminalSizeRoundTrip(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()
	var serverWriteMu sync.Mutex
	stdinReader, stdinWriter := io.Pipe()
	defer stdinReader.Close()
	terminal := newDaemonTerminalInput(stdinReader, serverConn, &serverWriteMu)
	go receiveDaemonInput(bufio.NewReader(serverConn), stdinWriter, true, terminal)

	result := make(chan struct {
		cols int
		rows int
		ok   bool
		err  error
	}, 1)
	go func() {
		cols, rows, ok, err := terminal.terminalSize()
		result <- struct {
			cols int
			rows int
			ok   bool
			err  error
		}{cols, rows, ok, err}
	}()

	var clientWriteMu sync.Mutex
	if err := writeDaemonFrame(clientConn, &clientWriteMu, daemonFrameStdin, []byte("queued-before-attach\n")); err != nil {
		t.Fatal(err)
	}
	typ, payload, err := readDaemonFrame(clientConn)
	if err != nil {
		t.Fatal(err)
	}
	if typ != daemonFrameTerminal || len(payload) != 1 || payload[0] != 0 {
		t.Fatalf("terminal request type=%q payload=%v", typ, payload)
	}
	var size [daemonTerminalSizeBytes]byte
	binary.BigEndian.PutUint32(size[0:4], 24)
	binary.BigEndian.PutUint32(size[4:8], 80)
	if err := writeDaemonFrame(clientConn, &clientWriteMu, daemonFrameResize, size[:]); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		if got.err != nil || !got.ok || got.cols != 80 || got.rows != 24 {
			t.Fatalf("terminalSize()=(%d,%d,%t,%v)", got.cols, got.rows, got.ok, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("terminal size round trip timed out")
	}
	resizeEvents := terminal.terminalResizeEvents()
	terminal.updateSize(40, 100)
	select {
	case <-resizeEvents:
	default:
		t.Fatal("terminal resize event was not published after watching began")
	}
}

func TestDaemonClientNonTerminalReportsUnsupportedSize(t *testing.T) {
	var typ byte
	var payload []byte
	session := newDaemonClientTerminal(strings.NewReader(""), func(gotType byte, gotPayload []byte) error {
		typ = gotType
		payload = append([]byte(nil), gotPayload...)
		return nil
	})
	defer session.Close()
	if err := session.configure([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if typ != daemonFrameResize {
		t.Fatalf("frame type=%q, want resize", typ)
	}
	rows, cols, err := decodeDaemonTerminalSize(payload)
	if err != nil {
		t.Fatal(err)
	}
	if rows != 0 || cols != 0 {
		t.Fatalf("non-terminal size=(%d,%d), want unsupported zeros", rows, cols)
	}
}

func TestDecodeDaemonTerminalSizeRejectsMalformedFrame(t *testing.T) {
	if _, _, err := decodeDaemonTerminalSize([]byte{1, 2, 3}); err == nil {
		t.Fatal("decodeDaemonTerminalSize accepted a malformed frame")
	}
}
