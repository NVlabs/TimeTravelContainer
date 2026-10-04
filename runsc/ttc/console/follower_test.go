// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package console

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A follower message crosses a SOCK_SEQPACKET socket whole, with its file, and
// the peer's close reads as io.EOF.
func TestFollowerMessageCarriesItsFile(t *testing.T) {
	// A directory whose path is too long for a socket address.
	dir := filepath.Join(t.TempDir(), strings.Repeat("d", 100))
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	address, done, err := SocketAddress(filepath.Join(dir, FollowerSocketName))
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	listener, err := net.ListenUnix(FollowerNetwork, &net.UnixAddr{Name: address, Net: FollowerNetwork})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.DialUnix(FollowerNetwork, nil, &net.UnixAddr{Name: address, Net: FollowerNetwork})
	if err != nil {
		t.Fatal(err)
	}
	server, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	lock, err := os.Create(filepath.Join(t.TempDir(), "init.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	sent := FollowerMessage{ID: 3, Op: FollowerFollow, Follow: &Follow{Branch: "main", ConsoleID: "init", Lock: lock.Name(), Log: "init.log", LogMaxFiles: 2}}
	if err := WriteFollowerMessage(client, sent, lock); err != nil {
		t.Fatal(err)
	}
	if err := WriteFollowerMessage(client, FollowerMessage{ID: 4, Op: FollowerStop, Lock: lock.Name()}); err != nil {
		t.Fatal(err)
	}
	received, files, err := ReadFollowerMessage(server)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(received, sent) || len(files) != 1 {
		t.Fatalf("received %+v with %d files, want %+v with one", received, len(files), sent)
	}
	var mine, theirs os.FileInfo
	if mine, err = lock.Stat(); err == nil {
		theirs, err = files[0].Stat()
	}
	if err != nil || !os.SameFile(mine, theirs) {
		t.Fatalf("the file received is not the lock sent: %v", err)
	}
	files[0].Close()
	if received, files, err = ReadFollowerMessage(server); err != nil || received.Op != FollowerStop || len(files) != 0 {
		t.Fatalf("second message: %+v, %d files, %v", received, len(files), err)
	}
	client.Close()
	if _, _, err := ReadFollowerMessage(server); !errors.Is(err, io.EOF) {
		t.Fatalf("reading after the peer closed: %v, want EOF", err)
	}
}

// A reader waits for a message that has yet to come, and a packet larger than
// any message reads as truncated.
func TestFollowerMessageWaitedForAndOversized(t *testing.T) {
	address, done, err := SocketAddress(filepath.Join(t.TempDir(), FollowerSocketName))
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	listener, err := net.ListenUnix(FollowerNetwork, &net.UnixAddr{Name: address, Net: FollowerNetwork})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.DialUnix(FollowerNetwork, nil, &net.UnixAddr{Name: address, Net: FollowerNetwork})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	sent := make(chan error, 1)
	go func() {
		time.Sleep(50 * time.Millisecond)
		sent <- WriteFollowerMessage(client, FollowerMessage{ID: 5, Op: FollowerStop, Lock: "init.lock"})
	}()
	if received, _, err := ReadFollowerMessage(server); err != nil || received.ID != 5 || received.Lock != "init.lock" {
		t.Fatalf("the message waited for: %+v, %v", received, err)
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	if err := sendFollowerPacket(client, make([]byte, maxFollowerMessage+1), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadFollowerMessage(server); err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("an oversized packet read: %v, want it truncated", err)
	}
}
