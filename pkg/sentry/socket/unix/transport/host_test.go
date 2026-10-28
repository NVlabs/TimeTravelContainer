// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"testing"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux/errno"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/syserr"
	"gvisor.dev/gvisor/pkg/waiter"
)

func TestHostConnectedEndpointLinuxErrorTranslation(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("Socketpair: %v", err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])

	ep, serr := NewHostSender(fds[0], "test")
	if serr != nil {
		t.Fatalf("NewHostSender: %v", serr)
	}
	// Force fdReadVec's pre-syscall truncation path. The resulting buildIovec
	// error is a Linux EAGAIN, not a host errno. On Darwin, interpreting its
	// numeric value as a host errno used to panic with EDEADLK.
	ep.sndbuf.Store(-1)
	_, _, serr = ep.Recv(context.Background(), nil, RecvArgs{})
	if serr == nil {
		t.Fatal("Recv unexpectedly succeeded")
	}
	if got := serr.ToLinux(); got != errno.EAGAIN {
		t.Fatalf("Recv error = %d, want EAGAIN (%d)", got, errno.EAGAIN)
	}
}

func TestHostConnectedEndpointWouldBlock(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("Socketpair: %v", err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])

	ep, serr := NewHostSender(fds[0], "test")
	if serr != nil {
		t.Fatalf("NewHostSender: %v", serr)
	}
	_, _, serr = ep.Recv(context.Background(), [][]byte{make([]byte, 8)}, RecvArgs{})
	if serr == nil {
		t.Fatal("Recv unexpectedly succeeded")
	}
	if got := serr.ToLinux(); got != errno.EAGAIN {
		t.Fatalf("Recv error = %d, want EAGAIN (%d)", got, errno.EAGAIN)
	}
}

func TestHostConnectedEndpointRoundTrip(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("Socketpair: %v", err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])

	ep, serr := NewHostSender(fds[0], "test")
	if serr != nil {
		t.Fatalf("NewHostSender: %v", serr)
	}
	want := []byte("host socket transport")
	if _, err := unix.Write(fds[1], want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	first := make([]byte, 5)
	second := make([]byte, len(want)-len(first))
	out, _, serr := ep.Recv(context.Background(), [][]byte{first, second}, RecvArgs{})
	if serr != nil {
		t.Fatalf("Recv: %v", serr)
	}
	if out.RecvLen != int64(len(want)) || out.MsgLen != int64(len(want)) {
		t.Fatalf("Recv lengths = (%d, %d), want (%d, %d)", out.RecvLen, out.MsgLen, len(want), len(want))
	}
	got := append(first, second...)
	if string(got) != string(want) {
		t.Fatalf("Recv data = %q, want %q", got, want)
	}

	reply := [][]byte{[]byte("round"), []byte(" trip")}
	n, _, serr := ep.Send(context.Background(), reply, ControlMessages{}, Address{})
	if serr != nil {
		t.Fatalf("Send: %v", serr)
	}
	if n != 10 {
		t.Fatalf("Send length = %d, want 10", n)
	}
	buf := make([]byte, n)
	if n, err := unix.Read(fds[1], buf); err != nil || n != len(buf) {
		t.Fatalf("Read = (%d, %v), want (%d, nil)", n, err, len(buf))
	}
	if got, want := string(buf), "round trip"; got != want {
		t.Fatalf("Send data = %q, want %q", got, want)
	}
}

func TestHostConnectedEndpointShutdownAfterPeerDisconnect(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("Socketpair: %v", err)
	}
	defer unix.Close(fds[0])

	ep, serr := NewHostSender(fds[0], "test")
	if serr != nil {
		t.Fatalf("NewHostSender: %v", serr)
	}
	if err := unix.Close(fds[1]); err != nil {
		t.Fatalf("closing peer: %v", err)
	}

	// Darwin returns ENOTCONN here after the peer disconnects. Closing a
	// port-forwarded connection must not turn that harmless condition into a
	// Sentry-wide panic.
	ep.CloseSend()
	ep.CloseRecv()
}

