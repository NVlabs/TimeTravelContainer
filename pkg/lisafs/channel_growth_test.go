// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package lisafs

import (
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/unet"
)

// echoID is the message the growth test's server echoes, slowly.
const echoID = Channel + 1

// growthConn is a connection of a mount of nothing.
type growthConn struct{}

type growthControlFD struct {
	ControlFD
	ControlFDImpl
}

func (fd *growthControlFD) FD() *ControlFD { return &fd.ControlFD }

func (fd *growthControlFD) Close() {}

// Mount implements ConnectionImpl.Mount.
//
// +checklocksread:c.server.renameMu
// +checklocksexclude:c.fdsMu
func (growthConn) Mount(c *Connection, mountNode *Node) (*ControlFD, Statx, int, error) {
	root := &growthControlFD{}
	mountNode.IncRef() // Ref is transferred to ControlFD.
	root.Init(c, mountNode, linux.ModeDirectory, root)
	return root.FD(), Statx{Mode: uint16(linux.S_IFDIR)}, -1, nil
}

func (growthConn) SupportedMessages() []MID { return []MID{Mount, Channel, echoID} }

func (growthConn) MaxMessageSize() uint32 { return MaxMessageSize() }

// echo answers a request with its own payload, a millisecond later, so that
// requests sent at once overlap.
func echo(c *Connection, comm Communicator, payloadLen uint32) (uint32, error) {
	time.Sleep(time.Millisecond)
	return payloadLen, nil
}

// A client starts one channel, keeps one while its requests come one at a
// time, and makes more, up to the most it may, once they come at once.
func TestChannelsGrowOnlyWithConcurrentRequests(t *testing.T) {
	serverSocket, clientSocket, err := unet.SocketPair(false)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer()
	handlers := make([]RPCHandler, echoID+1)
	handlers[Error], handlers[Mount], handlers[Channel], handlers[echoID] = ErrorHandler, MountHandler, ChannelHandler, echo
	server.SetHandlers(handlers)
	conn, err := server.CreateConnection(serverSocket, "/", ConnectionOpts{}, growthConn{})
	if err != nil {
		t.Fatal(err)
	}
	server.StartConnection(conn)
	c, _, _, err := NewClient(clientSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		c.Close()
		server.Wait()
		server.Destroy()
	}()
	if err := c.StartChannels(); err != nil {
		t.Fatal(err)
	}
	channels := func() int {
		c.channelsMu.Lock()
		defer c.channelsMu.Unlock()
		return len(c.channels)
	}
	send := func() error {
		var req, resp MsgDynamic
		req.Randomize(10)
		return c.SndRcvMessage(echoID, uint32(req.SizeBytes()), req.MarshalBytes, resp.CheckedUnmarshal, nil, req.String, resp.String)
	}
	if n := channels(); n != 1 {
		t.Fatalf("a new client has %d channels, want 1", n)
	}
	for i := 0; i < 50; i++ {
		if err := send(); err != nil {
			t.Fatal(err)
		}
	}
	if n := channels(); n != 1 {
		t.Errorf("requests one at a time made %d channels, want 1", n)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if err := send(); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if n := channels(); n < 2 || n > maxChannels() {
		t.Errorf("requests at once made %d channels, want 2 to %d", n, maxChannels())
	}
}
