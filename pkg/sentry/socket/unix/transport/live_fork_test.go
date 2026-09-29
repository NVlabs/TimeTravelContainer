// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/waiter"
)

type liveForkTestIDProvider struct {
	next uint64
}

func TestLocalShutdownNotifiesHangupWaiters(t *testing.T) {
	ctx := context.Background()
	a, b := NewPair(ctx, linux.SOCK_STREAM, &liveForkTestIDProvider{})
	defer a.Close(ctx)
	defer b.Close(ctx)

	mask := waiter.EventHUp | waiter.EventRdHUp
	entry, ch := waiter.NewChannelEntry(mask)
	if err := a.EventRegister(&entry); err != nil {
		t.Fatalf("EventRegister: %v", err)
	}
	defer a.EventUnregister(&entry)

	if err := a.Shutdown(tcpip.ShutdownRead | tcpip.ShutdownWrite); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("local shutdown did not notify hangup waiter")
	}
	if got := a.Readiness(mask); got != mask {
		t.Fatalf("Readiness after SHUT_RDWR: got %#x, want %#x", got, mask)
	}
}

func (p *liveForkTestIDProvider) UniqueID() uint64 {
	p.next++
	return p.next
}

type liveForkTestRights struct{}

func (*liveForkTestRights) Clone() RightsControlMessage {
	return &liveForkTestRights{}
}

func (*liveForkTestRights) TransferRights() RightsControlMessage {
	return &liveForkTestRights{}
}

func (*liveForkTestRights) Release(context.Context) {}

func TestValidateLiveForkRejectsQueuedRights(t *testing.T) {
	ctx := context.Background()
	a, b := NewPair(ctx, linux.SOCK_SEQPACKET, &liveForkTestIDProvider{})
	defer a.Close(ctx)
	defer b.Close(ctx)
	if _, err := ValidateLiveFork(a); err != nil {
		t.Fatalf("empty internal socketpair rejected: %v", err)
	}
	n, notify, err := a.SendMsg(ctx, [][]byte{[]byte("rights")}, ControlMessages{Rights: &liveForkTestRights{}}, nil)
	if err != nil || n != 6 {
		t.Fatalf("SendMsg: got (%d, %v), want (6, nil)", n, err)
	}
	if notify != nil {
		notify()
	}
	if _, err := ValidateLiveFork(a); err == nil {
		t.Fatal("socketpair with queued SCM_RIGHTS was accepted")
	}
}
