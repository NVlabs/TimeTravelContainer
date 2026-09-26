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

package tcp_test

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"testing"
	"time"
	"unsafe"

	"gvisor.dev/gvisor/pkg/state"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/loopback"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp/test/e2e"
	testcontext "gvisor.dev/gvisor/pkg/tcpip/transport/tcp/testing/context"
	"gvisor.dev/gvisor/pkg/waiter"
)

func restoredEndpoints(t *testing.T, s *stack.Stack) []stack.RestoredEndpoint {
	t.Helper()
	stackType := reflect.TypeOf(s).Elem()
	sf, ok := stackType.FieldByName("restoredEndpoints")
	if !ok {
		t.Fatalf("Field restoredEndpoints not found")
	}
	ptr := unsafe.Pointer(uintptr(unsafe.Pointer(s)) + sf.Offset)
	return *(*[]stack.RestoredEndpoint)(ptr)
}

func endpointWaiterQueue(t *testing.T, ep tcpip.Endpoint) *waiter.Queue {
	t.Helper()
	v := reflect.ValueOf(ep)
	if v.Kind() != reflect.Pointer || v.Elem().Kind() != reflect.Struct {
		t.Fatalf("endpoint has unexpected type %T", ep)
	}
	sf, ok := v.Elem().Type().FieldByName("waiterQueue")
	if !ok {
		t.Fatalf("endpoint %T has no waiterQueue", ep)
	}
	ptr := unsafe.Pointer(v.Pointer() + sf.Offset)
	return *(**waiter.Queue)(ptr)
}

func configureLoopback(t *testing.T, s *stack.Stack) {
	t.Helper()
	if err := s.CreateNIC(1, loopback.New()); err != nil {
		t.Fatalf("CreateNIC: %v", err)
	}
	if err := s.AddProtocolAddress(1, tcpip.ProtocolAddress{
		Protocol:          header.IPv4ProtocolNumber,
		AddressWithPrefix: tcpip.AddrFrom4([4]byte{127, 0, 0, 1}).WithPrefix(),
	}, stack.AddressProperties{}); err != nil {
		t.Fatalf("AddProtocolAddress: %v", err)
	}
	if err := s.AddProtocolAddress(1, tcpip.ProtocolAddress{
		Protocol:          header.IPv6ProtocolNumber,
		AddressWithPrefix: tcpip.AddrFrom16([16]byte{15: 1}).WithPrefix(),
	}, stack.AddressProperties{}); err != nil {
		t.Fatalf("AddProtocolAddress IPv6: %v", err)
	}
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}})
}

func TestTCPDualStackListeningEndpointAcceptsIPv4AfterRestore(t *testing.T) {
	s := stack.New(stack.Options{
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
	})
	defer s.Destroy()
	configureLoopback(t, s)
	listener, err := gonet.ListenTCP(s, tcpip.FullAddress{Port: 8080}, header.IPv6ProtocolNumber)
	if err != nil {
		t.Fatalf("ListenTCP IPv6 wildcard: %v", err)
	}
	defer listener.Close()

	s.SetLiveForkSaveConfig(true)
	var buf bytes.Buffer
	if _, err := state.Save(context.Background(), &buf, s); err != nil {
		t.Fatalf("Save: %v", err)
	}
	s.SetLiveForkSaveConfig(false)
	restored := stack.New(stack.Options{
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
	})
	defer restored.Destroy()
	if _, err := state.Load(context.Background(), bytes.NewReader(buf.Bytes()), restored); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := restored.PrepareLiveForkRestore(); err != nil {
		t.Fatalf("PrepareLiveForkRestore: %v", err)
	}
	eps := restoredEndpoints(t, restored)
	if len(eps) != 1 {
		t.Fatalf("restored endpoint count: got %d, want 1", len(eps))
	}
	ep := eps[0].(tcpip.Endpoint)
	restoredListener := gonet.NewTCPListener(restored, endpointWaiterQueue(t, ep), ep)
	defer restoredListener.Close()
	restored.Restore()

	acceptResult := make(chan error, 1)
	go func() {
		conn, err := restoredListener.Accept()
		if err == nil {
			_, err = io.WriteString(conn, "dual-stack")
			conn.Close()
		}
		acceptResult <- err
	}()
	client, err := gonet.DialTCP(restored, tcpip.FullAddress{
		NIC: 1, Addr: tcpip.AddrFrom4([4]byte{127, 0, 0, 1}), Port: 8080,
	}, header.IPv4ProtocolNumber)
	if err != nil {
		t.Fatalf("IPv4 DialTCP after restore: %v", err)
	}
	defer client.Close()
	if err := client.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	got, err := io.ReadAll(client)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if want := "dual-stack"; string(got) != want {
		t.Fatalf("payload: got %q, want %q", got, want)
	}
	if err := <-acceptResult; err != nil {
		t.Fatalf("Accept: %v", err)
	}
}

