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
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/ttcvolume"
)

func TestDaemonWireOptionsPreserveClientExecutable(t *testing.T) {
	wire := wireOptions(options{clientExecutable: "/opt/ttc/bin/ttc"})
	if got := wire.ClientExecutable; got != "/opt/ttc/bin/ttc" {
		t.Fatalf("wire client executable=%q, want /opt/ttc/bin/ttc", got)
	}
	opts := optionsFromWire(wire)
	backend := directBackend(opts, strings.NewReader(""), io.Discard, io.Discard)
	if got := backend.clientExecutable; got != "/opt/ttc/bin/ttc" {
		t.Fatalf("backend client executable=%q, want /opt/ttc/bin/ttc", got)
	}
}

func TestDaemonRequestOptionsUseServerDefaults(t *testing.T) {
	server := &daemonServer{
		root:                  "/run/ttc/runsc",
		runsc:                 "/opt/ttc/bin/ttcd",
		dataRoot:              "/var/lib/ttc",
		clientExecutable:      "/opt/ttc/bin/ttc",
		unsafeNoHostIsolation: true,
	}
	opts := server.optionsFromRequest(daemonWireOptions{})
	if got, want := opts.root, server.root; got != want {
		t.Errorf("root=%q, want %q", got, want)
	}
	if got, want := opts.runsc, server.runsc; got != want {
		t.Errorf("runsc=%q, want %q", got, want)
	}
	if got, want := opts.dataRoot, server.dataRoot; got != want {
		t.Errorf("data root=%q, want %q", got, want)
	}
	if got, want := opts.clientExecutable, server.clientExecutable; got != want {
		t.Errorf("client executable=%q, want %q", got, want)
	}
	if !opts.unsafeNoHostIsolation {
		t.Error("unsafe-no-host-isolation daemon mode was not applied")
	}
}

func TestDaemonRequestOptionsPreserveClientRuntimeOverrides(t *testing.T) {
	server := &daemonServer{
		root:             "/default/root",
		runsc:            "/default/ttcd",
		dataRoot:         "/daemon/data",
		clientExecutable: "/default/ttc",
	}
	opts := server.optionsFromRequest(daemonWireOptions{
		Root:             "/custom/root",
		Runsc:            "/custom/ttcd",
		DataRoot:         "/ignored/client/data",
		ClientExecutable: "/custom/ttc",
	})
	if got, want := opts.root, "/custom/root"; got != want {
		t.Errorf("root=%q, want %q", got, want)
	}
	if got, want := opts.runsc, "/custom/ttcd"; got != want {
		t.Errorf("runsc=%q, want %q", got, want)
	}
	if got, want := opts.dataRoot, server.dataRoot; got != want {
		t.Errorf("data root=%q, want daemon-owned %q", got, want)
	}
	if got, want := opts.clientExecutable, "/custom/ttc"; got != want {
		t.Errorf("client executable=%q, want %q", got, want)
	}
}

