// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	ttcconsole "gvisor.dev/gvisor/runsc/ttc/console"
)

// A family's follower asked to retire, as ttcd asks once the family's runtime
// is gone, replies, removes its socket and exits at once, rather than once it
// has idled for familyFollowerIdle.
func TestFamilyFollowerRetiresAtOnce(t *testing.T) {
	socket := filepath.Join(t.TempDir(), ttcconsole.FollowerSocketName)
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyRead.Close()
	exited := make(chan error, 1)
	go func() { exited <- runFamilyConsoleFollower(nil, "family", socket, "", readyWrite) }()
	if _, err := readyRead.Read(make([]byte, 1)); err != nil {
		t.Fatalf("waiting for the follower to listen: %v", err)
	}
	address, closeDir, err := ttcconsole.SocketAddress(socket)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialUnix(ttcconsole.FollowerNetwork, nil, &net.UnixAddr{Name: address, Net: ttcconsole.FollowerNetwork})
	closeDir()
	if err != nil {
		t.Fatalf("connecting to the follower: %v", err)
	}
	defer conn.Close()
	started := time.Now()
	if err := ttcconsole.WriteFollowerMessage(conn, ttcconsole.FollowerMessage{ID: 1, Op: ttcconsole.FollowerRetire}); err != nil {
		t.Fatal(err)
	}
	reply, _, err := ttcconsole.ReadFollowerMessage(conn)
	if err != nil || reply.ID != 1 || reply.Error != "" {
		t.Fatalf("reply to retire: %+v, %v", reply, err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("the retired follower: %v", err)
		}
	case <-time.After(familyFollowerIdle / 2):
		t.Fatalf("the follower had not exited %v after it was asked to retire", time.Since(started))
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Errorf("the retired follower's socket: %v, want it gone", err)
	}
}
