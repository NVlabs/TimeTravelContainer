// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tcpconntrack_test

import (
	"bytes"
	"context"
	"testing"

	"gvisor.dev/gvisor/pkg/state"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcpconntrack"
)

// restored returns a copy of tcb that has been saved and loaded.
func restored(t *testing.T, tcb *tcpconntrack.TCB) *tcpconntrack.TCB {
	t.Helper()
	var buf bytes.Buffer
	if _, err := state.Save(context.Background(), &buf, tcb); err != nil {
		t.Fatalf("state.Save: %v", err)
	}
	var loaded tcpconntrack.TCB
	if _, err := state.Load(context.Background(), &buf, &loaded); err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	return &loaded
}

func TestRestoredSynSent(t *testing.T) {
	const iss, irs = 1234, 789
	tcp := make(header.TCP, header.TCPMinimumSize)
	tcp.Encode(&header.TCPFields{
		SeqNum:     iss,
		DataOffset: header.TCPMinimumSize,
		Flags:      header.TCPFlagSyn,
		WindowSize: 30000,
	})
	var tcb tcpconntrack.TCB
	tcb.Init(tcp, dataLen(tcp))

	loaded := restored(t, &tcb)
	tcp.Encode(&header.TCPFields{
		SeqNum:     irs,
		AckNum:     iss + 1,
		DataOffset: header.TCPMinimumSize,
		Flags:      header.TCPFlagSyn | header.TCPFlagAck,
		WindowSize: 50000,
	})
	if r := loaded.UpdateStateReply(tcp, dataLen(tcp)); r != tcpconntrack.ResultAlive {
		t.Fatalf("a restored SYN-SENT TCB's SYN-ACK: got %v, want %v", r, tcpconntrack.ResultAlive)
	}
}

func TestRestoredConnected(t *testing.T) {
	const iss, irs = 1234, 789
	loaded := restored(t, connected(t, iss, irs, 30000, 50000))
	tcp := make(header.TCP, header.TCPMinimumSize)
	tcp.Encode(&header.TCPFields{
		SeqNum:     iss + 1,
		AckNum:     irs + 1,
		DataOffset: header.TCPMinimumSize,
		Flags:      header.TCPFlagAck,
		WindowSize: 30000,
	})
	if r := loaded.UpdateStateOriginal(tcp, dataLen(tcp)); r != tcpconntrack.ResultAlive {
		t.Fatalf("a restored connection's ACK: got %v, want %v", r, tcpconntrack.ResultAlive)
	}
	tcp.Encode(&header.TCPFields{
		SeqNum:     irs + 1,
		AckNum:     iss + 1,
		DataOffset: header.TCPMinimumSize,
		Flags:      header.TCPFlagRst | header.TCPFlagAck,
		WindowSize: 50000,
	})
	if r := loaded.UpdateStateReply(tcp, dataLen(tcp)); r != tcpconntrack.ResultReset {
		t.Fatalf("a restored connection's RST: got %v, want %v", r, tcpconntrack.ResultReset)
	}
}

func TestRestoredEmpty(t *testing.T) {
	if loaded := restored(t, &tcpconntrack.TCB{}); !loaded.IsEmpty() {
		t.Errorf("a restored empty TCB is not empty")
	}
}
