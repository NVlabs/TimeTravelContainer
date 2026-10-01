// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package gfxrenderer

import (
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"gvisor.dev/gvisor/pkg/gfxproto"
)

// helperEnv makes the test binary act as the renderer's executable, in the
// mode of its first argument.
const helperEnv = "GFXRENDERER_TEST_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) != "" {
		helperMain()
	}
	os.Exit(m.Run())
}

// helperMain is the fake renderer: its launcher is the real one, its
// renderer writes its arguments to its connection, and its probe prints
// them, or fails if the library is "fail".
func helperMain() {
	args := os.Args[1:]
	switch args[0] {
	case "launch":
		opts := Options{}
		if len(args) == 3 && args[1] == "--library" {
			opts.Library = args[2]
		}
		if err := ServeLauncher(3, os.Args[0], opts); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "serve":
		conn := os.NewFile(3, "connection")
		fmt.Fprint(conn, strings.Join(args, " "))
	case "serve-family":
		if err := ServeFamily(3, Options{Backend: BackendVenus}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "probe":
		if len(args) == 3 && args[2] == "fail" {
			fmt.Fprintln(os.Stderr, "no GPU here")
			os.Exit(1)
		}
		fmt.Printf("%q\n", strings.Join(args, " "))
	}
	os.Exit(0)
}

func TestLauncher(t *testing.T) {
	t.Setenv(helperEnv, "1")
	l, err := StartLauncher(BrokerOptions{Renderer: os.Args[0], Options: Options{Library: "/lib"}, Stderr: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for _, test := range []struct {
		flags uint32
		want  string
	}{
		{0, "serve --library /lib"},
		{gfxproto.RendererFlagSnapshots, "serve --library /lib --snapshots"},
	} {
		fd, err := l.StartRenderer(test.flags)
		if err != nil {
			t.Fatalf("StartRenderer: %v", err)
		}
		conn := os.NewFile(uintptr(fd), "renderer")
		got, err := io.ReadAll(conn)
		conn.Close()
		if err != nil || string(got) != test.want {
			t.Errorf("the renderer was started with %q, %v; want %s", got, err, test.want)
		}
	}
	output, err := l.Probe(0)
	if err != nil || string(output) != "\"probe --library /lib\"\n" {
		t.Errorf("Probe: %q, %v", output, err)
	}
	output, err = l.Probe(gfxproto.RendererFlagSnapshots)
	if err != nil || string(output) != "\"probe --library /lib --snapshots\"\n" {
		t.Errorf("Probe of a renderer for snapshots: %q, %v", output, err)
	}
}

func TestLauncherProbeFails(t *testing.T) {
	t.Setenv(helperEnv, "1")
	l, err := StartLauncher(BrokerOptions{Renderer: os.Args[0], Options: Options{Library: "fail"}, Stderr: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := l.Probe(0); err == nil || !strings.Contains(err.Error(), "no GPU here") {
		t.Errorf("Probe: %v, want the probe's error", err)
	}
	// The launcher serves on.
	fd, err := l.StartRenderer(0)
	if err != nil {
		t.Fatalf("StartRenderer after a failed probe: %v", err)
	}
	os.NewFile(uintptr(fd), "renderer").Close()
}
