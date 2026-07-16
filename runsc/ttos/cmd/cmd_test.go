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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	ttosbundle "gvisor.dev/gvisor/runsc/ttos/bundle"
	ttosconsole "gvisor.dev/gvisor/runsc/ttos/console"
	ttosimages "gvisor.dev/gvisor/runsc/ttos/images"
)

func TestParseSelector(t *testing.T) {
	tests := []struct {
		input string
		want  selector
		err   bool
	}{
		{input: "web", want: selector{container: "web", branch: "main"}},
		{input: "web@test-a", want: selector{container: "web", branch: "test-a"}},
		{input: "", err: true},
		{input: "@child", err: true},
		{input: "web@", err: true},
		{input: "web@child@again", err: true},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			got, err := parseSelector(test.input)
			if (err != nil) != test.err {
				t.Fatalf("parseSelector(%q) error: %v, want error=%t", test.input, err, test.err)
			}
			if got != test.want {
				t.Errorf("parseSelector(%q) = %+v, want %+v", test.input, got, test.want)
			}
		})
	}
}

func TestWriteTree(t *testing.T) {
	families := []Family{{
		ID:    "web",
		State: "running",
		Branches: []Branch{
			{ID: "retry", ParentID: "test-a", State: "running", ForkTotalNS: 2_000_000, SourceDowntimeNS: 1_000_000},
			{ID: "test-b", ParentID: "main", State: "paused"},
			{ID: "main", State: "running"},
			{ID: "test-a", ParentID: "main", State: "paused"},
			{ID: "orphan", ParentID: "deleted", State: "running"},
		},
	}}
	var out bytes.Buffer
	writeTree(&out, families)
	want := "web (running)\n" +
		"├── main running\n" +
		"│   ├── test-a paused\n" +
		"│   │   └── retry running fork=2ms downtime=1ms\n" +
		"│   └── test-b paused\n" +
		"└── orphan running parent=deleted (deleted)\n"
	if got := out.String(); got != want {
		t.Errorf("writeTree output:\n%s\nwant:\n%s", got, want)
	}
}