func TestTCPListeningEndpointAcceptsAfterRestore(t *testing.T) {
	s := stack.New(stack.Options{
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
	})
	defer s.Destroy()
	configureLoopback(t, s)
	listener, err := gonet.ListenTCP(s, tcpip.FullAddress{
		NIC: 1, Addr: tcpip.AddrFrom4([4]byte{127, 0, 0, 1}), Port: 8080,
	}, header.IPv4ProtocolNumber)
	if err != nil {
		t.Fatalf("ListenTCP: %v", err)
	}
	defer listener.Close()

	var buf bytes.Buffer
	if _, err := state.Save(context.Background(), &buf, s); err != nil {
		t.Fatalf("Save: %v", err)
	}

	restored := stack.New(stack.Options{
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
	})
	defer restored.Destroy()
	if _, err := state.Load(context.Background(), bytes.NewReader(buf.Bytes()), restored); err != nil {
		t.Fatalf("Load: %v", err)
	}
	configureLoopback(t, restored)
	eps := restoredEndpoints(t, restored)
	if len(eps) != 1 {
		t.Fatalf("restored endpoint count: got %d, want 1", len(eps))
	}
	ep, ok := eps[0].(tcpip.Endpoint)
	if !ok {
		t.Fatalf("restored endpoint has type %T, want tcpip.Endpoint", eps[0])
	}
	restoredListener := gonet.NewTCPListener(restored, endpointWaiterQueue(t, ep), ep)
	defer restoredListener.Close()
	restored.Restore()

	acceptResult := make(chan error, 1)
	go func() {
		conn, err := restoredListener.Accept()
		if err != nil {
			acceptResult <- err
			return
		}
		defer conn.Close()
		_, err = io.WriteString(conn, "restored")
		acceptResult <- err
	}()
	client, err := gonet.DialTCP(restored, tcpip.FullAddress{
		NIC: 1, Addr: tcpip.AddrFrom4([4]byte{127, 0, 0, 1}), Port: 8080,
	}, header.IPv4ProtocolNumber)
	if err != nil {
		t.Fatalf("DialTCP after restore: %v", err)
	}
	defer client.Close()
	if err := client.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	got, err := io.ReadAll(client)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if want := "restored"; string(got) != want {
		t.Fatalf("payload: got %q, want %q", got, want)
	}
	if err := <-acceptResult; err != nil {
		t.Fatalf("Accept: %v", err)
	}
}