func TestClientRequiresExplicitDaemonStart(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "runtime", "ttcd.sock")
	var stdout, stderr bytes.Buffer
	status := Run([]string{"--daemon-socket", socket, "completion", "bash"}, strings.NewReader(""), &stdout, &stderr)
	if status != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "start it explicitly with 'ttc daemon start'") {
		t.Fatalf("missing daemon: status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
}

func TestReportDaemonStartupFailureIncludesDaemonDiagnostic(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "ttcd.log")
	if err := os.WriteFile(logPath, []byte("ttcd: volume backing filesystem does not support FICLONE\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	reportDaemonStartupFailure(&stderr, logPath, errors.New("exit status 1"))
	got := stderr.String()
	if !strings.Contains(got, logPath) || !strings.Contains(got, "exit status 1") || !strings.Contains(got, "does not support FICLONE") {
		t.Fatalf("startup diagnostic=%q, want log path, process status, and daemon error", got)
	}
}

func TestStopDaemonContainersStopsDescendantsBeforeMain(t *testing.T) {
	b := &fakeBackend{
		families: []Family{{
			ID: "web",
			Branches: []Branch{
				{ID: defaultBranchID, State: "running"},
				{ID: "child", ParentID: defaultBranchID, State: "paused"},
				{ID: "grandchild", ParentID: "child", State: "running"},
				{ID: "old", ParentID: defaultBranchID, State: "exited"},
			},
		}},
		inspectInfo: InspectInfo{},
	}
	if err := stopDaemonContainers(b); err != nil {
		t.Fatalf("stopDaemonContainers: %v", err)
	}
	if got, want := b.resumed, (selector{container: "web", branch: "child"}); got != want {
		t.Errorf("resumed=%+v, want %+v", got, want)
	}
	wantOrder := []selector{
		{container: "web", branch: "grandchild"},
		{container: "web", branch: "child"},
		{container: "web", branch: defaultBranchID},
	}
	if len(b.signals) != len(wantOrder) {
		t.Fatalf("signals=%+v, want targets %+v", b.signals, wantOrder)
	}
	for i, want := range wantOrder {
		if b.signals[i].target != want {
			t.Errorf("signal[%d] target=%+v, want %+v", i, b.signals[i].target, want)
		}
	}
}

func TestClientCannotSelectDaemonIsolationMode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	status := Run([]string{"--unsafe-no-host-isolation", "completion", "bash"}, strings.NewReader(""), &stdout, &stderr)
	if status != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "ordinary commands inherit the running daemon's mode") {
		t.Fatalf("per-command mode: status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	status = Run([]string{"--unsafe-no-host-isolation", "daemon", "status"}, strings.NewReader(""), &stdout, &stderr)
	if status != 2 || !strings.Contains(stderr.String(), "valid with 'daemon start' only") {
		t.Fatalf("daemon status mode: status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
}

func TestRootlessDaemonStartupPreflight(t *testing.T) {
	original := createDaemonRootlessProbe
	originalEUID := daemonEffectiveUID
	defer func() {
		createDaemonRootlessProbe = original
		daemonEffectiveUID = originalEUID
	}()
	daemonEffectiveUID = func() int { return 1000 }
	calls := 0
	createDaemonRootlessProbe = func() error {
		calls++
		return errors.New("permission denied")
	}

	if err := preflightDaemonIsolation(true); err != nil {
		t.Fatalf("unsafe preflight: %v", err)
	}
	if calls != 0 {
		t.Fatalf("unsafe mode ran %d rootless probes, want zero", calls)
	}

	socket := filepath.Join(t.TempDir(), "runtime", "ttcd.sock")
	var stdout, stderr bytes.Buffer
	status := Run([]string{"--daemon-socket", socket, "daemon", "start"}, strings.NewReader(""), &stdout, &stderr)
	if status != 1 || stdout.Len() != 0 {
		t.Fatalf("rootless start: status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "rootless mode is unavailable") || !strings.Contains(stderr.String(), "--unsafe-no-host-isolation") {
		t.Fatalf("rootless start diagnostic=%q", stderr.String())
	}
	if calls != 1 {
		t.Fatalf("rootless start ran %d probes, want one", calls)
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatalf("failed preflight created daemon socket: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	status = RunDaemon([]string{"--socket", socket}, &stdout, &stderr)
	if status != 1 || !strings.Contains(stderr.String(), "rootless mode is unavailable") {
		t.Fatalf("direct ttcd preflight: status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
}

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

func TestDaemonStopsOwnedPortSidecar(t *testing.T) {
	command := exec.Command("sleep", "30")
	if err := command.Start(); err != nil {
		t.Fatalf("starting port sidecar fixture: %v", err)
	}
	server := &daemonServer{}
	if err := server.OwnPortSidecar("port-lock", command); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("OwnPortSidecar: %v", err)
	}
	stopped, err := server.StopPortSidecar("port-lock")
	if err != nil || !stopped {
		t.Fatalf("StopPortSidecar=(%t, %v), want true, nil", stopped, err)
	}
	if command.ProcessState == nil {
		t.Fatal("port sidecar was not reaped")
	}
	if stopped, err := server.StopPortSidecar("missing"); err != nil || stopped {
		t.Fatalf("StopPortSidecar missing=(%t, %v), want false, nil", stopped, err)
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
		{[]string{"cp", "-", "demo:/tmp"}, true},
		{[]string{"cp", "local", "demo:/tmp"}, false},
		{[]string{"cp", "demo:/tmp", "-"}, false},
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
		{[]string{"branch", "remove", "demo"}, true},
		{[]string{"port", "publish", "demo", "8080:80"}, true},
		{[]string{"port", "unpublish", "demo", "8080:80"}, true},
		{[]string{"port", "ls", "demo"}, false},
		{[]string{"run", "-d", "alpine"}, true},
		{[]string{"run", "--detach=false", "alpine"}, false},
		{[]string{"run", "alpine"}, false},
		{[]string{"start", "demo"}, true},
		{[]string{"start", "-ai", "demo"}, false},
		{[]string{"start", "--attach=false", "demo"}, true},
		{[]string{"cp", "local", "demo:/tmp"}, true},
		{[]string{"cp", "demo:/tmp", "local"}, true},
		{[]string{"checkpoint", "demo", "/tmp/demo.ttc"}, true},
		{[]string{"restore", "--name", "recovered", "/tmp/demo.ttc"}, true},
		{[]string{"exec", "-d", "demo", "true"}, false},
		{[]string{"logs", "-f", "demo"}, false},
		{[]string{"wait", "demo"}, false},
	} {
		if got := daemonCommandNeedsMutationLock(test.args); got != test.want {
			t.Errorf("daemonCommandNeedsMutationLock(%q)=%t, want %t", test.args, got, test.want)
		}
	}
}

func TestDaemonCommandNeedsMutationJournal(t *testing.T) {
	for _, test := range []struct {
		args []string
		want bool
	}{
		{[]string{"cp", "local", "demo:/tmp"}, true},
		{[]string{"cp", "-", "demo:/tmp"}, true},
		{[]string{"cp", "demo:/tmp", "local"}, false},
		{[]string{"cp", "demo:/tmp", "-"}, false},
		{[]string{"fork", "demo", "child"}, true},
		{[]string{"checkpoint", "demo", "/tmp/demo.ttc"}, false},
		{[]string{"restore", "--name", "recovered", "/tmp/demo.ttc"}, true},
		{[]string{"pull", "alpine"}, false},
		{[]string{"image", "pull", "alpine"}, false},
		{[]string{"logs", "demo"}, false},
	} {
		if got := daemonCommandNeedsMutationJournal(test.args); got != test.want {
			t.Errorf("daemonCommandNeedsMutationJournal(%q)=%t, want %t", test.args, got, test.want)
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
	socket := filepath.Join(t.TempDir(), "runtime", "ttcd.sock")
	dataRoot := t.TempDir()
	done := make(chan error, 1)
	go func() {
		done <- serveDaemon(socket, true, dataRoot, ttcvolume.Capability{Mode: ttcvolume.CopyModeFull}, io.Discard, io.Discard)
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
	if status != 0 || !strings.Contains(stdout, "_ttc_completion()") || stderr != "" {
		t.Fatalf("completion response: status=%d stdout=%q stderr=%q", status, stdout, stderr)
	}

	status, stdout, stderr = exchange(daemonRequest{Version: daemonAPIVersion, Action: "ping"})
	if status != 0 ||
		!strings.Contains(stdout, `"isolation_mode":"unsafe-no-host-isolation"`) ||
		!strings.Contains(stdout, `"unsafe_no_host_isolation":true`) ||
		!strings.Contains(stdout, `"data_root":"`+dataRoot+`"`) ||
		!strings.Contains(stdout, `"volume_copy_mode":"full-copy"`) ||
		stderr != "" {
		t.Fatalf("daemon mode status: status=%d stdout=%q stderr=%q", status, stdout, stderr)
	}
	requestID, err := newDaemonRequestID()
	if err != nil {
		t.Fatal(err)
	}
	mutation := daemonRequest{
		Version: daemonAPIVersion, Action: "command", RequestID: requestID,
		Options: daemonWireOptions{DataRoot: dataRoot}, Args: []string{"image", "rm", "missing"},
	}
	status, firstStdout, stderr := exchange(mutation)
	if status == 0 || firstStdout != "" || stderr == "" {
		t.Fatalf("first mutation: status=%d stdout=%q stderr=%q", status, firstStdout, stderr)
	}
	firstStatus, firstStderr := status, stderr
	status, replayStdout, stderr := exchange(mutation)
	if status != firstStatus || replayStdout != firstStdout || stderr != firstStderr {
		t.Fatalf("replayed mutation: status=%d stdout=%q stderr=%q, want status=%d stdout=%q stderr=%q", status, replayStdout, stderr, firstStatus, firstStdout, firstStderr)
	}
	records, err := listDaemonMutations(dataRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("idempotent replay recorded %d mutations, want 1", len(records))
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

func TestDefaultDaemonSocketHonorsTTCRuntimeDir(t *testing.T) {
	t.Setenv("TTC_RUNTIME_DIR", "/var/run/ttc-test")
	if got, want := defaultDaemonSocket(), "/var/run/ttc-test/ttcd.sock"; got != want {
		t.Fatalf("defaultDaemonSocket() = %q, want %q", got, want)
	}
}
