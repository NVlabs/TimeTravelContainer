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

package cmd

import (
	"net"
	"os"
	"testing"

	ttcconsole "gvisor.dev/gvisor/runsc/ttc/console"
)

// The test binary is also a fake family console follower, which fake
// runtimes start.
const (
	fakeFollowerSocketEnv = "TTC_TEST_FAKE_FOLLOWER_SOCKET"
	fakeFollowerMarkerEnv = "TTC_TEST_FAKE_FOLLOWER_MARKER"
)

func TestMain(m *testing.M) {
	if socket := os.Getenv(fakeFollowerSocketEnv); socket != "" {
		os.Exit(runFakeFamilyFollower(socket, os.Getenv(fakeFollowerMarkerEnv)))
	}
	os.Exit(m.Run())
}

// testBinary returns the path of the running test binary.
func testBinary(t *testing.T) string {
	path, err := os.Executable()
	if err != nil {
		t.Fatalf("finding the test binary: %v", err)
	}
	return path
}

// runFakeFamilyFollower accepts every console asked of it on one connection
// to socket, until the connection closes, and creates marker for one whose
// log rotates after 4096 bytes over 3 files.
func runFakeFamilyFollower(socket, marker string) int {
	address, closeDir, err := ttcconsole.SocketAddress(socket)
	if err != nil {
		return 1
	}
	listener, err := net.ListenUnix(ttcconsole.FollowerNetwork, &net.UnixAddr{Name: address, Net: ttcconsole.FollowerNetwork})
	closeDir()
	if err != nil {
		return 1
	}
	ready := os.NewFile(3, "console-ready")
	_, _ = ready.Write([]byte{1})
	_ = ready.Close()
	conn, err := listener.AcceptUnix()
	if err != nil {
		return 1
	}
	for {
		message, files, err := ttcconsole.ReadFollowerMessage(conn)
		if err != nil {
			return 0
		}
		for _, file := range files {
			_ = file.Close()
		}
		if follow := message.Follow; follow != nil && follow.LogMaxBytes == 4096 && follow.LogMaxFiles == 3 && marker != "" {
			_ = os.WriteFile(marker, nil, 0600)
		}
		if err := ttcconsole.WriteFollowerMessage(conn, ttcconsole.FollowerMessage{ID: message.ID}); err != nil {
			return 1
		}
	}
}
