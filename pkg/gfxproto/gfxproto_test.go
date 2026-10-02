// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package gfxproto

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/unet"
)

func connPair(t *testing.T) (*Conn, *Conn) {
	t.Helper()
	a, b, err := unet.SocketPair(false /* packet */)
	if err != nil {
		t.Fatalf("SocketPair: %v", err)
	}
	fa, err := a.Release()
	if err != nil {
		t.Fatal(err)
	}
	fb, err := b.Release()
	if err != nil {
		t.Fatal(err)
	}
	ca, err := NewConn(fa)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := NewConn(fb)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ca.Close()
		cb.Close()
	})
	return ca, cb
}

func TestMessages(t *testing.T) {
	sentry, renderer := connPair(t)

	// Larger than the socket's buffers.
	cmd := bytes.Repeat([]byte("draw"), 1<<20)
	sent := make(chan error, 1)
	go func() {
		if err := sentry.Send(MsgCreateContext, 1, &CreateContext{CtxID: 7, CapsetID: 3}, []byte("vulkaninfo")); err != nil {
			sent <- err
			return
		}
		sent <- sentry.Send(MsgSubmitCommand, 0, &SubmitCommand{CtxID: 7}, cmd)
	}()

	m, err := renderer.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	var cc CreateContext
	name, err := m.Decode(&cc)
	if err != nil || m.Type != MsgCreateContext || m.Seq != 1 || cc != (CreateContext{CtxID: 7, CapsetID: 3}) || string(name) != "vulkaninfo" {
		t.Errorf("received %v %d %+v %q, %v", m.Type, m.Seq, cc, name, err)
	}
	m, err = renderer.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	var sc SubmitCommand
	got, err := m.Decode(&sc)
	if err != nil || m.Type != MsgSubmitCommand || sc.CtxID != 7 || !bytes.Equal(got, cmd) {
		t.Errorf("received %v %+v with %d bytes, %v", m.Type, sc, len(got), err)
	}
	if err := <-sent; err != nil {
		t.Fatalf("Send: %v", err)
	}

	// A reply carries a file descriptor.
	f, err := os.CreateTemp(t.TempDir(), "blob")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString("blob memory"); err != nil {
		t.Fatal(err)
	}
	if err := renderer.Send(MsgReply, 2, &Reply{Size: 11, MapInfo: 1}, nil, int(f.Fd())); err != nil {
		t.Fatalf("Send: %v", err)
	}
	m, err = sentry.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	defer m.CloseFDs()
	var reply Reply
	if _, err := m.Decode(&reply); err != nil || m.Type != MsgReply || m.Seq != 2 || reply.Size != 11 || len(m.FDs) != 1 {
		t.Fatalf("received %v %d %+v with %d descriptors, %v", m.Type, m.Seq, reply, len(m.FDs), err)
	}
	buf := make([]byte, 11)
	if _, err := unix.Pread(m.FDs[0], buf, 0); err != nil || string(buf) != "blob memory" {
		t.Errorf("reading the received descriptor: %q, %v", buf, err)
	}

	renderer.Close()
	if _, err := sentry.Recv(); !errors.Is(err, ErrClosed) {
		t.Errorf("Recv after the peer closed: %v, want ErrClosed", err)
	}
}

func TestMessageTooLong(t *testing.T) {
	sentry, _ := connPair(t)
	if err := sentry.Send(MsgSubmitCommand, 0, &SubmitCommand{}, make([]byte, MaxMessageSize)); err == nil {
		t.Error("sent a message longer than the maximum")
	}
}
