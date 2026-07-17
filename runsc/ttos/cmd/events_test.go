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

package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCommandEventsClassifyManagerOperations(t *testing.T) {
	for _, test := range []struct {
		args        []string
		wantType    string
		wantAction  string
		wantSubject string
	}{
		{[]string{"fork", "--paused", "demo@main", "demo@child"}, "branch", "fork", "demo@child"},
		{[]string{"fork", "--check", "--json", "demo@child"}, "eligibility", "check", "demo@child"},
		{[]string{"exec", "-dit", "--env", "SECRET=value", "demo@child", "sh"}, "exec", "create", "demo@child"},
		{[]string{"exec", "kill", "--signal", "TERM", "demo@child:exec-one"}, "exec", "kill", "demo@child:exec-one"},
		{[]string{"container", "unpause", "demo@child"}, "branch", "resume", "demo@child"},
		{[]string{"run", "--name", "web", "nginx"}, "container", "run", "web"},
	} {
		events := commandEvents(test.args, 0)
		if len(events) != 1 {
			t.Fatalf("commandEvents(%q)=%+v", test.args, events)
		}
		event := events[0]
		if event.Type != test.wantType || event.Action != test.wantAction || event.Subject != test.wantSubject {
			t.Errorf("commandEvents(%q)=%+v, want type=%q action=%q subject=%q", test.args, event, test.wantType, test.wantAction, test.wantSubject)
		}
		data, _ := json.Marshal(event)
		if strings.Contains(string(data), "SECRET") {
			t.Errorf("event leaked command environment: %s", data)
		}
	}
}

func TestDaemonEventsPersistFilterAndTimeBound(t *testing.T) {
	root := t.TempDir()
	server := &daemonServer{}
	firstTime := time.Now().Add(-time.Minute).UTC()
	server.recordEvent(root, DaemonEvent{Type: "container", Action: "start", Subject: "demo", Status: 0})
	server.recordEvent(root, DaemonEvent{Type: "branch", Action: "fork", Subject: "demo@child", Parent: "demo@main", Status: 0})

	var output bytes.Buffer
	if err := server.streamEvents(root, []string{"--json", "--filter", "type=branch", "--since", firstTime.Format(time.RFC3339Nano)}, &output); err != nil {
		t.Fatalf("streamEvents: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("filtered event lines=%q", lines)
	}
	var event DaemonEvent
	if err := json.Unmarshal([]byte(lines[0]), &event); err != nil {
		t.Fatal(err)
	}
	if event.Version != daemonEventSchemaVersion || event.Type != "branch" || event.Action != "fork" || event.Subject != "demo@child" || event.ID == "" {
		t.Fatalf("persisted event=%+v", event)
	}

	output.Reset()
	if err := server.streamEvents(root, []string{"--filter=action=start"}, &output); err != nil {
		t.Fatalf("streamEvents text: %v", err)
	}
	if got := output.String(); !strings.Contains(got, " container start demo status=0 id=") {
		t.Fatalf("text event output=%q", got)
	}
}

func TestDaemonEventsFollowNewAppend(t *testing.T) {
	root := t.TempDir()
	server := &daemonServer{}
	until := time.Now().Add(500 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	var output bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- server.streamEvents(root, []string{"--json", "--follow", "--until", until}, &output)
	}()
	time.Sleep(25 * time.Millisecond)
	server.recordEvent(root, DaemonEvent{Type: "exec", Action: "create", Subject: "demo@child", Status: 0})
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("streamEvents follow: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("event follower did not honor --until")
	}
	if got := output.String(); !strings.Contains(got, "\"type\":\"exec\"") || !strings.Contains(got, "\"subject\":\"demo@child\"") {
		t.Fatalf("followed events=%q", got)
	}
}

func TestCommandEventsExposeExitAndDestroy(t *testing.T) {
	stopEvents := commandEvents([]string{"stop", "demo@child"}, 0)
	if len(stopEvents) != 2 || stopEvents[0].Action != "stop" || stopEvents[1].Action != "exit" {
		t.Fatalf("stop events=%+v", stopEvents)
	}
	for _, test := range []struct {
		args       []string
		wantType   string
		wantAction string
	}{
		{[]string{"rm", "demo"}, "container", "destroy"},
		{[]string{"image", "pull", "alpine"}, "image", "pull"},
		{[]string{"branch", "rm", "demo@child"}, "branch", "destroy"},
		{[]string{"exec", "wait", "demo@child:exec-one"}, "exec", "exit"},
	} {
		events := commandEvents(test.args, 0)
		if len(events) != 1 || events[0].Type != test.wantType || events[0].Action != test.wantAction {
			t.Errorf("commandEvents(%q)=%+v, want %s/%s", test.args, events, test.wantType, test.wantAction)
		}
	}
}
