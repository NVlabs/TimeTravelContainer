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
	"os"
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

func TestDaemonCommandNeedsMutationLock(t *testing.T) {
	for _, test := range []struct {
		args []string
		want bool
	}{
		{[]string{"fork", "--check", "demo"}, true},
		{[]string{"pause", "demo"}, true},
		{[]string{"container", "restart", "demo"}, true},
		{[]string{"image", "rm", "alpine"}, true},
		{[]string{"volume", "create", "cache"}, true},
		{[]string{"branch", "remove", "demo"}, true},
		{[]string{"run", "-d", "alpine"}, true},
		{[]string{"run", "--detach=false", "alpine"}, false},
		{[]string{"run", "alpine"}, false},
		{[]string{"start", "demo"}, true},
		{[]string{"start", "-ai", "demo"}, false},
		{[]string{"start", "--attach=false", "demo"}, true},
		{[]string{"exec", "-d", "demo", "true"}, false},
		{[]string{"logs", "-f", "demo"}, false},
		{[]string{"wait", "demo"}, false},
	} {
		if got := daemonCommandNeedsMutationLock(test.args); got != test.want {
			t.Errorf("daemonCommandNeedsMutationLock(%q)=%t, want %t", test.args, got, test.want)
		}
	}
}

func TestDaemonMutationLocksAreScopedByDataRoot(t *testing.T) {
	server := &daemonServer{}
	root := filepath.Join(t.TempDir(), "state")
	unlockFirst := server.lockMutation(root, []string{"pause", "demo"})

	secondAcquired := make(chan func(), 1)
	go func() {
		secondAcquired <- server.lockMutation(root, []string{"stop", "demo"})
	}()
	select {
	case unlock := <-secondAcquired:
		unlock()
		t.Fatal("conflicting mutation acquired the same data-root lock")
	case <-time.After(50 * time.Millisecond):
	}

	unlockRead := server.lockMutation(root, []string{"inspect", "demo"})
	unlockRead()
	unlockOther := server.lockMutation(root+"-other", []string{"stop", "demo"})
	unlockOther()

	unlockFirst()
	select {
	case unlock := <-secondAcquired:
		unlock()
	case <-time.After(5 * time.Second):
		t.Fatal("conflicting mutation remained blocked after unlock")
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

	dataRoot := t.TempDir()
	requestID, err := newDaemonRequestID()
	if err != nil {
		t.Fatal(err)
	}
	mutation := daemonRequest{
		Version: daemonAPIVersion, Action: "command", RequestID: requestID,
		Options: daemonWireOptions{DataRoot: dataRoot}, Args: []string{"volume", "create"},
	}
	status, firstStdout, stderr := exchange(mutation)
	if status != 0 || firstStdout == "" || stderr != "" {
		t.Fatalf("first mutation: status=%d stdout=%q stderr=%q", status, firstStdout, stderr)
	}
	status, replayStdout, stderr := exchange(mutation)
	if status != 0 || replayStdout != firstStdout || stderr != "" {
		t.Fatalf("replayed mutation: status=%d stdout=%q want=%q stderr=%q", status, replayStdout, firstStdout, stderr)
	}
	volumes, err := os.ReadDir(filepath.Join(dataRoot, "volumes"))
	if err != nil {
		t.Fatal(err)
	}
	if len(volumes) != 1 {
		t.Fatalf("idempotent mutation created %d volumes, want 1", len(volumes))
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