func TestPsFiltersAndUsesQualifiedSelectors(t *testing.T) {
	b := &fakeBackend{families: []Family{
		{ID: "web", State: "running", Branches: []Branch{
			{ID: "main", State: "running"},
			{ID: "child", ParentID: "main", State: "paused"},
			{ID: "old", ParentID: "main", State: "exited"},
		}},
		{ID: "prepared", State: "created", Branches: []Branch{{ID: "main", State: "created"}}},
	}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"ps", "--quiet"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("ps status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := stdout.String(), "web@main\nweb@child\n"; got != want {
		t.Errorf("ps --quiet=%q, want %q", got, want)
	}
	stdout.Reset()
	if status := runCommand([]string{"ps", "--all", "--quiet"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("ps --all status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := stdout.String(), "web@main\nweb@child\nweb@old\nprepared@main\n"; got != want {
		t.Errorf("ps --all --quiet=%q, want %q", got, want)
	}
}

func TestTopTableAndJSON(t *testing.T) {
	b := &fakeBackend{processList: []ProcessInfo{{
		UID: 1000, PID: 7, PPID: 1, PGID: 7, Threads: []int32{7, 8},
		C: 3, TTY: "?", STime: "12:34", Time: "9ms", Cmd: "worker",
	}}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"top", "web@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("top status=%d, stderr=%q", status, stderr.String())
	}
	if b.processTarget != (selector{container: "web", branch: "child"}) {
		t.Errorf("top target=%+v", b.processTarget)
	}
	for _, text := range []string{"UID", "PID", "CMD", "1000", "worker"} {
		if !strings.Contains(stdout.String(), text) {
			t.Errorf("top output %q does not contain %q", stdout.String(), text)
		}
	}
	stdout.Reset()
	if status := runCommand([]string{"top", "--json", "web@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("top --json status=%d, stderr=%q", status, stderr.String())
	}
	var got []ProcessInfo
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil || !reflect.DeepEqual(got, b.processList) {
		t.Errorf("top JSON=%+v err=%v, want %+v", got, err, b.processList)
	}
}

func TestCommandsUseQualifiedBranches(t *testing.T) {
	b := &fakeBackend{families: []Family{{ID: "web", State: "running", Branches: []Branch{{ID: "main", State: "running"}}}}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"fork", "--paused", "web@main", "web@test-a"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("fork status=%d, stderr=%q", status, stderr.String())
	}
	if got := stdout.String(); got != "web@test-a\n" {
		t.Errorf("fork output=%q, want %q", got, "web@test-a\n")
	}
	if b.forkSource != (selector{container: "web", branch: "main"}) || b.forkDestination != "test-a" || !b.forkPaused {
		t.Errorf("fork call = source %+v destination %q paused=%t", b.forkSource, b.forkDestination, b.forkPaused)
	}

	stdout.Reset()
	if status := runCommand([]string{"pause", "web@test-a"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("pause status=%d, stderr=%q", status, stderr.String())
	}
	if b.paused != (selector{container: "web", branch: "test-a"}) {
		t.Errorf("pause target=%+v", b.paused)
	}

	stdout.Reset()
	if status := runCommand([]string{"resume", "web@test-a"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("resume status=%d, stderr=%q", status, stderr.String())
	}
	if b.resumed != (selector{container: "web", branch: "test-a"}) {
		t.Errorf("resume target=%+v", b.resumed)
	}

	stdout.Reset()
	if status := runCommand([]string{"branch", "rm", "web@test-a"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("branch rm status=%d, stderr=%q", status, stderr.String())
	}
	if b.destroyed != (selector{container: "web", branch: "test-a"}) {
		t.Errorf("destroy target=%+v", b.destroyed)
	}
}

func TestForkGeneratesDestination(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"fork", "web"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("fork status=%d, stderr=%q", status, stderr.String())
	}
	if !strings.HasPrefix(b.forkDestination, "fork-") {
		t.Errorf("generated destination %q does not start with fork-", b.forkDestination)
	}
	if got := stdout.String(); got != "web@"+b.forkDestination+"\n" {
		t.Errorf("fork output=%q, want %q", got, "web@"+b.forkDestination+"\n")
	}
}

func TestForkRejectsCrossFamilyDestination(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	status := runCommand([]string{"fork", "web", "other@child"}, &stdout, &stderr, b)
	if status != 1 {
		t.Fatalf("status=%d, want 1", status)
	}
	if !strings.Contains(stderr.String(), "must remain in the source container family") {
		t.Errorf("stderr=%q", stderr.String())
	}
}

func TestBranchRMRejectsMain(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	status := runCommand([]string{"branch", "rm", "web"}, &stdout, &stderr, b)
	if status != 1 {
		t.Fatalf("status=%d, want 1", status)
	}
	if b.destroyed.container != "" {
		t.Errorf("destroy unexpectedly called for %+v", b.destroyed)
	}
}

func TestPullNormalizesDockerReference(t *testing.T) {
	b := &fakeBackend{pullInfo: ttosimages.Info{
		Reference: "docker.io/library/ubuntu:latest",
		Digest:    "sha256:1234",
		Platform:  "linux/amd64",
	}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"pull", "ubuntu"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("pull status=%d, stderr=%q", status, stderr.String())
	}
	if b.pullReference != "docker.io/library/ubuntu:latest" || b.pullSnapshotter != "native" || b.pullUnpack {
		t.Errorf("pull call: reference=%q snapshotter=%q unpack=%t", b.pullReference, b.pullSnapshotter, b.pullUnpack)
	}
	want := "Pulled docker.io/library/ubuntu:latest\nDigest: sha256:1234\nPlatform: linux/amd64\n"
	if got := stdout.String(); got != want {
		t.Errorf("pull output=%q, want %q", got, want)
	}
}

func TestPullOptionsAndQuietOutput(t *testing.T) {
	b := &fakeBackend{pullInfo: ttosimages.Info{Digest: "sha256:5678"}}
	var stdout, stderr bytes.Buffer
	args := []string{"pull", "--platform", "linux/arm64", "--snapshotter", "overlayfs", "--unpack=false", "-q", "example/app:v1"}
	if status := runCommand(args, &stdout, &stderr, b); status != 0 {
		t.Fatalf("pull status=%d, stderr=%q", status, stderr.String())
	}
	if b.pullReference != "docker.io/example/app:v1" || b.pullPlatform != "linux/arm64" || b.pullSnapshotter != "overlayfs" || b.pullUnpack {
		t.Errorf("pull call: reference=%q platform=%q snapshotter=%q unpack=%t", b.pullReference, b.pullPlatform, b.pullSnapshotter, b.pullUnpack)
	}
	if got := stdout.String(); got != "sha256:5678\n" {
		t.Errorf("quiet pull output=%q", got)
	}
}

func TestRunParsesDockerStyleOptions(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	args := []string{"run", "--name", "demo", "-d", "-e", "A=1", "--workdir", "/work", "--user", "123:456", "--entrypoint", "/entry", "-v", "/host/a:/host-data:ro", "--mount", "type=bind,src=/host/b,dst=/config,readonly", "-v", "cache:/cache", "--mount", "type=volume,source=data,target=/data,readonly", "busybox:1.36", "echo", "ok"}
	if status := runCommand(args, &stdout, &stderr, b); status != 0 {
		t.Fatalf("run status=%d, stderr=%q", status, stderr.String())
	}
	if got := stdout.String(); got != "demo\n" {
		t.Errorf("run output=%q, want demo", got)
	}
	if b.runRequest.name != "demo" || b.runRequest.reference != "busybox:1.36" || !b.runRequest.detach || b.runRequest.cwd != "/work" || b.runRequest.user != "123:456" {
		t.Errorf("run request=%+v", b.runRequest)
	}
	if got, want := strings.Join(b.runRequest.args, " "), "echo ok"; got != want {
		t.Errorf("run args=%q, want %q", got, want)
	}
	if got, want := strings.Join(b.runRequest.entrypoint, " "), "/entry"; got != want {
		t.Errorf("entrypoint=%q, want %q", got, want)
	}
	wantMounts := []ttosbundle.HostMount{
		{Source: "/host/a", Destination: "/host-data", Readonly: true},
		{Source: "/host/b", Destination: "/config", Readonly: true},
	}
	if !reflect.DeepEqual(b.runRequest.hostMounts, wantMounts) {
		t.Errorf("host mounts=%+v, want %+v", b.runRequest.hostMounts, wantMounts)
	}
	wantVolumes := []ttosbundle.ManagedVolume{
		{Name: "cache", Destination: "/cache"},
		{Name: "data", Destination: "/data", Readonly: true},
	}
	if !reflect.DeepEqual(b.runRequest.managedVolumes, wantVolumes) {
		t.Errorf("managed volumes=%+v, want %+v", b.runRequest.managedVolumes, wantVolumes)
	}
}

func TestRunRejectsUnmanagedWritableMounts(t *testing.T) {
	for _, args := range [][]string{
		{"run", "-v", "/host:/data", "busybox"},
		{"run", "--mount", "type=bind,source=/host,target=/data", "busybox"},
	} {
		b := &fakeBackend{}
		var stdout, stderr bytes.Buffer
		if status := runCommand(args, &stdout, &stderr, b); status != 1 {
			t.Errorf("runCommand(%q) status=%d, want 1", args, status)
		}
		if b.runRequest.reference != "" {
			t.Errorf("runCommand(%q) invoked backend with %+v", args, b.runRequest)
		}
	}
}

func TestRunForwardsContainerExitStatus(t *testing.T) {
	b := &fakeBackend{runStatus: 42}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"run", "busybox", "false"}, &stdout, &stderr, b); status != 42 {
		t.Fatalf("run status=%d, want 42; stderr=%q", status, stderr.String())
	}
}

func TestCreateAndStart(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"create", "--name", "demo", "busybox", "sleep", "30"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("create status=%d, stderr=%q", status, stderr.String())
	}
	if !b.created || b.runRequest.name != "demo" || strings.Join(b.runRequest.args, " ") != "sleep 30" {
		t.Errorf("create request=%+v created=%t", b.runRequest, b.created)
	}
	stdout.Reset()
	if status := runCommand([]string{"start", "demo"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("start status=%d, stderr=%q", status, stderr.String())
	}
	if b.started != "demo" || !b.startDetach || b.startInteractive {
		t.Errorf("start name=%q detach=%t interactive=%t", b.started, b.startDetach, b.startInteractive)
	}
	if stdout.String() != "demo\n" {
		t.Errorf("start output=%q", stdout.String())
	}
}

func TestAttach(t *testing.T) {
	b := &fakeBackend{waitStatus: uint32(7 << 8)}
	var stdout, stderr bytes.Buffer
	status := runCommand([]string{"attach", "-i", "--detach-keys", "ctrl-a,z", "demo@child"}, &stdout, &stderr, b)
	if status != 7 {
		t.Fatalf("attach status=%d, stderr=%q", status, stderr.String())
	}
	if b.attached != (selector{container: "demo", branch: "child"}) || !b.attachInteractive {
		t.Errorf("attach target=%+v interactive=%t", b.attached, b.attachInteractive)
	}
	if got, want := b.attachDetachKeys, []byte{1, 'z'}; !bytes.Equal(got, want) {
		t.Errorf("attach detach keys=%v, want %v", got, want)
	}
}

func TestAttachDetachIsSuccess(t *testing.T) {
	b := &fakeBackend{err: errDetached}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"attach", "-i", "demo"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("attach status=%d, stderr=%q", status, stderr.String())
	}
	if got, want := b.attachDetachKeys, defaultDetachKeys; !bytes.Equal(got, want) {
		t.Errorf("default detach keys=%v, want %v", got, want)
	}
}

func TestParseDetachKeys(t *testing.T) {
	for _, test := range []struct {
		value string
		want  []byte
	}{
		{value: "ctrl-p,ctrl-q", want: []byte{0x10, 0x11}},
		{value: "CTRL-@,ctrl-[,ctrl-\\,ctrl-_,x", want: []byte{0, 27, 28, 31, 'x'}},
	} {
		got, err := parseDetachKeys(test.value)
		if err != nil {
			t.Errorf("parseDetachKeys(%q): %v", test.value, err)
		} else if !bytes.Equal(got, test.want) {
			t.Errorf("parseDetachKeys(%q)=%v, want %v", test.value, got, test.want)
		}
	}
	for _, value := range []string{"", "ctrl-", "ctrl-1", "too-long"} {
		if _, err := parseDetachKeys(value); err == nil {
			t.Errorf("parseDetachKeys(%q) succeeded", value)
		}
	}
}

func TestDetachKeyFilterAcrossReads(t *testing.T) {
	filter := detachKeyFilter{sequence: append([]byte(nil), defaultDetachKeys...)}
	output, detached := filter.push([]byte("hello\x10"))
	if got, want := string(output), "hello"; got != want || detached {
		t.Fatalf("first push=(%q, %t), want (%q, false)", got, detached, want)
	}
	output, detached = filter.push([]byte("\x11ignored"))
	if len(output) != 0 || !detached {
		t.Fatalf("second push=(%q, %t), want (empty, true)", output, detached)
	}
	if pending := filter.flush(); len(pending) != 0 {
		t.Errorf("flush after detach=%q, want empty", pending)
	}
}

func TestDetachKeyFilterForwardsNearMatchAndFlushesPrefix(t *testing.T) {
	filter := detachKeyFilter{sequence: append([]byte(nil), defaultDetachKeys...)}
	output, detached := filter.push([]byte{'a', 0x10, 'x', 0x10})
	if got, want := output, []byte{'a', 0x10, 'x'}; !bytes.Equal(got, want) || detached {
		t.Fatalf("push=(%v, %t), want (%v, false)", got, detached, want)
	}
	if got, want := filter.flush(), []byte{0x10}; !bytes.Equal(got, want) {
		t.Errorf("flush=%v, want %v", got, want)
	}
}

func TestLogs(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"logs", "--follow", "demo@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("logs status=%d, stderr=%q", status, stderr.String())
	}
	if b.logsTarget != (selector{container: "demo", branch: "child"}) || !b.logsFollow {
		t.Errorf("logs target=%+v follow=%t", b.logsTarget, b.logsFollow)
	}
}

func TestConsoleLogFollowerHandlesPartialAppend(t *testing.T) {
	var encoded bytes.Buffer
	for _, frame := range []struct {
		stream byte
		data   string
	}{
		{stream: ttosconsole.StreamStdout, data: "hello"},
		{stream: ttosconsole.StreamStderr, data: "problem"},
		{stream: ttosconsole.StreamStdout},
		{stream: ttosconsole.StreamStderr},
	} {
		if err := ttosconsole.WriteFrame(&encoded, frame.stream, []byte(frame.data)); err != nil {
			t.Fatalf("WriteFrame: %v", err)
		}
	}
	path := filepath.Join(t.TempDir(), "main.log")
	data := encoded.Bytes()
	if err := os.WriteFile(path, data[:5], 0600); err != nil {
		t.Fatalf("writing partial log: %v", err)
	}
	follower, err := openConsoleLogFollower(path)
	if err != nil {
		t.Fatalf("openConsoleLogFollower: %v", err)
	}
	defer follower.file.Close()
	var stdout, stderr bytes.Buffer
	if progress, err := follower.drain(&stdout, &stderr); err != nil || !progress {
		t.Fatalf("draining partial header: progress=%t err=%v", progress, err)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("partial frame produced stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	appendFile, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatalf("opening append log: %v", err)
	}
	if _, err := appendFile.Write(data[5:]); err != nil {
		appendFile.Close()
		t.Fatalf("appending log: %v", err)
	}
	if err := appendFile.Close(); err != nil {
		t.Fatalf("closing append log: %v", err)
	}
	if progress, err := follower.drain(&stdout, &stderr); err != nil || !progress {
		t.Fatalf("draining completed frames: progress=%t err=%v", progress, err)
	}
	if got := stdout.String(); got != "hello" {
		t.Errorf("stdout=%q, want hello", got)
	}
	if got := stderr.String(); got != "problem" {
		t.Errorf("stderr=%q, want problem", got)
	}
	if !follower.stdoutEOF || !follower.stderrEOF || len(follower.pending) != 0 {
		t.Errorf("follower state: stdoutEOF=%t stderrEOF=%t pending=%d", follower.stdoutEOF, follower.stderrEOF, len(follower.pending))
	}
}

func TestConsoleLogFollowerFollowsLiveRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "main.log")
	w, err := ttosconsole.NewRotatingWriter(path, 28)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	for _, value := range []string{"first", "second"} {
		if err := ttosconsole.WriteFrame(w, ttosconsole.StreamStdout, []byte(value)); err != nil {
			t.Fatalf("WriteFrame(%q): %v", value, err)
		}
	}
	follower, err := openConsoleLogFollower(path)
	if err != nil {
		t.Fatalf("openConsoleLogFollower: %v", err)
	}
	defer follower.file.Close()
	if err := ttosconsole.WriteFrame(w, ttosconsole.StreamStdout, []byte("third")); err != nil {
		t.Fatalf("WriteFrame(third): %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if progress, err := follower.drain(&stdout, &stderr); err != nil || !progress {
		t.Fatalf("drain: progress=%t err=%v", progress, err)
	}
	if got, want := stdout.String(), "firstsecondthird"; got != want {
		t.Errorf("stdout=%q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr=%q, want empty", stderr.String())
	}
}

func TestConsoleLogFollowerReplaysRetainedGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "main.log")
	w, err := ttosconsole.NewRotatingWriter(path, 28)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	for _, value := range []string{"first", "second", "third"} {
		if err := ttosconsole.WriteFrame(w, ttosconsole.StreamStderr, []byte(value)); err != nil {
			t.Fatalf("WriteFrame(%q): %v", value, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	follower, err := openConsoleLogFollower(path)
	if err != nil {
		t.Fatalf("openConsoleLogFollower: %v", err)
	}
	defer follower.file.Close()
	var stdout, stderr bytes.Buffer
	if progress, err := follower.drain(&stdout, &stderr); err != nil || !progress {
		t.Fatalf("drain: progress=%t err=%v", progress, err)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout=%q, want empty", stdout.String())
	}
	if got, want := stderr.String(), "firstsecondthird"; got != want {
		t.Errorf("stderr=%q, want %q", got, want)
	}
}

func TestConsoleDrainHealth(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "console.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatalf("opening lock: %v", err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("locking: %v", err)
	}
	if active, err := consoleDrainActive(lockPath); err != nil || !active {
		t.Fatalf("active probe=(%t, %v), want (true, nil)", active, err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatalf("unlocking: %v", err)
	}
	if active, err := consoleDrainActive(lockPath); err != nil || active {
		t.Fatalf("inactive probe=(%t, %v), want (false, nil)", active, err)
	}
	errorPath := filepath.Join(dir, "console.error.log")
	if err := os.WriteFile(errorPath, []byte("low-level failure\n"), 0600); err != nil {
		t.Fatalf("writing diagnostic: %v", err)
	}
	if err := consoleDrainStoppedError(errorPath); err == nil || !strings.Contains(err.Error(), "low-level failure") {
		t.Fatalf("consoleDrainStoppedError=%v", err)
	}
}

func TestExecParsesBranchAndDockerStyleOptions(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	args := []string{"exec", "-i", "-e", "A=1", "--workdir", "/work", "--user", "123:456", "demo@child", "sh", "-c", "echo ok"}
	if status := runCommand(args, &stdout, &stderr, b); status != 0 {
		t.Fatalf("exec status=%d, stderr=%q", status, stderr.String())
	}
	request := b.execRequest
	if request.target != (selector{container: "demo", branch: "child"}) || !request.interactive || request.cwd != "/work" || request.user != "123:456" {
		t.Errorf("exec request=%+v", request)
	}
	if got := strings.Join(request.env, " "); got != "A=1" {
		t.Errorf("exec env=%q", got)
	}
	if got := strings.Join(request.args, " "); got != "sh -c echo ok" {
		t.Errorf("exec args=%q", got)
	}
}

func TestExecForwardsProcessExitStatus(t *testing.T) {
	b := &fakeBackend{execStatus: 23}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"exec", "demo", "false"}, &stdout, &stderr, b); status != 23 {
		t.Fatalf("exec status=%d, want 23; stderr=%q", status, stderr.String())
	}
}

func TestInspect(t *testing.T) {
	b := &fakeBackend{inspectInfo: InspectInfo{
		Container: ttosbundle.ContainerInfo{Name: "demo", Reference: "docker.io/library/busybox:latest"},
		Runtime:   &Family{ID: "demo", State: "running", Branches: []Branch{{ID: "main", State: "running"}}},
	}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"inspect", "demo"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("inspect status=%d, stderr=%q", status, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"reference": "docker.io/library/busybox:latest"`) || !strings.Contains(stdout.String(), `"state": "running"`) {
		t.Errorf("inspect output=%q", stdout.String())
	}
}

func TestRemove(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"rm", "--force", "demo"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("rm status=%d, stderr=%q", status, stderr.String())
	}
	if b.removed != "demo" || !b.removeForce || stdout.String() != "demo\n" {
		t.Errorf("removed=%q force=%t output=%q", b.removed, b.removeForce, stdout.String())
	}
}

func TestVolumeCommands(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"volume", "create", "cache"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("volume create status=%d stderr=%q", status, stderr.String())
	}
	if b.ensuredVolume != "cache" || stdout.String() != "cache\n" {
		t.Errorf("create ensured=%q output=%q", b.ensuredVolume, stdout.String())
	}

	stdout.Reset()
	if status := runCommand([]string{"volume", "ls", "-q"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("volume ls status=%d stderr=%q", status, stderr.String())
	}
	if stdout.String() != "cache\n" {
		t.Errorf("quiet list output=%q", stdout.String())
	}

	stdout.Reset()
	if status := runCommand([]string{"volume", "inspect", "cache"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("volume inspect status=%d stderr=%q", status, stderr.String())
	}
	var inspected []ttosbundle.VolumeInfo
	if err := json.Unmarshal(stdout.Bytes(), &inspected); err != nil {
		t.Fatalf("decoding volume inspect: %v", err)
	}
	if len(inspected) != 1 || inspected[0].Name != "cache" || inspected[0].Driver != "local-tmpfs" {
		t.Errorf("inspect returned %+v", inspected)
	}

	stdout.Reset()
	if status := runCommand([]string{"volume", "rm", "cache"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("volume rm status=%d stderr=%q", status, stderr.String())
	}
	if b.removedVolume != "cache" || stdout.String() != "cache\n" {
		t.Errorf("remove volume=%q output=%q", b.removedVolume, stdout.String())
	}
}

func TestListJSON(t *testing.T) {
	b := &fakeBackend{families: []Family{{ID: "web", State: "running", Branches: []Branch{{ID: "main", State: "running"}}}}}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"ls", "--json"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("status=%d, stderr=%q", status, stderr.String())
	}
	want := `"id": "web"`
	if !strings.Contains(stdout.String(), want) {
		t.Errorf("JSON output %q does not contain %q", stdout.String(), want)
	}
}

func TestKillParsesSignalAndSelectsBranch(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"kill", "--signal", "SIGTERM", "web@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("kill status=%d, stderr=%q", status, stderr.String())
	}
	if len(b.signals) != 1 || b.signals[0].target != (selector{container: "web", branch: "child"}) || b.signals[0].signal != 15 || b.signals[0].all {
		t.Errorf("signal calls=%+v", b.signals)
	}
}

func TestStopGraceful(t *testing.T) {
	b := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"stop", "--time", "1", "web@child"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("stop status=%d, stderr=%q", status, stderr.String())
	}
	if len(b.signals) != 1 || b.signals[0].signal != 15 {
		t.Errorf("signal calls=%+v, want SIGTERM only", b.signals)
	}
	if b.waited != (selector{container: "web", branch: "child"}) {
		t.Errorf("wait target=%+v", b.waited)
	}
}

func TestStopEscalatesToKill(t *testing.T) {
	b := &fakeBackend{waitRelease: make(chan struct{})}
	var stdout, stderr bytes.Buffer
	if status := runCommand([]string{"stop", "--time", "0", "web"}, &stdout, &stderr, b); status != 0 {
		t.Fatalf("stop status=%d, stderr=%q", status, stderr.String())
	}
	if len(b.signals) != 2 || b.signals[0].signal != 15 || b.signals[1].signal != 9 {
		t.Errorf("signal calls=%+v, want SIGTERM then SIGKILL", b.signals)
	}
}

func TestAcquireConsoleStdinIsExclusiveAndReleasable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "main.stdin.lock")
	target := selector{container: "demo", branch: "main"}
	first, err := acquireConsoleStdin(path, target)
	if err != nil {
		t.Fatalf("first acquireConsoleStdin: %v", err)
	}
	if _, err := acquireConsoleStdin(path, target); err == nil {
		first.Close()
		t.Fatal("second acquireConsoleStdin succeeded")
	} else if !strings.Contains(err.Error(), "stdin for demo@main is already attached") {
		first.Close()
		t.Fatalf("second acquireConsoleStdin error=%q", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("closing first owner: %v", err)
	}
	second, err := acquireConsoleStdin(path, target)
	if err != nil {
		t.Fatalf("reacquireConsoleStdin: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("closing second owner: %v", err)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatalf("stating stdin lock: %v", err)
	} else if info.Mode().Perm() != 0600 {
		t.Errorf("stdin lock mode=%o, want 600", info.Mode().Perm())
	}
}

func TestParseSignal(t *testing.T) {
	for _, test := range []struct {
		value string
		want  int
	}{
		{value: "9", want: 9},
		{value: "KILL", want: 9},
		{value: "sigusr1", want: 10},
	} {
		got, err := parseSignal(test.value)
		if err != nil || got != test.want {
			t.Errorf("parseSignal(%q)=(%d, %v), want (%d, nil)", test.value, got, err, test.want)
		}
	}
	if _, err := parseSignal("NOPE"); err == nil {
		t.Error("parseSignal(NOPE) succeeded")
	}
}

func TestRuntimeBackendUsesRunscCLI(t *testing.T) {
	dir := t.TempDir()
	runsc := filepath.Join(dir, "runsc")
	script := `#!/bin/sh
if [ "$1" != "--root" ] || [ "$2" != "/runtime" ]; then
	echo "unexpected globals: $*" >&2
	exit 2
fi
if [ "$3" = "list" ]; then
	echo '[{"id":"web","status":"running"}]'
	exit 0
fi
if [ "$3" = "timetravel" ] && [ "$4" = "web" ] && [ "$5" = "branches" ]; then
	echo '[{"ID":"main","ParentID":"","State":"running","ForkMetrics":{"SourceDowntime":0,"Kernel":{"Total":0},"Total":0}}]'
	exit 0
fi
if [ "$3" = "timetravel" ] && [ "$4" = "--source" ] && [ "$5" = "main" ] && [ "$6" = "--paused" ] && [ "$7" = "web" ] && [ "$8" = "fork" ] && [ "$9" = "child" ]; then
	echo '{"ID":"child","ParentID":"main","State":"paused","ForkMetrics":{"SourceDowntime":1000000,"Kernel":{"Total":2000000},"Total":3000000}}'
	exit 0
fi
if [ "$3" = "timetravel" ] && [ "$4" = "--console" ] && [ "$5" = "init" ] && [ "$6" = "--console-log" ] && [ "$8" = "--console-log-max-bytes" ] && [ "$9" = "16777216" ] && [ "${10}" = "web" ] && [ "${11}" = "console-follow" ] && [ "${12}" = "child" ]; then
	exit 0
fi
if [ "$3" = "timetravel" ] && [ "$4" = "web" ] && [ "$5" = "ps" ] && [ "$6" = "child" ]; then
	echo '[{"uid":1000,"pid":7,"ppid":1,"pgid":7,"threads":[7,8],"c":3,"tty":"?","stime":"12:34","time":"9ms","cmd":"worker"}]'
	exit 0
fi
if [ "$3" = "timetravel" ] && [ "$4" = "--signal" ] && [ "$5" = "15" ] && [ "$6" = "web" ] && [ "$7" = "signal" ] && [ "$8" = "child" ]; then
	exit 0
fi
if [ "$3" = "timetravel" ] && [ "$4" = "web" ] && [ "$5" = "wait" ] && [ "$6" = "child" ]; then
	echo '{"wait_status":15}'
	exit 0
fi
echo "unexpected command: $*" >&2
exit 2
`
	if err := os.WriteFile(runsc, []byte(script), 0755); err != nil {
		t.Fatalf("writing fake runsc: %v", err)
	}
	dataRoot := filepath.Join(dir, "data")
	containerDir := filepath.Join(dataRoot, "containers", "web")
	bundleDir := filepath.Join(containerDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatalf("creating bundle: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	metadata, err := json.Marshal(ttosbundle.ContainerInfo{Name: "web", BundleDir: bundleDir, State: "running", CreatedAt: time.Now()})
	if err != nil {
		t.Fatalf("encoding metadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(containerDir, "container.json"), metadata, 0600); err != nil {
		t.Fatalf("writing metadata: %v", err)
	}
	consoleDir := filepath.Join(containerDir, "console")
	if err := os.MkdirAll(consoleDir, 0700); err != nil {
		t.Fatalf("creating console directory: %v", err)
	}
	staleRotatedLog := filepath.Join(consoleDir, "child.log"+ttosconsole.RotatedSuffix)
	if err := os.WriteFile(staleRotatedLog, []byte("stale"), 0600); err != nil {
		t.Fatalf("writing stale rotated log: %v", err)
	}
	b := &runtimeBackend{root: "/runtime", runsc: runsc, dataRoot: dataRoot}
	families, err := b.listFamilies()
	if err != nil {
		t.Fatalf("listFamilies: %v", err)
	}
	if len(families) != 1 || families[0].ID != "web" || len(families[0].Branches) != 1 || families[0].Branches[0].ID != "main" {
		t.Fatalf("listFamilies returned %+v", families)
	}
	child, err := b.fork(selector{container: "web", branch: "main"}, "child", true)
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if child.ID != "child" || child.ParentID != "main" || child.State != "paused" || child.ForkTotalNS != 3_000_000 || child.KernelForkNS != 2_000_000 {
		t.Errorf("fork returned %+v", child)
	}
	if _, err := os.Stat(filepath.Join(containerDir, "console", "child.log")); err != nil {
		t.Errorf("fork console log: %v", err)
	}
	if _, err := os.Stat(staleRotatedLog); !os.IsNotExist(err) {
		t.Errorf("stale rotated log still exists: %v", err)
	}
	processes, err := b.processes(selector{container: "web", branch: "child"})
	if err != nil {
		t.Fatalf("processes: %v", err)
	}
	if len(processes) != 1 || processes[0].PID != 7 || processes[0].UID != 1000 || processes[0].Cmd != "worker" {
		t.Errorf("processes returned %+v", processes)
	}
	if err := b.signal(selector{container: "web", branch: "child"}, 15, false); err != nil {
		t.Fatalf("signal: %v", err)
	}
	status, err := b.wait(selector{container: "web", branch: "child"})
	if err != nil || status != 15 {
		t.Errorf("wait returned (%d, %v), want (15, nil)", status, err)
	}
}

func TestRuntimeBackendExecUsesBranchFlag(t *testing.T) {
	dir := t.TempDir()
	runsc := filepath.Join(dir, "runsc")
	script := `#!/bin/sh
if [ "$1" = "--root" ] && [ "$2" = "/runtime" ] &&
   [ "$3" = "exec" ] && [ "$4" = "--branch" ] && [ "$5" = "child" ] &&
   [ "$6" = "--env" ] && [ "$7" = "A=1" ] &&
   [ "$8" = "--cwd" ] && [ "$9" = "/work" ] &&
   [ "${10}" = "--user" ] && [ "${11}" = "123:456" ] &&
   [ "${12}" = "web" ] && [ "${13}" = "echo" ] && [ "${14}" = "ok" ]; then
	echo branch-output
	exit 0
fi
echo "unexpected command: $*" >&2
exit 2
`
	if err := os.WriteFile(runsc, []byte(script), 0755); err != nil {
		t.Fatalf("writing fake runsc: %v", err)
	}
	var stdout, stderr bytes.Buffer
	b := &runtimeBackend{root: "/runtime", runsc: runsc, stdout: &stdout, stderr: &stderr}
	status, err := b.exec(execRequest{
		target: selector{container: "web", branch: "child"},
		args:   []string{"echo", "ok"},
		env:    []string{"A=1"},
		cwd:    "/work",
		user:   "123:456",
	})
	if err != nil || status != 0 {
		t.Fatalf("exec returned status=%d err=%v stderr=%q", status, err, stderr.String())
	}
	if stdout.String() != "branch-output\n" {
		t.Errorf("stdout=%q", stdout.String())
	}
}

func TestListFamiliesMergesPersistentStateAndReconcilesStaleRunning(t *testing.T) {
	dir := t.TempDir()
	runsc := filepath.Join(dir, "runsc")
	if err := os.WriteFile(runsc, []byte(`#!/bin/sh
if [ "$3" = "list" ]; then
	echo '[]'
	exit 0
fi
exit 2
`), 0755); err != nil {
		t.Fatalf("writing fake runsc: %v", err)
	}

	dataRoot := filepath.Join(dir, "data")
	bundleDir := filepath.Join(dataRoot, "containers", "demo", "bundle")
	if err := os.MkdirAll(bundleDir, 0700); err != nil {
		t.Fatalf("creating bundle: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	metadata, err := json.Marshal(ttosbundle.ContainerInfo{
		Name:      "demo",
		BundleDir: bundleDir,
		State:     "created",
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("encoding metadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataRoot, "containers", "demo", "container.json"), metadata, 0600); err != nil {
		t.Fatalf("writing metadata: %v", err)
	}

	b := &runtimeBackend{root: "/runtime", runsc: runsc, dataRoot: dataRoot}
	families, err := b.listFamilies()
	if err != nil {
		t.Fatalf("listFamilies: %v", err)
	}
	if len(families) != 1 || families[0].ID != "demo" || families[0].State != "created" || families[0].Branches[0].State != "created" {
		t.Fatalf("listFamilies returned %+v", families)
	}

	store := ttosbundle.Store{Root: dataRoot}
	if err := store.UpdateState("demo", "running", nil); err != nil {
		t.Fatalf("marking container running: %v", err)
	}
	families, err = b.listFamilies()
	if err != nil {
		t.Fatalf("reconciling listFamilies: %v", err)
	}
	if len(families) != 1 || families[0].State != "exited" || families[0].Branches[0].State != "exited" {
		t.Fatalf("reconciled listFamilies returned %+v", families)
	}
	info, err := store.LoadContainer("demo")
	if err != nil {
		t.Fatalf("loading reconciled metadata: %v", err)
	}
	if info.State != "exited" || info.ExitedAt == nil {
		t.Errorf("reconciled metadata = %+v, want exited with timestamp", info)
	}
}

type signalCall struct {
	target selector
	signal int
	all    bool
}

type fakeBackend struct {
	families          []Family
	processTarget     selector
	processList       []ProcessInfo
	err               error
	forkSource        selector
	forkDestination   string
	forkPaused        bool
	paused            selector
	resumed           selector
	destroyed         selector
	signals           []signalCall
	waited            selector
	waitStatus        uint32
	waitRelease       chan struct{}
	pullReference     string
	pullPlatform      string
	pullSnapshotter   string
	pullUnpack        bool
	pullInfo          ttosimages.Info
	runRequest        runRequest
	runStatus         int
	created           bool
	started           string
	startDetach       bool
	startInteractive  bool
	attached          selector
	attachInteractive bool
	attachDetachKeys  []byte
	logsTarget        selector
	logsFollow        bool
	inspectInfo       InspectInfo
	removed           string
	removeForce       bool
	execRequest       execRequest
	execStatus        int
	volumes           []ttosbundle.VolumeInfo
	ensuredVolume     string
	removedVolume     string
}

func (b *fakeBackend) create(request runRequest) error {
	b.runRequest = request
	b.created = true
	return b.err
}

func (b *fakeBackend) run(request runRequest) (int, error) {
	b.runRequest = request
	return b.runStatus, b.err
}

func (b *fakeBackend) start(name string, detach, interactive bool) (int, error) {
	b.started = name
	b.startDetach = detach
	b.startInteractive = interactive
	return b.runStatus, b.err
}

func (b *fakeBackend) attach(target selector, interactive bool, detachKeys []byte) (uint32, error) {
	b.attached = target
	b.attachInteractive = interactive
	b.attachDetachKeys = append([]byte(nil), detachKeys...)
	return b.waitStatus, b.err
}

func (b *fakeBackend) logs(target selector, follow bool) error {
	b.logsTarget = target
	b.logsFollow = follow
	return b.err
}

func (b *fakeBackend) exec(request execRequest) (int, error) {
	b.execRequest = request
	return b.execStatus, b.err
}

func (b *fakeBackend) inspect(name string) (InspectInfo, error) {
	return b.inspectInfo, b.err
}

func (b *fakeBackend) remove(name string, force bool) error {
	b.removed = name
	b.removeForce = force
	return b.err
}

func (b *fakeBackend) pull(reference, platform, snapshotter string, unpack bool) (ttosimages.Info, error) {
	b.pullReference = reference
	b.pullPlatform = platform
	b.pullSnapshotter = snapshotter
	b.pullUnpack = unpack
	return b.pullInfo, b.err
}

func (b *fakeBackend) listFamilies() ([]Family, error) {
	return b.families, b.err
}

func (b *fakeBackend) processes(target selector) ([]ProcessInfo, error) {
	b.processTarget = target
	return b.processList, b.err
}

func (b *fakeBackend) fork(source selector, destination string, paused bool) (Branch, error) {
	b.forkSource = source
	b.forkDestination = destination
	b.forkPaused = paused
	if b.err != nil {
		return Branch{}, b.err
	}
	return Branch{ID: destination, ParentID: source.branch, State: "paused"}, nil
}

func (b *fakeBackend) pause(target selector) error {
	b.paused = target
	return b.err
}

func (b *fakeBackend) resume(target selector) error {
	b.resumed = target
	return b.err
}

func (b *fakeBackend) destroy(target selector) error {
	b.destroyed = target
	return b.err
}

func (b *fakeBackend) signal(target selector, signal int, all bool) error {
	b.signals = append(b.signals, signalCall{target: target, signal: signal, all: all})
	if signal == 9 && b.waitRelease != nil {
		close(b.waitRelease)
	}
	return b.err
}

func (b *fakeBackend) wait(target selector) (uint32, error) {
	b.waited = target
	if b.waitRelease != nil {
		<-b.waitRelease
	}
	return b.waitStatus, b.err
}

func (b *fakeBackend) ensureVolume(name string) (ttosbundle.VolumeInfo, error) {
	b.ensuredVolume = name
	if b.err != nil {
		return ttosbundle.VolumeInfo{}, b.err
	}
	for _, volume := range b.volumes {
		if volume.Name == name {
			return volume, nil
		}
	}
	volume := ttosbundle.VolumeInfo{Name: name, Driver: "local-tmpfs"}
	b.volumes = append(b.volumes, volume)
	return volume, nil
}

func (b *fakeBackend) listVolumes() ([]ttosbundle.VolumeInfo, error) {
	return append([]ttosbundle.VolumeInfo(nil), b.volumes...), b.err
}

func (b *fakeBackend) inspectVolume(name string) (ttosbundle.VolumeInfo, error) {
	for _, volume := range b.volumes {
		if volume.Name == name {
			return volume, b.err
		}
	}
	return ttosbundle.VolumeInfo{}, fmt.Errorf("volume %q does not exist", name)
}

func (b *fakeBackend) removeVolume(name string) error {
	b.removedVolume = name
	return b.err
}