func TestTCPMigration(t *testing.T) {
	testCases := []struct {
		name                 string
		allowMigration       bool
		removeConf           bool
		hasRouteAfterRestore bool
		wantOriginalState    tcp.EndpointState
		wantRestoredState    tcp.EndpointState
		expectRestored       bool
	}{
		{
			name:                 "MigrationAllowed_RemoveConf_RouteExists",
			allowMigration:       true,
			removeConf:           true,
			hasRouteAfterRestore: true,
			wantOriginalState:    tcp.StateEstablished,
			wantRestoredState:    tcp.StateEstablished,
			expectRestored:       true,
		},
		{
			name:                 "MigrationDisabled_RemoveConf",
			allowMigration:       false,
			removeConf:           true,
			hasRouteAfterRestore: true,
			wantOriginalState:    tcp.StateError,
			wantRestoredState:    tcp.StateError,
			expectRestored:       true,
		},
		{
			name:                 "MigrationAllowed_RemoveConf_NoRoute",
			allowMigration:       true,
			removeConf:           true,
			hasRouteAfterRestore: false,
			wantOriginalState:    tcp.StateEstablished,
			wantRestoredState:    tcp.StateError,
			expectRestored:       true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Create stack.
			c := testcontext.New(t, e2e.DefaultMTU)
			defer c.Cleanup()

			c.Stack().SetAllowLiveTCPMigration(tc.allowMigration)
			c.Stack().SetRemoveConf(tc.removeConf)

			// Establish connection.
			c.CreateConnected(testcontext.TestInitialSequenceNumber, 30000, -1 /* epRcvBuf */)

			if got, want := tcp.EndpointState(c.EP.State()), tcp.StateEstablished; got != want {
				t.Fatalf("Unexpected endpoint state: want %v, got %v", want, got)
			}

			// Save the stack.
			var buf bytes.Buffer
			saveStats, err := state.Save(context.Background(), &buf, c.Stack())
			if err != nil {
				t.Fatalf("Save failed: %v", err)
			}
			t.Logf("Save stats:\n%s", saveStats.String())

			if got, want := tcp.EndpointState(c.EP.State()), tc.wantOriginalState; got != want {
				t.Fatalf("Unexpected original endpoint state after save: want %v, got %v", want, got)
			}

			// Restore the stack.
			restoredStack := stack.New(stack.Options{
				TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
				NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
			})
			restoredStack.SetAllowLiveTCPMigration(tc.allowMigration)
			defer restoredStack.Destroy()

			loadStats, err := state.Load(context.Background(), bytes.NewReader(buf.Bytes()), restoredStack)
			if err != nil {
				t.Fatalf("Load failed: %v", err)
			}
			t.Logf("Load stats:\n%s", loadStats.String())

			if tc.hasRouteAfterRestore {
				// Recreate the NIC 1 with same IP.
				ep := channel.New(1000, e2e.DefaultMTU, "")
				if err := restoredStack.CreateNIC(1, ep); err != nil {
					t.Fatalf("CreateNIC failed: %v", err)
				}
				if err := restoredStack.AddProtocolAddress(1, tcpip.ProtocolAddress{
					Protocol:          header.IPv4ProtocolNumber,
					AddressWithPrefix: testcontext.StackAddrWithPrefix,
				}, stack.AddressProperties{}); err != nil {
					t.Fatalf("AddProtocolAddress failed: %v", err)
				}
				restoredStack.SetRouteTable([]tcpip.Route{
					{
						Destination: header.IPv4EmptySubnet,
						NIC:         1,
					},
				})
			}

			stackType := reflect.TypeOf(restoredStack).Elem()
			sf, ok := stackType.FieldByName("restoredEndpoints")
			if !ok {
				t.Fatalf("Field restoredEndpoints not found")
			}
			offset := sf.Offset
			ptr := unsafe.Pointer(uintptr(unsafe.Pointer(restoredStack)) + offset)
			restoredEndpointsPtr := (*[]stack.RestoredEndpoint)(ptr)
			restoredEndpoints := *restoredEndpointsPtr

			var restoredEP tcpip.Endpoint
			if tc.expectRestored {
				if len(restoredEndpoints) != 1 {
					t.Fatalf("Expected 1 restored endpoint, got %d", len(restoredEndpoints))
				}
				var ok bool
				restoredEP, ok = restoredEndpoints[0].(tcpip.Endpoint)
				if !ok {
					t.Fatalf("Restored endpoint does not implement tcpip.Endpoint")
				}
			} else {
				if len(restoredEndpoints) != 0 {
					t.Fatalf("Expected 0 restored endpoints, got %d", len(restoredEndpoints))
				}
			}

			// Now call Restore.
			restoredStack.Restore()

			if tc.expectRestored {
				got, want := tcp.EndpointState(restoredEP.State()), tc.wantRestoredState
				t.Logf("Subtest %s: got state: %v, want state: %v", tc.name, got, want)
				if got != want {
					t.Fatalf("Unexpected restored endpoint state: want %v, got %v", want, got)
				}
			}
		})
	}
}
