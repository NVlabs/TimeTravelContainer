// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os/exec"
	"slices"
	"testing"
)

func TestEmbeddedRuntimeArgv0(t *testing.T) {
	for _, test := range []struct {
		argv0 string
		want  bool
	}{
		{"/usr/local/bin/runsc", true},
		{"runsc-gofer", true},
		{"runsc-exec", true},
		{"ttc-sentry[demo@family]", false},
		{"/usr/local/bin/ttcd", false},
		{"ttcd-runsc", false},
		{"unrelated-runsc-gofer", false},
	} {
		if got := embeddedRuntimeArgv0(test.argv0); got != test.want {
			t.Errorf("embeddedRuntimeArgv0(%q) = %t, want %t", test.argv0, got, test.want)
		}
	}
}

func TestSentryArgv0(t *testing.T) {
	for _, test := range []struct {
		argv0 string
		want  bool
	}{
		{"ttc-sentry[demo@family]", true},
		{"gvisor_sentry", true},
		{"/some/dir/gvisor_sentry", true},
		{"runsc-gofer", false},
		{"/usr/local/bin/ttcd", false},
		{"ttc-sentry", false},
	} {
		if got := sentryArgv0(test.argv0); got != test.want {
			t.Errorf("sentryArgv0(%q) = %t, want %t", test.argv0, got, test.want)
		}
	}
}

func TestRendererArgs(t *testing.T) {
	for _, test := range []struct {
		args []string
		want []string
		ok   bool
	}{
		{[]string{"ttc-gfx-renderer", "serve-family"}, []string{"serve-family"}, true},
		{[]string{"/usr/local/bin/ttcd", "_gfx-renderer", "probe", "--unconfined"}, []string{"probe", "--unconfined"}, true},
		{[]string{"/usr/local/bin/ttcd", "--socket", "/tmp/ttcd.sock"}, nil, false},
		{[]string{"runsc-gofer", "gofer"}, nil, false},
		{[]string{"/usr/local/bin/ttcd"}, nil, false},
	} {
		got, ok := rendererArgs(test.args)
		if ok != test.ok || !slices.Equal(got, test.want) {
			t.Errorf("rendererArgs(%q) = %q, %t, want %q, %t", test.args, got, ok, test.want, test.ok)
		}
	}
}

func TestInheritedChildren(t *testing.T) {
	child := exec.Command("sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatalf("starting a child: %v", err)
	}
	defer func() {
		child.Process.Kill()
		child.Wait()
	}()
	if children := inheritedChildren(); !slices.Contains(children, child.Process.Pid) {
		t.Errorf("inheritedChildren() = %v, want it to hold %d", children, child.Process.Pid)
	}
}
