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

package unet

import (
	"io"
	"runtime/pprof"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestBlockedReadersHoldNoThreads checks that readers waiting on silent
// sockets park in Go's network poller, rather than hold a host thread each,
// and that both data and Close wake them.
func TestBlockedReadersHoldNoThreads(t *testing.T) {
	const readers = 100
	threads := pprof.Lookup("threadcreate")
	before := threads.Count()
	servers := make([]*Socket, readers)
	clients := make([]*Socket, readers)
	results := make(chan error, readers)
	for i := range servers {
		server, client, err := SocketPair(false)
		if err != nil {
			t.Fatal(err)
		}
		servers[i], clients[i] = server, client
		go func() {
			reader := server.Reader(true)
			_, err := reader.ReadVec([][]byte{make([]byte, 1)})
			results <- err
		}()
	}
	// Let every reader find its socket empty and wait.
	time.Sleep(100 * time.Millisecond)
	if created := threads.Count() - before; created >= readers/2 {
		t.Errorf("%d readers waiting on silent sockets created %d threads", readers, created)
	}
	for i := range servers {
		if i%2 == 0 {
			if _, err := clients[i].Write([]byte{1}); err != nil {
				t.Fatal(err)
			}
		} else if err := servers[i].Close(); err != nil {
			t.Fatal(err)
		}
	}
	woken, closed := 0, 0
	for range servers {
		switch err := <-results; err {
		case nil:
			woken++
		case unix.EBADF, io.EOF:
			// Close shuts the socket down first, which a waiting reader may
			// see as the end of the stream.
			closed++
		default:
			t.Errorf("a waiting reader returned %v", err)
		}
	}
	if woken != readers/2 || closed != readers/2 {
		t.Errorf("data woke %d readers and Close %d, want %d each", woken, closed, readers/2)
	}
	for i := range servers {
		if i%2 == 0 {
			servers[i].Close()
		}
		clients[i].Close()
	}
}
