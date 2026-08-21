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
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	ttcpolicy "gvisor.dev/gvisor/pkg/ttcremote"
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
		runtimePlatform:       "kvm",
		clientExecutable:      "/opt/ttc/bin/ttc",
		unsafeNoHostIsolation: true,
		disableHostGSO:        true,
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
	if got, want := opts.runtimePlatform, "kvm"; got != want {
		t.Errorf("runtime platform=%q, want %q", got, want)
	}
	if !opts.unsafeNoHostIsolation {
		t.Error("unsafe-no-host-isolation daemon mode was not applied")
	}
	if !opts.disableHostGSO {
		t.Error("daemon host-GSO compatibility mode was not applied")
	}
}

func TestDaemonRequestOptionsIgnoreClientRuntimeOverrides(t *testing.T) {
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
	if got, want := opts.root, server.root; got != want {
		t.Errorf("root=%q, want %q", got, want)
	}
	if got, want := opts.runsc, server.runsc; got != want {
		t.Errorf("runsc=%q, want %q", got, want)
	}
	if got, want := opts.dataRoot, server.dataRoot; got != want {
		t.Errorf("data root=%q, want daemon-owned %q", got, want)
	}
	if got, want := opts.clientExecutable, "/custom/ttc"; got != want {
		t.Errorf("client executable=%q, want %q", got, want)
	}
}

func TestHostKernelCompatibility(t *testing.T) {
	for _, test := range []struct {
		release        string
		version        hostKernelVersion
		disableHostGSO bool
		wantError      bool
	}{
		{release: "3.16.99", wantError: true},
		{release: "3.17", version: hostKernelVersion{major: 3, minor: 17}, disableHostGSO: true},
		{release: "3.17.0-custom", version: hostKernelVersion{major: 3, minor: 17}, disableHostGSO: true},
		{release: "4.14-legacy", version: hostKernelVersion{major: 4, minor: 14}, disableHostGSO: true},
		{release: "4.14.76-generic", version: hostKernelVersion{major: 4, minor: 14, patch: 76}, disableHostGSO: true},
		{release: "4.14.77-generic", version: hostKernelVersion{major: 4, minor: 14, patch: 77}},
		{release: "4.15.0", version: hostKernelVersion{major: 4, minor: 15}},
		{release: "6.8.0-60-generic", version: hostKernelVersion{major: 6, minor: 8}},
		{release: "", wantError: true},
		{release: "not-a-kernel", wantError: true},
		{release: "4", wantError: true},
		{release: "4.x", wantError: true},
	} {
		t.Run(test.release, func(t *testing.T) {
			compatibility, err := evaluateHostKernelCompatibility(test.release)
			if test.wantError {
				if err == nil {
					t.Fatalf("evaluateHostKernelCompatibility(%q) succeeded: %+v", test.release, compatibility)
				}
				return
			}
			if err != nil {
				t.Fatalf("evaluateHostKernelCompatibility(%q): %v", test.release, err)
			}
			if compatibility.version != test.version || compatibility.release != test.release || compatibility.disableHostGSO != test.disableHostGSO {
				t.Errorf("evaluateHostKernelCompatibility(%q)=%+v, want version=%+v disableHostGSO=%t", test.release, compatibility, test.version, test.disableHostGSO)
			}
		})
	}
}

func TestRuntimePlatformValidation(t *testing.T) {
	for _, test := range []struct {
		value string
		want  string
		ok    bool
	}{
		{value: "systrap", want: "systrap", ok: true},
		{value: "KVM", want: "kvm", ok: true},
		{value: "ptrace"},
		{value: ""},
	} {
		got, err := normalizeRuntimePlatform(test.value)
		if test.ok {
			if err != nil || got != test.want {
				t.Errorf("normalizeRuntimePlatform(%q)=(%q, %v), want %q", test.value, got, err, test.want)
			}
		} else if err == nil {
			t.Errorf("normalizeRuntimePlatform(%q)=%q, want error", test.value, got)
		}
	}
}

