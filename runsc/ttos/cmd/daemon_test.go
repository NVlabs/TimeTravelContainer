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
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDaemonOwnsAndReapsConsoleSidecars(t *testing.T) {
	command := exec.Command("sleep", "30")
	if err := command.Start(); err != nil {
		t.Fatalf("starting sidecar fixture: %v", err)
	}
	server := &daemonServer{}
	if err := server.OwnConsoleSidecar("console-lock", command); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("OwnConsoleSidecar: %v", err)
	}
	if got := server.ownedConsoleSidecars(); got != 1 {
		t.Fatalf("owned sidecars=%d, want 1", got)
	}
	if err := server.OwnConsoleSidecar("console-lock", exec.Command("true")); err == nil {
		t.Fatal("duplicate console sidecar ownership succeeded")
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatalf("stopping sidecar fixture: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for server.ownedConsoleSidecars() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := server.ownedConsoleSidecars(); got != 0 {
		t.Fatalf("owned sidecars after exit=%d, want 0", got)
	}
	if command.ProcessState == nil {
		t.Fatalf("sidecar was not reaped: state=%v", command.ProcessState)
	}
}

func TestDaemonCommandUsesStdin(t *testing.T) {
	for _, test := range []struct {
		args []string
		want bool
	}{
		{[]string{"attach", "-i", "demo"}, true},
		{[]string{"container", "attach", "-i", "demo"}, true},
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
		{[]string{"image", "pull", "alpine"}, true},
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

func TestDaemonStdinLeaseRejectsAndTakesOver(t *testing.T) {
	server := &daemonServer{}
	cancelOld := make(chan struct{})
	releaseOld, err := server.acquireStdinLease("root\x00demo", false, func() { close(cancelOld) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.acquireStdinLease("root\x00demo", false, func() {}); err == nil || !strings.Contains(err.Error(), "--takeover") {
		t.Fatalf("second lease error=%v", err)
	}

	released := make(chan struct{})
	go func() {
		<-cancelOld
		releaseOld()
		close(released)
	}()
	releaseSecond, err := server.acquireStdinLease("root\x00demo", true, func() {})
	if err != nil {
		t.Fatalf("takeover: %v", err)
	}
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("old stdin owner was not detached")
	}
	releaseSecond()
}

func TestDaemonAttachTakeoverParsing(t *testing.T) {
	clean, key, takeover, err := daemonAttachStdinLease(options{}, []string{"attach", "-i", "--takeover", "--no-stdout"}, true)
	if err != nil || key != "" || !takeover || strings.Contains(strings.Join(clean, " "), "takeover") {
		t.Fatalf("takeover parse=(%q, %q, %t, %v)", clean, key, takeover, err)
	}
	if _, _, _, err := daemonAttachStdinLease(options{}, []string{"attach", "--takeover", "demo"}, false); err == nil {
		t.Fatal("takeover without interactive stdin was accepted")
	}
	clean, key, takeover, err = daemonAttachStdinLease(options{}, []string{"run", "image", "--takeover"}, false)
	if err != nil || key != "" || takeover || !strings.Contains(strings.Join(clean, " "), "--takeover") {
		t.Fatalf("non-attach args were modified: (%q, %q, %t, %v)", clean, key, takeover, err)
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
