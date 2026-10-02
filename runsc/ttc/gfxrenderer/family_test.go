// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package gfxrenderer

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/gfxproto"
)

// TestFamilyRenderer checks that the renderer of a family serves the
// connections that the broker gives it, exits once it serves none, and that
// the broker then starts another.
func TestFamilyRenderer(t *testing.T) {
	t.Setenv(helperEnv, "1")
	opts := &BrokerOptions{Renderer: os.Args[0], Stderr: os.Stderr}
	fd, family, err := connectFamilyRenderer(opts, nil)
	if err != nil {
		t.Fatalf("connecting to a family's renderer: %v", err)
	}
	defer func() { family.Close() }()
	conn, err := gfxproto.NewConn(fd)
	if err != nil {
		unix.Close(fd)
		t.Fatal(err)
	}
	// The renderer greets the branch, whether or not the host has Vulkan.
	m, err := conn.Recv()
	if err != nil {
		t.Fatalf("receiving the renderer's greeting: %v", err)
	}
	m.CloseFDs()
	if m.Type != gfxproto.MsgHello {
		t.Errorf("the renderer sent %v, want %v", m.Type, gfxproto.MsgHello)
	}
	conn.Close()

	// Serving no branch, the renderer exits.
	if m, err := family.Recv(); !errors.Is(err, gfxproto.ErrClosed) {
		if err == nil {
			m.CloseFDs()
		}
		t.Fatalf("the idle renderer's control connection: %v, %v; want it closed", m, err)
	}
	fd, next, err := connectFamilyRenderer(opts, family)
	if err != nil {
		t.Fatalf("connecting to a family's renderer after the last exited: %v", err)
	}
	if next == family {
		t.Error("the broker gave a connection to the renderer that exited")
	}
	family = next
	conn, err = gfxproto.NewConn(fd)
	if err != nil {
		unix.Close(fd)
		t.Fatal(err)
	}
	defer conn.Close()
	if m, err := conn.Recv(); err != nil {
		t.Errorf("receiving the next renderer's greeting: %v", err)
	} else {
		m.CloseFDs()
	}
}