// TestSCMSenderBeforeSaveIdempotent verifies that calling beforeSave
// on an SCMSender whose host file descriptor is already closed (fd < 0)
// is a safe no-op and does not panic in fdnotifier.RemoveFD.
func TestSCMSenderBeforeSaveIdempotent(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("Socketpair failed: %v", err)
	}
	defer unix.Close(fds[1])

	q := &waiter.Queue{}
	ep, sysErr := NewSCMSender(fds[0], q, "test-addr")
	if sysErr != nil {
		unix.Close(fds[0])
		t.Fatalf("NewSCMSender failed: %v", sysErr)
	}
	if err := ep.Init(); err != nil {
		unix.Close(fds[0])
		t.Fatalf("Init failed: %v", err)
	}

	// First beforeSave cleans up the host FD and sets ep.fd = -1.
	ep.beforeSave()
	if ep.fd != -1 {
		t.Fatalf("after beforeSave, ep.fd = %d, want -1", ep.fd)
	}

	// Second beforeSave must be idempotent and must not panic on fdnotifier.RemoveFD(-1).
	ep.beforeSave()
}

// TestHostSenderClosedHostReadiness verifies that HostReadiness reports
// hangup and readable events when the host socket is closed (fd < 0) so that pollers
// are notified of EOF / disconnection.
func TestHostSenderClosedHostReadiness(t *testing.T) {
	c := &HostSender{fd: -1}

	mask := waiter.ReadableEvents | waiter.EventHUp | waiter.EventRdHUp | waiter.WritableEvents
	got := c.HostReadiness(mask)
	want := (waiter.ReadableEvents | waiter.WritableEvents | waiter.EventHUp | waiter.EventRdHUp) & mask
	if got != want {
		t.Fatalf("HostReadiness(%v) when fd < 0 = %v, want %v", mask, got, want)
	}

	// A severed socket reports writable.
	if gotOut := c.HostReadiness(waiter.EventOut); gotOut != waiter.EventOut {
		t.Errorf("HostReadiness(EventOut) when fd < 0 = %v, want %v", gotOut, waiter.EventOut)
	}

	// Querying EventHUp explicitly must return EventHUp.
	if gotHup := c.HostReadiness(waiter.EventHUp); gotHup != waiter.EventHUp {
		t.Errorf("HostReadiness(EventHUp) when fd < 0 = %v, want %v", gotHup, waiter.EventHUp)
	}

	// Querying EventIn explicitly must return EventIn.
	if gotIn := c.HostReadiness(waiter.EventIn); gotIn != waiter.EventIn {
		t.Errorf("HostReadiness(EventIn) when fd < 0 = %v, want %v", gotIn, waiter.EventIn)
	}
}

// TestConnectionedEndpointClosedHostReadiness verifies that connectionedEndpoint.Readiness
// properly propagates hangup and readable events when backed by a closed HostSender.
func TestConnectionedEndpointClosedHostReadiness(t *testing.T) {
	c := &HostSender{fd: -1}
	ep := &connectionedEndpoint{
		baseEndpoint: baseEndpoint{
			receiver: c,
			peer:     c,
		},
	}

	mask := waiter.EventIn | waiter.EventHUp | waiter.EventRdHUp | waiter.EventOut
	got := ep.Readiness(mask)
	want := (waiter.ReadableEvents | waiter.WritableEvents | waiter.EventHUp | waiter.EventRdHUp) & mask
	if got != want {
		t.Fatalf("Readiness(%v) for closed host endpoint = %v, want %v", mask, got, want)
	}
}

// TestSeveredRecvSend verifies that an SCMSender whose host fd was severed by
// beforeSave reports EOF on receive and EPIPE on send, never EBADF: the
// application's fd is still valid even though the sentry's host fd is gone.
func TestSeveredRecvSend(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("Socketpair failed: %v", err)
	}
	defer unix.Close(fds[1])

	ep, serr := NewSCMSender(fds[0], &waiter.Queue{}, "test-addr")
	if serr != nil {
		unix.Close(fds[0])
		t.Fatalf("NewSCMSender failed: %v", serr)
	}
	if err := ep.Init(); err != nil {
		unix.Close(fds[0])
		t.Fatalf("Init failed: %v", err)
	}

	ep.beforeSave()
	if ep.fd != -1 {
		t.Fatalf("after beforeSave, ep.fd = %d, want -1", ep.fd)
	}

	buf := [][]byte{make([]byte, 8)}
	if _, _, serr := ep.Recv(nil, buf, RecvArgs{}); serr != syserr.ErrClosedForReceive {
		t.Errorf("Recv on severed endpoint returned %v, want %v", serr, syserr.ErrClosedForReceive)
	}
	if _, _, serr := ep.Send(nil, [][]byte{{1}}, ControlMessages{}, Address{}); serr != syserr.ErrClosedForSend {
		t.Errorf("Send on severed endpoint returned %v, want %v", serr, syserr.ErrClosedForSend)
	}
}
