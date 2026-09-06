// Copyright 2026 The gVisor Authors.
// Licensed under the Apache License, Version 2.0 (the "License");

package client

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHelpAndCompletionAreOffline(t *testing.T) {
	t.Setenv("TTC_SOCKET", filepath.Join(t.TempDir(), "absent.sock"))
	for _, args := range [][]string{{"--help"}, {"run", "--help"}, {"completion", "bash"}} {
		var stdout, stderr bytes.Buffer
		if status := Run(args, strings.NewReader(""), &stdout, &stderr); status != 0 || stdout.Len() == 0 || stderr.Len() != 0 {
			t.Fatalf("Run(%q): status=%d stdout=%q stderr=%q", args, status, stdout.String(), stderr.String())
		}
	}
}

func TestClientRejectsRemovedRootAndDirectOptions(t *testing.T) {
	for _, option := range []string{"--root=/tmp/runtime", "--runsc=/bin/ttcd", "--data-root=/tmp/data", "--direct"} {
		var stdout, stderr bytes.Buffer
		if status := Run([]string{option, "ps"}, strings.NewReader(""), &stdout, &stderr); status != 2 || !strings.Contains(stderr.String(), "unknown global option") {
			t.Fatalf("removed option %q: status=%d stderr=%q", option, status, stderr.String())
		}
	}
}

func TestTTCSocketAndOverride(t *testing.T) {
	t.Setenv("TTC_SOCKET", "/tmp/from-env.sock")
	if got := defaultSocket(); got != "/tmp/from-env.sock" {
		t.Fatalf("defaultSocket=%q", got)
	}
	opts, rest, done, err := parseGlobal([]string{"--daemon-socket", "/tmp/from-flag.sock", "ps"}, os.Stdout)
	if err != nil || done || opts.socket != "/tmp/from-flag.sock" || len(rest) != 1 || rest[0] != "ps" {
		t.Fatalf("parseGlobal=(%+v, %q, %t, %v)", opts, rest, done, err)
	}
}

func TestDaemonStartRequiresExplicitRootBeforeSideEffects(t *testing.T) {
	t.Setenv("TTC_ROOT", "")
	t.Setenv("TTC_DATA_ROOT", "")
	if err := validateDaemonStartEnvironment(); err == nil || !strings.Contains(err.Error(), "TTC_ROOT") {
		t.Fatalf("missing root error=%v", err)
	}
	t.Setenv("TTC_DATA_ROOT", "/legacy")
	if err := validateDaemonStartEnvironment(); err == nil || !strings.Contains(err.Error(), "no longer supported") {
		t.Fatalf("legacy root error=%v", err)
	}
	t.Setenv("TTC_ROOT", "/fresh")
	if err := validateDaemonStartEnvironment(); err != nil {
		t.Fatalf("explicit root: %v", err)
	}
}

func TestNormalizeClientRelativePaths(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		args  []string
		index int
	}{
		{[]string{"save", "-o", "image.tar", "busybox"}, 2},
		{[]string{"load", "--input=archive.tar"}, 1},
		{[]string{"checkpoint", "demo@main", "state.ttc"}, 2},
		{[]string{"restore", "state.ttc"}, 1},
		{[]string{"cp", "local.txt", "demo@main:/tmp/local.txt"}, 1},
	} {
		got, err := normalizeHostPaths(test.args)
		if err != nil {
			t.Fatalf("normalizeHostPaths(%q): %v", test.args, err)
		}
		if !strings.Contains(got[test.index], cwd) {
			t.Fatalf("normalizeHostPaths(%q)=%q", test.args, got)
		}
	}
}
