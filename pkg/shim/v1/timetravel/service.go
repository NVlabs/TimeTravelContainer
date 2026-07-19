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

// Package timetravel defines an additive containerd-shim TTRPC side channel.
// It intentionally does not extend or reinterpret containerd's task-v2 API.
package timetravel

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/containerd/ttrpc"
	"google.golang.org/protobuf/types/known/anypb"
)

const (
	// ServiceName is versioned independently from containerd task-v2.
	ServiceName = "gvisor.timetravel.v1.Branches"
	// RequestTypeURL identifies the JSON request carried by protobuf Any.
	RequestTypeURL = "type.googleapis.com/gvisor.timetravel.v1.Request"
	// ResultTypeURL identifies the JSON result carried by protobuf Any.
	ResultTypeURL = "type.googleapis.com/gvisor.timetravel.v1.JSONResult"
)

// Request selects one sandbox and branch operation. Branch defaults to main
// for pause/resume/destroy; Source defaults to main for fork.
type Request struct {
	ContainerID string `json:"container_id"`
	Branch      string `json:"branch,omitempty"`
	Source      string `json:"source,omitempty"`
	Destination string `json:"destination,omitempty"`
	Paused      bool   `json:"paused,omitempty"`
}

// Controller performs an already-validated branch operation and returns the
// JSON emitted by runsc timetravel. Empty successful output is normalized to {}.
type Controller interface {
	ControlTimeTravel(context.Context, string, *Request) ([]byte, error)
}

var operations = []string{"List", "Fork", "Pause", "Resume", "Destroy"}

func validate(operation string, req *Request) error {
	if req.ContainerID == "" {
		return fmt.Errorf("container_id is required")
	}
	switch operation {
	case "List":
		if req.Branch != "" || req.Source != "" || req.Destination != "" || req.Paused {
			return fmt.Errorf("list accepts only container_id")
		}
	case "Pause", "Resume", "Destroy":
		if req.Source != "" || req.Destination != "" || req.Paused {
			return fmt.Errorf("%s accepts container_id and optional branch", operation)
		}
	case "Fork":
		if req.Destination == "" {
			return fmt.Errorf("destination is required for fork")
		}
		if req.Branch != "" {
			return fmt.Errorf("fork uses source instead of branch")
		}
	default:
		return fmt.Errorf("unsupported operation %q", operation)
	}
	return nil
}

func method(controller Controller, operation string) ttrpc.Method {
	return func(ctx context.Context, unmarshal func(interface{}) error) (interface{}, error) {
		envelope := &anypb.Any{}
		if err := unmarshal(envelope); err != nil {
			return nil, err
		}
		if envelope.TypeUrl != "" && envelope.TypeUrl != RequestTypeURL {
			return nil, fmt.Errorf("unexpected request type %q", envelope.TypeUrl)
		}
		var req Request
		if err := json.Unmarshal(envelope.Value, &req); err != nil {
			return nil, fmt.Errorf("decoding request: %w", err)
		}
		if err := validate(operation, &req); err != nil {
			return nil, err
		}
		result, err := controller.ControlTimeTravel(ctx, operation, &req)
		if err != nil {
			return nil, err
		}
		if len(result) == 0 {
			result = []byte("{}")
		}
		if !json.Valid(result) {
			return nil, fmt.Errorf("runsc returned invalid JSON")
		}
		return &anypb.Any{TypeUrl: ResultTypeURL, Value: result}, nil
	}
}

// Call invokes one branch operation and returns a private copy of its JSON result.
func Call(ctx context.Context, client *ttrpc.Client, operation string, req Request) (json.RawMessage, error) {
	if err := validate(operation, &req); err != nil {
		return nil, err
	}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	request := &anypb.Any{TypeUrl: RequestTypeURL, Value: data}
	response := &anypb.Any{}
	if err := client.Call(ctx, ServiceName, operation, request, response); err != nil {
		return nil, err
	}
	if response.TypeUrl != ResultTypeURL {
		return nil, fmt.Errorf("unexpected result type %q", response.TypeUrl)
	}
	if !json.Valid(response.Value) {
		return nil, fmt.Errorf("invalid JSON result")
	}
	return append(json.RawMessage(nil), response.Value...), nil
}

// ServiceDesc returns the additive TTRPC service descriptor.
func ServiceDesc(controller Controller) *ttrpc.ServiceDesc {
	methods := make(map[string]ttrpc.Method, len(operations))
	for _, operation := range operations {
		methods[operation] = method(controller, operation)
	}
	return &ttrpc.ServiceDesc{Methods: methods}
}

// Register adds the branch side channel without changing TaskService.
func Register(server *ttrpc.Server, controller Controller) {
	server.RegisterService(ServiceName, ServiceDesc(controller))
}
