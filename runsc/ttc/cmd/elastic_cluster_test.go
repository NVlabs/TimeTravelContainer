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

package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
)

func TestClusterAdvertiseResolution(t *testing.T) {
	base, name, err := parseClusterAdvertise("https://node-a.example:0,node-a.example")
	if err != nil {
		t.Fatal(err)
	}
	if name != "node-a.example" {
		t.Fatalf("server name=%q", name)
	}
	resolved, err := resolveClusterAdvertise(base, "[::]:17443")
	if err != nil {
		t.Fatal(err)
	}
	if resolved != "https://node-a.example:17443" {
		t.Fatalf("resolved URL=%q", resolved)
	}
	page, err := advertisedPageAddress(base)
	if err != nil {
		t.Fatal(err)
	}
	if page != "node-a.example:0" {
		t.Fatalf("page address=%q", page)
	}
}

func TestDaemonCommandOwner(t *testing.T) {
	root := t.TempDir()
	containerDir := filepath.Join(root, "containers", "web")
	if err := os.MkdirAll(containerDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(containerDir, "bundle"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(containerDir, "bundle", "config.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(ttcbundle.ContainerInfo{ID: "web", Name: "web", NodeID: "node-b", BundleDir: filepath.Join(containerDir, "bundle")})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(containerDir, "container.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if info, err := (ttcbundle.Store{Root: root}).LoadContainer("web"); err != nil || info.NodeID != "node-b" {
		t.Fatalf("loading routing fixture=(%+v, %v)", info, err)
	}
	for _, args := range [][]string{
		{"exec", "-it", "web@main", "bash"},
		{"cp", "web@main:/tmp/result", "/tmp/result"},
		{"fork", "web@main", "child"},
	} {
		owner, err := daemonCommandOwner(root, args)
		if err != nil || owner != "node-b" {
			t.Fatalf("daemonCommandOwner(%v)=(%q, %v)", args, owner, err)
		}
	}
	if owner, err := daemonCommandOwner(root, []string{"images"}); err != nil || owner != "" {
		t.Fatalf("image command owner=(%q, %v)", owner, err)
	}
}

func TestClusterAdvertiseRejectsUnreachableOrInsecureURLs(t *testing.T) {
	for _, value := range []string{
		"http://node-a.example:7443,node-a.example",
		"https://0.0.0.0:7443,node-a.example",
		"https://node-a.example,node-a.example",
		"https://node-a.example:7443/path,node-a.example",
	} {
		if _, _, err := parseClusterAdvertise(value); err == nil {
			t.Errorf("parseClusterAdvertise(%q) succeeded", value)
		}
	}
}
