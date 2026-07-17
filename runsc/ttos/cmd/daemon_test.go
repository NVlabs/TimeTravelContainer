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
	"bytes"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDaemonCommandUsesStdin(t *testing.T) {
	for _, test := range []struct {
		args []string
		want bool
	}{
		{[]string{"attach", "-i", "demo"}, true},
		{[]string{"exec", "-dit", "demo", "sh"}, true},
		{[]string{"run", "--interactive", "alpine"}, true},
		{[]string{"start", "-ai", "demo"}, true},
		{[]string{"exec", "ls", "demo"}, false},
		{[]string{"exec", "demo", "true"}, false},
		{[]string{"logs", "-f", "demo"}, false},
	} {
		if got := daemonCommandUsesStdin(test.args); got != test.want {
			t.Errorf("daemonCommandUsesStdin(%q)=%t, want %t", test.args, got, test.want)
		}
	}
}

func TestDaemonVersionedCommandTransport(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "runtime", "ttosd.sock")
	done := make(chan error, 1)
	go func() {
		done <- serveDaemon(socket, io.Discard, io.Discard)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := connectDaemon(socket)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon did not become ready: %v", err)
		}
		time.Sleep(time.Millisecond)
	}

	exchange := func(request daemonRequest) (int, string, string) {
		t.Helper()
		conn, err := connectDaemon(socket)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		var stdout, stderr bytes.Buffer
		status, err := exchangeDaemon(conn, request, strings.NewReader(""), &stdout, &stderr)
		if err != nil {
			t.Fatalf("exchangeDaemon: %v", err)
		}
		return status, stdout.String(), stderr.String()
	}

	status, stdout, stderr := exchange(daemonRequest{
		Version: daemonAPIVersion,
		Action:  "command",
		Options: daemonWireOptions{},
		Args:    []string{"completion", "bash"},
	})
	if status != 0 || !strings.Contains(stdout, "_ttos_completion()") || stderr != "" {
		t.Fatalf("completion response: status=%d stdout=%q stderr=%q", status, stdout, stderr)
	}

	status, _, stderr = exchange(daemonRequest{Version: daemonAPIVersion + 1, Action: "ping"})
	if status != 1 || !strings.Contains(stderr, "unsupported API version") {
		t.Fatalf("version rejection: status=%d stderr=%q", status, stderr)
	}

	status, _, stderr = exchange(daemonRequest{Version: daemonAPIVersion, Action: "stop"})
	if status != 0 || stderr != "" {
		t.Fatalf("stop response: status=%d stderr=%q", status, stderr)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveDaemon: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop")
	}
}
