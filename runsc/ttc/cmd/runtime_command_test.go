// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"reflect"
	"testing"
)

func TestRunscCommandUsesEmbeddedRuntime(t *testing.T) {
	command := runscCommand("/opt/ttc/ttcd", "--root", "/tmp/root", "list")
	want := []string{"/opt/ttc/ttcd", "runsc", "--root", "/tmp/root", "list"}
	if !reflect.DeepEqual(command.Args, want) {
		t.Fatalf("runscCommand(ttcd).Args = %q, want %q", command.Args, want)
	}
}

func TestRunscCommandKeepsExplicitDevelopmentRuntime(t *testing.T) {
	command := runscCommand("/tmp/fake-runsc", "list")
	want := []string{"/tmp/fake-runsc", "list"}
	if !reflect.DeepEqual(command.Args, want) {
		t.Fatalf("runscCommand(fake-runsc).Args = %q, want %q", command.Args, want)
	}
}

func TestEmbeddedRuntimeCommandAnswersOnlyWhatItRuns(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][]string{
		{"list"},
		{"list", "--format=json"},
		{"state"},
		{"delete", "--force", "family"},
		{"timetravel", "family", "fork", "b1"},
		{"timetravel", "--source", "main", "family", "fork"},
		{"timetravel", "--source", "main", "family", "pause", "b1"},
		{"timetravel", "--source", "main", "--filesystem-origins-fd=3", "family", "fork", "b1"},
		{"timetravel", "--signal", "15", "family", "pause", "main"},
		{"timetravel", "--signal", "TERM", "family", "signal", "main"},
		{"timetravel", "family", "signal", "main"},
		{"timetravel", "family", "ps"},
		{"timetravel", "family", "branches", "main"},
		{"timetravel", "family", "pause", "main", "extra"},
	} {
		if output, answered, err := embeddedRuntimeCommand(root, args); answered {
			t.Errorf("embeddedRuntimeCommand(%q) = %q, %v, answered; want it left to the runtime", args, output, err)
		}
	}
	output, answered, err := embeddedRuntimeCommand(root, []string{"list", "--format=json", "--sandbox"})
	if !answered || err != nil || string(output) != "null" {
		t.Fatalf("listing an empty root = %q, %t, %v; want null, answered", output, answered, err)
	}
	for _, args := range [][]string{
		{"state", "missing"},
		{"timetravel", "missing", "branches"},
		{"timetravel", "missing", "pause", "main"},
		{"timetravel", "--signal", "15", "--all", "missing", "signal", "main"},
		{"timetravel", "--source", "main", "missing", "fork", "b1"},
		{"timetravel", "--source", "main", "--paused", "--quarantine", "missing", "fork", "b1", "b2"},
	} {
		if _, answered, err := embeddedRuntimeCommand(root, args); !answered || err == nil {
			t.Errorf("embeddedRuntimeCommand(%q) of a missing sandbox: answered %t, err %v; want an error", args, answered, err)
		}
	}
}
