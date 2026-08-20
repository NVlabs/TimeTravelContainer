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
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDaemonJSONFrameWriterFramesCompleteObjects(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	var writeMu sync.Mutex
	w := &daemonJSONFrameWriter{conn: server, mu: &writeMu, typ: daemonFrameTTYState}
	done := make(chan error, 1)
	go func() {
		if _, err := w.Write([]byte(`{"version":1`)); err != nil {
			done <- err
			return
		}
		_, err := w.Write([]byte("}\n{\"version\":2}\n"))
		done <- err
	}()
	for want := 1; want <= 2; want++ {
		typ, payload, err := readDaemonFrame(client)
		if err != nil {
			t.Fatal(err)
		}
		if typ != daemonFrameTTYState {
			t.Fatalf("frame type=%q, want %q", typ, daemonFrameTTYState)
		}
		var got struct {
			Version int `json:"version"`
		}
		if err := json.Unmarshal(payload, &got); err != nil {
			t.Fatalf("payload %q: %v", payload, err)
		}
		if got.Version != want {
			t.Fatalf("version=%d, want %d", got.Version, want)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDaemonStdinFlowControlWindow(t *testing.T) {
	flow := newDaemonStdinFlow()
	defer flow.close()
	if err := flow.reserve(daemonStdinWindowBytes); err != nil {
		t.Fatal(err)
	}
	reserved := make(chan error, 1)
	go func() { reserved <- flow.reserve(1) }()
	select {
	case err := <-reserved:
		t.Fatalf("reservation passed a full window: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := flow.acknowledge(1); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-reserved:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reservation did not resume after acknowledgement")
	}
	if err := flow.acknowledge(daemonStdinWindowBytes); err != nil {
		t.Fatal(err)
	}
	if err := flow.acknowledge(1); err == nil {
		t.Fatal("flow accepted acknowledgement with no outstanding bytes")
	}
	if err := flow.reserve(daemonStdinWindowBytes + 1); err == nil {
		t.Fatal("flow accepted a reservation larger than the window")
	}
}

func TestDaemonStdinQueueIsBounded(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	queue := newDaemonStdinQueue(writer, nil)
	if !queue.enqueue(make([]byte, daemonStdinWindowBytes)) {
		t.Fatal("queue rejected one full window")
	}
	if queue.enqueue([]byte{1}) {
		t.Fatal("queue accepted bytes beyond its window")
	}
	queue.closeWithError(nil)
}

func TestDaemonStdinAcknowledgementCodec(t *testing.T) {
	payload := encodeDaemonStdinAcknowledgement(32768)
	got, err := decodeDaemonStdinAcknowledgement(payload)
	if err != nil || got != 32768 {
		t.Fatalf("acknowledgement round trip=(%d,%v)", got, err)
	}
	if _, err := decodeDaemonStdinAcknowledgement(payload[:3]); err == nil {
		t.Fatal("decoder accepted malformed acknowledgement")
	}
}

// A client that vanishes has its acknowledged input delivered, and then the
// commands reading it are told it detached rather than that its input ended.
//
// The difference decides whether the session survives. "Input ended" is what a
// client says when it means "keep showing me the output", so an attach told
// that goes on waiting — holding the terminal's stdin against everyone, the
// same client included, for as long as the session stays quiet.
func TestDaemonClientCrashDeliversAcknowledgedStdinThenDetaches(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	stdinReader, stdinWriter := io.Pipe()
	var serverWriteMu sync.Mutex
	terminal := newDaemonTerminalInput(stdinReader, serverConn, &serverWriteMu)
	receiverDone := make(chan struct{})
	go func() {
		receiveDaemonInput(bufio.NewReader(serverConn), stdinWriter, true, terminal)
		close(receiverDone)
	}()
	readDone := make(chan struct {
		data []byte
		err  error
	}, 1)
	go func() {
		data, err := io.ReadAll(stdinReader)
		readDone <- struct {
			data []byte
			err  error
		}{data, err}
	}()
	var clientWriteMu sync.Mutex
	if err := writeDaemonFrame(clientConn, &clientWriteMu, daemonFrameStdin, []byte("accepted")); err != nil {
		t.Fatal(err)
	}
	typ, payload, err := readDaemonFrame(clientConn)
	if err != nil {
		t.Fatal(err)
	}
	ack, err := decodeDaemonStdinAcknowledgement(payload)
	if err != nil || typ != daemonFrameStdinAck || ack != len("accepted") {
		t.Fatalf("ack type=%q bytes=%d err=%v", typ, ack, err)
	}
	_ = clientConn.Close()
	select {
	case result := <-readDone:
		if !errors.Is(result.err, errDetached) || string(result.data) != "accepted" {
			t.Fatalf("stdin after client crash=(%q, %v)", result.data, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued stdin did not close after client crash")
	}
	select {
	case <-receiverDone:
	case <-time.After(5 * time.Second):
		t.Fatal("daemon input receiver survived client crash")
	}
	_ = serverConn.Close()
	_ = stdinReader.Close()
}

func TestExchangeDaemonReturnsOnControlSocketFailure(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	serverDone := make(chan struct{})
	go func() {
		reader := bufio.NewReader(serverConn)
		_, _ = reader.ReadBytes('\n')
		_, _, _ = readDaemonFrame(reader)
		_ = serverConn.Close()
		close(serverDone)
	}()
	result := make(chan error, 1)
	go func() {
		_, err := exchangeDaemon(clientConn, daemonRequest{Version: daemonAPIVersion, Action: "command", Stdin: true}, strings.NewReader("payload"), io.Discard, io.Discard)
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("exchange succeeded after control socket failure")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("exchange did not return after control socket failure")
	}
	<-serverDone
	_ = clientConn.Close()
}

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