func TestDaemonKVMPreflightFailure(t *testing.T) {
	original := daemonPlatformPreflight
	defer func() { daemonPlatformPreflight = original }()
	var gotPlatform string
	daemonPlatformPreflight = func(platform string) error {
		gotPlatform = platform
		return errors.New("test KVM unavailable")
	}
	var stdout, stderr bytes.Buffer
	status := RunDaemon([]string{"--platform=kvm", "--socket", filepath.Join(t.TempDir(), "ttcd.sock")}, &stdout, &stderr)
	if status != 1 || gotPlatform != "kvm" || !strings.Contains(stderr.String(), "test KVM unavailable") {
		t.Fatalf("KVM preflight: status=%d platform=%q stdout=%q stderr=%q", status, gotPlatform, stdout.String(), stderr.String())
	}
}

func TestDaemonRejectsUnsupportedRuntimePlatform(t *testing.T) {
	var stdout, stderr bytes.Buffer
	status := RunDaemon([]string{"--platform=ptrace"}, &stdout, &stderr)
	if status != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "supported platforms are systrap and kvm") {
		t.Fatalf("unsupported platform: status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
}

func TestPrepareDaemonRuntimeStorageRejectsNFS(t *testing.T) {
	original := daemonStatfs
	t.Cleanup(func() { daemonStatfs = original })
	daemonStatfs = func(_ string, statfs *unix.Statfs_t) error {
		statfs.Type = unix.NFS_SUPER_MAGIC
		return nil
	}
	dir := t.TempDir()
	err := prepareDaemonRuntimeStorage(filepath.Join(dir, "runtime", "ttcd.sock"), filepath.Join(dir, "runsc"))
	if err == nil || !strings.Contains(err.Error(), "node-local storage") || !strings.Contains(err.Error(), "TTC_DATA_ROOT") {
		t.Fatalf("prepareDaemonRuntimeStorage on NFS: %v", err)
	}
}

func TestPrepareDaemonRuntimeStorageCreatesLocalPaths(t *testing.T) {
	original := daemonStatfs
	t.Cleanup(func() { daemonStatfs = original })
	daemonStatfs = func(_ string, statfs *unix.Statfs_t) error {
		statfs.Type = unix.TMPFS_MAGIC
		return nil
	}
	dir := t.TempDir()
	socketDir := filepath.Join(dir, "runtime")
	root := filepath.Join(dir, "runsc")
	if err := prepareDaemonRuntimeStorage(filepath.Join(socketDir, "ttcd.sock"), root); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{socketDir, root} {
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			t.Fatalf("runtime path %q: info=%v err=%v", path, info, err)
		}
	}
}

func TestDaemonRejectsUnsupportedHostKernel(t *testing.T) {
	original := daemonKernelRelease
	defer func() {
		daemonKernelRelease = original
	}()
	daemonKernelRelease = func() (string, error) {
		return "3.16.99-legacy", nil
	}

	socket := filepath.Join(t.TempDir(), "runtime", "ttcd.sock")
	var stdout, stderr bytes.Buffer
	status := RunDaemon([]string{
		"--socket", socket,
		"--data-root", t.TempDir(),
		"--unsafe-no-host-isolation",
		"--allow-slow-volume-copy",
	}, &stdout, &stderr)
	if status != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "requires Linux 3.17 or newer") {
		t.Fatalf("unsupported kernel: status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatalf("unsupported kernel created daemon socket: %v", err)
	}
}

