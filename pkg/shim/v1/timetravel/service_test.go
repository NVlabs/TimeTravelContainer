// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package timetravel

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containerd/ttrpc"
	"google.golang.org/protobuf/types/known/anypb"
)

type fakeController struct {
	operation string
	request   Request
}

func (f *fakeController) ControlTimeTravel(_ context.Context, operation string, req *Request) ([]byte, error) {
	f.operation = operation
	f.request = *req
	return []byte(`{"ok":true}`), nil
}

func invoke(t *testing.T, operation string, request Request) (*anypb.Any, error) {
	t.Helper()
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	envelope := &anypb.Any{TypeUrl: RequestTypeURL, Value: data}
	controller := &fakeController{}
	response, err := ServiceDesc(controller).Methods[operation](context.Background(), func(value interface{}) error {
		target := value.(*anypb.Any)
		target.TypeUrl = envelope.TypeUrl
		target.Value = envelope.Value
		return nil
	})
	if err != nil {
		return nil, err
	}
	if controller.operation != operation || controller.request != request {
		t.Fatalf("controller call=%q %+v, want %q %+v", controller.operation, controller.request, operation, request)
	}
	return response.(*anypb.Any), nil
}

func TestServiceMethods(t *testing.T) {
	for _, test := range []struct {
		operation string
		request   Request
	}{
		{"List", Request{ContainerID: "demo"}},
		{"Fork", Request{ContainerID: "demo", Source: "main", Destination: "child", Paused: true}},
		{"Pause", Request{ContainerID: "demo", Branch: "child"}},
		{"Resume", Request{ContainerID: "demo", Branch: "child"}},
		{"Destroy", Request{ContainerID: "demo", Branch: "child"}},
	} {
		t.Run(test.operation, func(t *testing.T) {
			response, err := invoke(t, test.operation, test.request)
			if err != nil {
				t.Fatal(err)
			}
			if response.TypeUrl != ResultTypeURL || string(response.Value) != `{"ok":true}` {
				t.Fatalf("response=%+v", response)
			}
		})
	}
}

func TestCallRoundTrip(t *testing.T) {
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "shim.sock"))
	if err != nil {
		t.Fatal(err)
	}
	server, err := ttrpc.NewServer()
	if err != nil {
		t.Fatal(err)
	}
	controller := &fakeController{}
	Register(server, controller)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		server.Close()
	})
	go server.Serve(ctx, listener)

	conn, err := net.Dial("unix", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	client := ttrpc.NewClient(conn)
	t.Cleanup(func() { client.Close() })
	request := Request{ContainerID: "demo", Source: "main", Destination: "child", Paused: true}
	result, err := Call(ctx, client, "Fork", request)
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != `{"ok":true}` {
		t.Fatalf("result=%s", result)
	}
	if controller.operation != "Fork" || controller.request != request {
		t.Fatalf("controller call=%q %+v", controller.operation, controller.request)
	}
}

func TestServiceRejectsInvalidRequests(t *testing.T) {
	for _, test := range []struct {
		operation string
		request   Request
		want      string
	}{
		{"List", Request{}, "container_id"},
		{"Fork", Request{ContainerID: "demo"}, "destination"},
		{"Pause", Request{ContainerID: "demo", Source: "main"}, "accepts"},
	} {
		if _, err := invoke(t, test.operation, test.request); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s error=%v, want %q", test.operation, err, test.want)
		}
	}
}
