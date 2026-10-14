// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"archive/tar"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gvisor.dev/gvisor/runsc/ttc/copyhelper"
	"gvisor.dev/gvisor/runsc/ttc/protocol"
)

func TestReadKeyValueFileOptions(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, "env")
	if err := os.WriteFile(env, []byte("\ufeffA=1\r\n# comment\n\n  \nTTC_TEST_IMPORTED\nTTC_TEST_UNSET\nB=two words\n"), 0600); err != nil {
		t.Fatal(err)
	}
	labels := filepath.Join(dir, "labels")
	if err := os.WriteFile(labels, []byte("team=x\nflag\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TTC_TEST_IMPORTED", "from the client")
	got, err := readKeyValueFileOptions([]string{"container", "run", "--name", "box", "--env-file", env, "-e", "A=override", "--label-file=" + labels, "-d", "alpine", "--env-file", "an-argument"})
	if err != nil {
		t.Fatal(err)
	}
	// A file's variables come first, as ttcd lets the command's own options
	// take precedence over them.
	want := []string{"container", "run",
		"--env", "A=1", "--env", "TTC_TEST_IMPORTED=from the client", "--env", "TTC_TEST_UNSET", "--env", "B=two words",
		"--label", "team=x", "--label", "flag",
		"--name", "box", "-e", "A=override", "-d", "alpine", "--env-file", "an-argument"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expanded args=%q\nwant %q", got, want)
	}
	unchanged := []string{"exec", "--env-file", env, "box@main", "true"}
	if got, err := readKeyValueFileOptions(unchanged); err != nil || !reflect.DeepEqual(got, unchanged) {
		t.Fatalf("exec args=(%q, %v)", got, err)
	}
	if _, err := readKeyValueFileOptions([]string{"create", "--env-file", filepath.Join(dir, "missing"), "alpine"}); err == nil || !strings.Contains(err.Error(), "opening environment file") {
		t.Fatalf("missing file error=%v", err)
	}
}

func TestPrepareMemberCopyFromTheHost(t *testing.T) {
	source := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(source, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "f"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	args, stdin, _, finish, err := prepareMemberCopy([]string{"cp", "-aL", source, "box@main:/tmp/"}, nil, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"cp", protocol.CopyPreparedInput, "--archive", "--", "-", "box@main:/tmp/"}
	if !reflect.DeepEqual(args, want) || finish == nil {
		t.Fatalf("args=%q, want %q", args, want)
	}
	contents := make(map[string]string)
	reader := tar.NewReader(stdin)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		contents[header.Name] = string(data)
	}
	if contents["data/f"] != "hello" {
		t.Fatalf("archive=%q", contents)
	}
	if status := finish(0); status != 0 {
		t.Fatalf("status=%d", status)
	}
}

func TestPrepareMemberCopyToTheHost(t *testing.T) {
	dir := t.TempDir()
	guest := filepath.Join(dir, "hostname")
	if err := os.WriteFile(guest, []byte("box\n"), 0644); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(dir, "copied")
	args, stdin, stdout, finish, err := prepareMemberCopy([]string{"cp", "box@main:/etc/hostname", destination}, strings.NewReader("the user's input"), io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"cp", "--", "box@main:/etc/hostname", "-"}; !reflect.DeepEqual(args, want) || finish == nil {
		t.Fatalf("args=%q, want %q", args, want)
	}
	if input, _ := io.ReadAll(stdin); len(input) != 0 {
		t.Fatalf("the command reads the user's input %q", input)
	}
	// What ttcd writes of the container's file, as its copy helper does.
	if err := copyhelper.WriteArchive(guest, false, stdout); err != nil {
		t.Fatal(err)
	}
	if status := finish(0); status != 0 {
		t.Fatalf("status=%d", status)
	}
	if data, err := os.ReadFile(destination); err != nil || string(data) != "box\n" {
		t.Fatalf("copied file=(%q, %v)", data, err)
	}
}

func TestPrepareMemberCopyLeavesStreamsToTTCD(t *testing.T) {
	for _, args := range [][]string{
		{"cp", "-", "box@main:/tmp/"},
		{"cp", "box@main:/etc", "-"},
		{"cp", "box@main:/etc"},
		{"ps"},
	} {
		got, _, _, finish, err := prepareMemberCopy(args, nil, io.Discard, io.Discard)
		if err != nil || finish != nil || !reflect.DeepEqual(got, args) {
			t.Errorf("args %q=(%q, finish %t, %v)", args, got, finish != nil, err)
		}
	}
	if _, _, _, _, err := prepareMemberCopy([]string{"cp", filepath.Join(t.TempDir(), "missing"), "box@main:/"}, nil, io.Discard, io.Discard); err == nil {
		t.Fatal("a missing source was copied")
	}
}

func TestSharedDaemonMemberOfOwnDaemon(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "ttcd.sock")
	if sharedDaemonMember(socket) {
		t.Fatal("a missing socket is another user's")
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if sharedDaemonMember(socket) {
		t.Fatal("this user's socket is another user's")
	}
}
