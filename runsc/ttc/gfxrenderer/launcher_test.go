// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package gfxrenderer

import (
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

// helperEnv makes the test binary act as the renderer's executable, in the
// mode of its first argument. With helperFakeEnv, its renderer and probe are
// fakes: its renderer writes its arguments to its connection, and its probe
// prints them, or fails if helperFakeEnv is "fail".
const (
	helperEnv     = "GFXRENDERER_TEST_HELPER"
	helperFakeEnv = "GFXRENDERER_TEST_FAKE"
)

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) != "" {
		helperMain()
	}
	os.Exit(m.Run())
}

// helperMain is the renderer's executable: its launcher is the real one.
func helperMain() {
	args := os.Args[1:]
	fake := os.Getenv(helperFakeEnv)
	// The renderer may be an executable that is it with arguments, as ttcd
	// is with _gfx-renderer.
	var rendererArgs []string
	if args[0] == "_gfx-renderer" {
		rendererArgs, args = args[:1], args[1:]
	}
	switch args[0] {
	case "launch":
		if err := ServeLauncher(3, os.Args[0], rendererArgs); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "serve-family":
		if fake != "" {
			conn := os.NewFile(3, "connection")
			fmt.Fprint(conn, strings.Join(os.Args[1:], " "))
			break
		}
		if err := ServeFamily(3); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "probe":
		if fake == "fail" {
			fmt.Fprintln(os.Stderr, "no GPU here")
			os.Exit(1)
		}
		fmt.Printf("%q\n", strings.Join(os.Args[1:], " "))
	}
	os.Exit(0)
}

func TestLauncher(t *testing.T) {
	t.Setenv(helperEnv, "1")
	t.Setenv(helperFakeEnv, "1")
	l, err := StartLauncher(BrokerOptions{Renderer: os.Args[0], Stderr: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	fd, err := l.StartRenderer()
	if err != nil {
		t.Fatalf("StartRenderer: %v", err)
	}
	conn := os.NewFile(uintptr(fd), "renderer")
	got, err := io.ReadAll(conn)
	conn.Close()
	if err != nil || string(got) != "serve-family" {
		t.Errorf("the renderer was started with %q, %v; want serve-family", got, err)
	}
	output, err := l.Probe()
	if err != nil || string(output) != "\"probe\"\n" {
		t.Errorf("Probe: %q, %v", output, err)
	}
}

func TestLauncherProbeFails(t *testing.T) {
	t.Setenv(helperEnv, "1")
	t.Setenv(helperFakeEnv, "fail")
	l, err := StartLauncher(BrokerOptions{Renderer: os.Args[0], Stderr: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := l.Probe(); err == nil || !strings.Contains(err.Error(), "no GPU here") {
		t.Errorf("Probe: %v, want the probe's error", err)
	}
	// The launcher serves on.
	fd, err := l.StartRenderer()
	if err != nil {
		t.Fatalf("StartRenderer after a failed probe: %v", err)
	}
	os.NewFile(uintptr(fd), "renderer").Close()
}

func TestLauncherWithRendererArgs(t *testing.T) {
	t.Setenv(helperEnv, "1")
	t.Setenv(helperFakeEnv, "1")
	l, err := StartLauncher(BrokerOptions{Renderer: os.Args[0], RendererArgs: []string{"_gfx-renderer"}, Stderr: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	fd, err := l.StartRenderer()
	if err != nil {
		t.Fatalf("StartRenderer: %v", err)
	}
	conn := os.NewFile(uintptr(fd), "renderer")
	got, err := io.ReadAll(conn)
	conn.Close()
	if err != nil || string(got) != "_gfx-renderer serve-family" {
		t.Errorf("the renderer was started with %q, %v; want _gfx-renderer serve-family", got, err)
	}
	output, err := l.Probe()
	if err != nil || string(output) != "\"_gfx-renderer probe\"\n" {
		t.Errorf("Probe: %q, %v", output, err)
	}
}