func TestDaemonWarnsWhenHostGSOMustBeDisabled(t *testing.T) {
	original := daemonKernelRelease
	defer func() {
		daemonKernelRelease = original
	}()
	daemonKernelRelease = func() (string, error) {
		return "4.14.76-legacy", nil
	}

	dataRoot := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(dataRoot, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	status := RunDaemon([]string{
		"--socket", filepath.Join(t.TempDir(), "runtime", "ttcd.sock"),
		"--data-root", dataRoot,
		"--unsafe-no-host-isolation",
		"--allow-slow-volume-copy",
	}, &stdout, &stderr)
	if status != 1 || stdout.Len() != 0 {
		t.Fatalf("old kernel startup fixture: status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
	if diagnostic := stderr.String(); !strings.Contains(diagnostic, "disabling host GSO") || !strings.Contains(diagnostic, "large payloads") {
		t.Fatalf("old kernel startup diagnostic=%q", diagnostic)
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

func TestReportDaemonStartupWarningsSurfacesHostGSOFallback(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "ttcd.log")
	log := "ttcd: warning: unrelated warning\n" +
		"ttcd: warning: host Linux kernel 4.14.76 predates 4.14.77; disabling host GSO for compatibility; network performance will be reduced\n" +
		"ttcd listening\n"
	if err := os.WriteFile(logPath, []byte(log), 0600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	reportDaemonStartupWarnings(&stderr, logPath)
	if diagnostic := stderr.String(); !strings.Contains(diagnostic, "disabling host GSO") || strings.Contains(diagnostic, "unrelated warning") {
		t.Fatalf("surfaced startup warnings=%q", diagnostic)
	}
}

func TestStopDaemonContainersTreatsLineageAsDisplayOnly(t *testing.T) {
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
		inspectInfo: InspectInfo{TTC: TTCInspectInfo{Branches: []InspectBranch{
			{Metadata: Branch{ID: defaultBranchID, State: "running"}},
			{Metadata: Branch{ID: "child", ParentID: defaultBranchID, State: "paused"}},
			{Metadata: Branch{ID: "grandchild", ParentID: "child", State: "running"}},
			{Metadata: Branch{ID: "old", ParentID: defaultBranchID, State: "exited"}},
		}}},
	}
	if err := stopDaemonContainers(b); err != nil {
		t.Fatalf("stopDaemonContainers: %v", err)
	}
	if b.resumed.container != "" {
		t.Errorf("daemon stop resumed paused branch: %+v", b.resumed)
	}
	wantOrder := []selector{
		{container: "web", branch: "grandchild"},
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
		{[]string{"exec", "-dit", "demo@main", "sh"}, true},
		{[]string{"run", "--interactive", "alpine"}, true},
		{[]string{"start", "-ai", "demo"}, true},
		{[]string{"cp", "-", "demo@main:/tmp"}, true},
		{[]string{"cp", "local", "demo@main:/tmp"}, false},
		{[]string{"cp", "demo@main:/tmp", "-"}, false},
		{[]string{"load"}, true},
		{[]string{"load", "--platform", "linux/amd64"}, true},
		{[]string{"load", "-i", "/tmp/images.tar"}, false},
		{[]string{"image", "load"}, true},
		{[]string{"image", "load", "--input=/tmp/images.tar"}, false},
		{[]string{"exec", "ls", "demo"}, false},
		{[]string{"exec", "demo@main", "true"}, false},
		{[]string{"logs", "-f", "demo"}, false},
	} {
		if got := daemonCommandUsesStdin(test.args); got != test.want {
			t.Errorf("daemonCommandUsesStdin(%q)=%t, want %t", test.args, got, test.want)
		}
	}
}

func TestNormalizeDaemonImageArchivePaths(t *testing.T) {
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		args []string
		want []string
	}{
		{[]string{"load", "-i", "images.tar"}, []string{"load", "-i", filepath.Join(workingDirectory, "images.tar")}},
		{[]string{"image", "load", "--input=images.tar.gz"}, []string{"image", "load", "--input=" + filepath.Join(workingDirectory, "images.tar.gz")}},
		{[]string{"save", "-o=out.tar", "alpine"}, []string{"save", "-o=" + filepath.Join(workingDirectory, "out.tar"), "alpine"}},
		{[]string{"image", "save", "--output", "/tmp/out.tar", "alpine"}, []string{"image", "save", "--output", "/tmp/out.tar", "alpine"}},
		{[]string{"load"}, []string{"load"}},
		{[]string{"pull", "alpine"}, []string{"pull", "alpine"}},
	} {
		got, err := normalizeDaemonImageArchivePaths(test.args)
		if err != nil {
			t.Errorf("normalizeDaemonImageArchivePaths(%q): %v", test.args, err)
			continue
		}
		if !reflect.DeepEqual(got, test.want) {
			t.Errorf("normalizeDaemonImageArchivePaths(%q)=%q, want %q", test.args, got, test.want)
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
		{[]string{"load"}, true},
		{[]string{"image", "load"}, true},
		{[]string{"save", "alpine"}, true},
		{[]string{"image", "save", "alpine"}, true},
		{[]string{"commit", "demo@main", "local/demo:latest"}, true},
		{[]string{"rm", "demo@child"}, true},
		{[]string{"port", "publish", "demo@main", "8080:80"}, true},
		{[]string{"port", "unpublish", "demo@main", "8080:80"}, true},
		{[]string{"port", "ls", "demo"}, false},
		{[]string{"run", "-d", "alpine"}, true},
		{[]string{"run", "--detach=false", "alpine"}, false},
		{[]string{"run", "alpine"}, false},
		{[]string{"start", "demo"}, true},
		{[]string{"start", "-ai", "demo"}, false},
		{[]string{"start", "--attach=false", "demo"}, true},
		{[]string{"cp", "local", "demo@main:/tmp"}, true},
		{[]string{"cp", "demo@main:/tmp", "local"}, true},
		{[]string{"checkpoint", "demo@main", "/tmp/demo.ttc"}, true},
		{[]string{"restore", "--name", "recovered", "/tmp/demo.ttc"}, true},
		{[]string{"exec", "-d", "demo@main", "true"}, false},
		{[]string{"logs", "-f", "demo@main"}, false},
		{[]string{"wait", "demo@main"}, false},
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
		{[]string{"cp", "local", "demo@main:/tmp"}, true},
		{[]string{"cp", "-", "demo@main:/tmp"}, true},
		{[]string{"cp", "demo@main:/tmp", "local"}, false},
		{[]string{"cp", "demo@main:/tmp", "-"}, false},
		{[]string{"fork", "demo@main", "demo@child"}, true},
		{[]string{"checkpoint", "demo@main", "/tmp/demo.ttc"}, false},
		{[]string{"restore", "--name", "recovered", "/tmp/demo.ttc"}, true},
		{[]string{"pull", "alpine"}, false},
		{[]string{"image", "pull", "alpine"}, false},
		{[]string{"load"}, false},
		{[]string{"image", "load"}, false},
		{[]string{"save", "alpine"}, false},
		{[]string{"image", "save", "alpine"}, false},
		{[]string{"commit", "demo@main", "local/demo:latest"}, false},
		{[]string{"logs", "demo@main"}, false},
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
		done <- serveDaemon(socket, "kvm", true, hostKernelCompatibility{release: "6.8.0"}, dataRoot, "/runtime", "/runsc", ttcvolume.Capability{Mode: ttcvolume.CopyModeFull}, io.Discard, io.Discard)
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
		!strings.Contains(stdout, `"runtime_platform":"kvm"`) ||
		!strings.Contains(stdout, `"isolation_mode":"unsafe-no-host-isolation"`) ||
		!strings.Contains(stdout, `"unsafe_no_host_isolation":true`) ||
		!strings.Contains(stdout, `"host_kernel_release":"6.8.0"`) ||
		!strings.Contains(stdout, `"host_gso_enabled":true`) ||
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

func TestConsumeDaemonClusterOptions(t *testing.T) {
	args := []string{
		"--cluster-listen=127.0.0.1:7443",
		"--cluster-node-id", "node-a",
		"--cluster-repository-id", "repo",
		"--cluster-cert", "cert.pem",
		"--cluster-key", "key.pem",
		"--cluster-ca", "ca.pem",
		"--cluster-advertise", "https://node-a.example:0,node-a.example",
		"--cluster-memory-target", "8GiB",
		"--cluster-auto-rebalance",
		"--cluster-capacity-command", "/usr/bin/provision-ttc",
		"--cluster-lease", "30s",
		"--cluster-max-transactions", "17",
		"--cluster-page-listen", "0.0.0.0:0",
		"--cluster-page-advertise", "node-a.example:0",
		"--cluster-transfer-bytes", "131072",
		"--cluster-max-outstanding-bytes", "8388608",
		"--cluster-max-parallel", "8",
		"--cluster-ack-batch-pages", "128",
		"--cluster-ack-interval", "250us",
		"--cluster-prefetch", "adaptive",
		"--cluster-push", "eager-drain",
		"--cluster-peer", "node-b=https://10.0.0.2:7443,node-b.example",
	}
	opts := defaultDaemonClusterOptions()
	for len(args) != 0 {
		consumed, err := consumeDaemonClusterOption(args, &opts)
		if err != nil {
			t.Fatal(err)
		}
		if consumed == 0 {
			t.Fatalf("option was not consumed: %q", args[0])
		}
		args = args[consumed:]
	}
	if !opts.Configured || opts.Listen != "127.0.0.1:7443" || opts.NodeID != "node-a" ||
		opts.RepositoryID != "repo" || opts.CertFile != "cert.pem" || opts.KeyFile != "key.pem" ||
		opts.CAFile != "ca.pem" || opts.Advertise != "https://node-a.example:0,node-a.example" ||
		opts.MemoryTarget != 8<<30 || !opts.AutoRebalance || opts.CapacityCommand != "/usr/bin/provision-ttc" || opts.LeaseDuration != 30*time.Second || opts.MaxTransactions != 17 || opts.PageListen != "0.0.0.0:0" ||
		opts.PageAdvertise != "node-a.example:0" || opts.PagingPolicy.TransferBytes != 128<<10 ||
		opts.PagingPolicy.MaxOutstandingBytes != 8<<20 || opts.PagingPolicy.MaxConcurrency != 8 ||
		opts.PagingPolicy.AckBatchPages != 128 || opts.PagingPolicy.AckInterval != 250*time.Microsecond ||
		opts.PagingPolicy.Prefetch != ttcpolicy.PrefetchAdaptive || !opts.PagingPolicy.Adaptive ||
		opts.PagingPolicy.Push != ttcpolicy.PushEagerDrain ||
		!reflect.DeepEqual(opts.Peers, []string{"node-b=https://10.0.0.2:7443,node-b.example"}) {
		t.Fatalf("cluster options=%+v", opts)
	}
	if node, url, name, err := parseClusterPeer(opts.Peers[0]); err != nil || node != "node-b" || url != "https://10.0.0.2:7443" || name != "node-b.example" {
		t.Fatalf("parseClusterPeer=(%q, %q, %q, %v)", node, url, name, err)
	}
}

func TestPrepareDaemonClusterRejectsPartialConfiguration(t *testing.T) {
	_, err := prepareDaemonCluster(daemonClusterOptions{Configured: true, Listen: "127.0.0.1:0", MaxTransactions: 1}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "requires") {
		t.Fatalf("partial cluster configuration error=%v", err)
	}
}

func TestClusterPageAddressesRequireEphemeralPorts(t *testing.T) {
	for _, address := range []string{"127.0.0.1:0", "[::1]:0", "node-a.example:0"} {
		if err := requireEphemeralClusterPagePort("--cluster-page-listen", address); err != nil {
			t.Errorf("ephemeral address %q rejected: %v", address, err)
		}
	}
	if err := requireEphemeralClusterPagePort("--cluster-page-listen", "127.0.0.1:7444"); err == nil || !strings.Contains(err.Error(), "port 0") {
		t.Fatalf("fixed cluster page port error=%v, want port 0 guidance", err)
	}
}

func TestAdaptivePrefetchControllerHysteresis(t *testing.T) {
	policy := ttcpolicy.DefaultPolicy()
	policy.MaxConcurrency = 4
	policy.MaxOutstandingBytes = 8 << 20
	controller := newAdaptivePrefetchController(policy)
	now := time.Unix(1000, 0)
	status := pgalloc.AsyncPagesFileLoadStats{PrefetchBytes: policy.TransferBytes - policy.PageSize}
	if _, _, changed := controller.observe(status, now); changed {
		t.Fatal("initial observation changed the prefetch window")
	}
	status.BytesLoaded = policy.TransferBytes
	status.BytesWaited = policy.PageSize
	status.TotalWaiters = 1
	status.Faults = 4
	status.SequentialFaults = 3
	behind, ahead, changed := controller.observe(status, now.Add(600*time.Millisecond))
	if !changed || behind != 0 || ahead != controller.maxAhead {
		t.Fatalf("blocking-fault update=(%d,%d,%t)", behind, ahead, changed)
	}
	grownAhead := ahead
	for i := 0; i < 8; i++ {
		status.BytesLoaded += 4 * (grownAhead + policy.PageSize)
		behind, ahead, changed = controller.observe(status, now.Add(3*time.Second+time.Duration(i)*250*time.Millisecond))
	}
	wantShrunk := (grownAhead+policy.PageSize)/2 - policy.PageSize
	if !changed || behind != 0 || ahead != wantShrunk {
		t.Fatalf("quiet-throughput update=(%d,%d,%t)", behind, ahead, changed)
	}
}

func TestAdaptivePrefetchControllerShrinksRandomFaults(t *testing.T) {
	policy := ttcpolicy.DefaultPolicy()
	policy.MaxConcurrency = 4
	controller := newAdaptivePrefetchController(policy)
	now := time.Unix(2000, 0)
	status := pgalloc.AsyncPagesFileLoadStats{PrefetchBytes: policy.TransferBytes - policy.PageSize}
	if _, _, changed := controller.observe(status, now); changed {
		t.Fatal("initial observation changed the prefetch window")
	}
	status.BytesLoaded = 8 * policy.TransferBytes
	status.BytesWaited = 8 * policy.PageSize
	status.TotalWaiters = 8
	status.Faults = 8
	behind, ahead, changed := controller.observe(status, now.Add(300*time.Millisecond))
	if !changed || behind != 0 || ahead != policy.TransferBytes/2-policy.PageSize {
		t.Fatalf("random-fault update=(%d,%d,%t)", behind, ahead, changed)
	}
}

func TestAdaptivePrefetchControllerAccumulatesLocalitySamples(t *testing.T) {
	policy := ttcpolicy.DefaultPolicy()
	policy.MaxConcurrency = 4
	controller := newAdaptivePrefetchController(policy)
	now := time.Unix(3000, 0)
	status := pgalloc.AsyncPagesFileLoadStats{PrefetchBytes: policy.TransferBytes - policy.PageSize}
	controller.observe(status, now)
	status.BytesWaited = 2 * policy.PageSize
	status.TotalWaiters = 2
	status.Faults = 2
	status.SequentialFaults = 2
	if _, _, changed := controller.observe(status, now.Add(60*time.Millisecond)); changed {
		t.Fatal("two locality samples changed the window")
	}
	status.BytesWaited += 2 * policy.PageSize
	status.TotalWaiters += 2
	status.Faults += 2
	status.SequentialFaults += 2
	behind, ahead, changed := controller.observe(status, now.Add(120*time.Millisecond))
	if !changed || behind != 0 || ahead != controller.maxAhead {
		t.Fatalf("accumulated sequential update=(%d,%d,%t)", behind, ahead, changed)
	}
}
