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

package cluster

import (
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func TestProtocolMuxRoutesAndReplays(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux, err := NewProtocolMux(listener)
	if err != nil {
		t.Fatal(err)
	}
	defer mux.Close()

	for _, test := range []struct {
		name    string
		payload string
		accept  net.Listener
	}{
		{name: "http", payload: "GET /v1/test HTTP/1.1\r\n", accept: mux.HTTPListener()},
		{name: "raft", payload: raftWireMagic + "payload", accept: mux.RaftListener()},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, err := net.Dial("tcp", mux.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			if _, err := io.WriteString(client, test.payload); err != nil {
				t.Fatal(err)
			}
			accepted := make(chan net.Conn, 1)
			go func() {
				conn, _ := test.accept.Accept()
				accepted <- conn
			}()
			select {
			case conn := <-accepted:
				if conn == nil {
					t.Fatal("accept returned nil")
				}
				defer conn.Close()
				got := make([]byte, len(test.payload))
				if _, err := io.ReadFull(conn, got); err != nil {
					t.Fatal(err)
				}
				if string(got) != test.payload {
					t.Fatalf("payload=%q, want %q", got, test.payload)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for routed connection")
			}
		})
	}
}

func TestProtocolMuxServesHTTPBesideRaft(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux, err := NewProtocolMux(listener)
	if err != nil {
		t.Fatal(err)
	}
	defer mux.Close()

	httpServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "control-ok")
	})}
	defer func() { _ = httpServer.Close() }()
	go func() { _ = httpServer.Serve(mux.HTTPListener()) }()

	raftNode, err := OpenAuthorityRaftNode(AuthorityRaftOptions{
		NodeID: "node-a", LocalDir: filepath.Join(t.TempDir(), "raft"),
		Listener: mux.RaftListener(), AdvertiseHost: mux.Addr().String(),
		AuthToken: "secret", Bootstrap: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer raftNode.Close()
	waitAuthorityLeader(t, raftNode)

	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get("http://" + mux.Addr().String() + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "control-ok" {
		t.Fatalf("HTTP response=%q, want control-ok", body)
	}
}
