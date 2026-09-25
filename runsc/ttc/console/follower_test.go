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
	listener, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: address, Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: address, Net: "unixpacket"})
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
