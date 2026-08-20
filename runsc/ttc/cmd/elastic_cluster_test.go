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

import "testing"

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
