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

package images

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestContentReuse(t *testing.T) {
	before := map[string]int64{"sha256:old": 60}
	after := map[string]int64{"sha256:old": 60, "sha256:new": 40}
	if got, want := contentReuse(before, after, 100), (PullStats{DownloadedBytes: 40, ReusedBytes: 60}); got != want {
		t.Fatalf("contentReuse=%+v, want %+v", got, want)
	}
	if got := contentReuse(after, after, 100); got.DownloadedBytes != 0 || got.ReusedBytes != 100 {
		t.Fatalf("full reuse=%+v", got)
	}
	if got := contentReuse(nil, map[string]int64{"extra": 120}, 100); got.DownloadedBytes != 100 || got.ReusedBytes != 0 {
		t.Fatalf("capped download=%+v", got)
	}
}

func TestContainerLeaseID(t *testing.T) {
	containerID := strings.Repeat("a", 64)
	if got, want := ContainerLeaseID(containerID), "ttos-"+containerID; got != want {
		t.Fatalf("ContainerLeaseID=%q, want %q", got, want)
	}
}

func TestNormalizeReference(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: "ubuntu", want: "docker.io/library/ubuntu:latest"},
		{input: "example/app", want: "docker.io/example/app:latest"},
		{input: "ubuntu:24.04", want: "docker.io/library/ubuntu:24.04"},
		{input: "quay.io/example/app:v1", want: "quay.io/example/app:v1"},
		{input: "localhost:5000/app", want: "localhost:5000/app:latest"},
		{input: "busybox@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662", want: "docker.io/library/busybox@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662"},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			got, err := NormalizeReference(test.input)
			if err != nil {
				t.Fatalf("NormalizeReference(%q): %v", test.input, err)
			}
			if got != test.want {
				t.Errorf("NormalizeReference(%q)=%q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestNormalizeReferenceRejectsInvalid(t *testing.T) {
	for _, input := range []string{"", "UPPERCASE", "bad reference"} {
		if got, err := NormalizeReference(input); err == nil {
			t.Errorf("NormalizeReference(%q)=%q, want error", input, got)
		}
	}
}

func TestResolveNameDigestAndAliases(t *testing.T) {
	digestA := "sha256:" + strings.Repeat("a", 64)
	digestB := "sha256:" + strings.Repeat("b", 64)
	infos := []Info{
		{Reference: "docker.io/library/ubuntu:latest", Digest: digestA},
		{Reference: "docker.io/library/ubuntu:stable", Digest: digestA},
		{Reference: "quay.io/example/tool:v1", Digest: digestB},
	}
	matches, err := Resolve(infos, "ubuntu")
	if err != nil {
		t.Fatalf("Resolve(ubuntu): %v", err)
	}
	if len(matches) != 1 || matches[0].Reference != "docker.io/library/ubuntu:latest" {
		t.Errorf("Resolve(ubuntu)=%+v", matches)
	}
	matches, err = Resolve(infos, strings.Repeat("a", 12))
	if err != nil {
		t.Fatalf("Resolve(short digest): %v", err)
	}
	wantAliases := []Info{infos[0], infos[1]}
	if !reflect.DeepEqual(matches, wantAliases) {
		t.Errorf("Resolve(short digest)=%+v, want %+v", matches, wantAliases)
	}
	matches, err = Resolve(infos, "sha256:"+strings.Repeat("b", 64))
	if err != nil {
		t.Fatalf("Resolve(full digest): %v", err)
	}
	if len(matches) != 1 || matches[0].Reference != infos[2].Reference {
		t.Errorf("Resolve(full digest)=%+v", matches)
	}
}

func TestResolveRejectsUnknownAndAmbiguousPrefixes(t *testing.T) {
	infos := []Info{
		{Reference: "example/a:v1", Digest: "sha256:abcd" + strings.Repeat("1", 60)},
		{Reference: "example/b:v1", Digest: "sha256:abcd" + strings.Repeat("2", 60)},
	}
	for _, identifier := range []string{"abcd", "missing", "1234"} {
		if matches, err := Resolve(infos, identifier); err == nil {
			t.Errorf("Resolve(%q)=%+v, want error", identifier, matches)
		}
	}
}

func TestImageCreatedAtPrefersOCIConfiguration(t *testing.T) {
	stored := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)
	configured := stored.Add(-24 * time.Hour)
	if got := imageCreatedAt(stored, &configured); !got.Equal(configured) {
		t.Errorf("imageCreatedAt with config=%v, want %v", got, configured)
	}
	if got := imageCreatedAt(stored, nil); !got.Equal(stored) {
		t.Errorf("imageCreatedAt without config=%v, want %v", got, stored)
	}
}

func TestDockerCredentialsInlineAndDockerHubAlias(t *testing.T) {
	auth := base64.StdEncoding.EncodeToString([]byte("test-user:test-secret"))
	config := dockerConfigFile{Auths: map[string]dockerAuthConfig{
		"https://index.docker.io/v1/": {Auth: auth},
	}}
	username, secret, err := config.credentials("registry-1.docker.io")
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	if username != "test-user" || secret != "test-secret" {
		t.Errorf("credentials=%q:%q, want test-user:test-secret", username, secret)
	}
}

func TestDockerCredentialHelper(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "docker-credential-fixture")
	script := "#!/bin/sh\nread server\nprintf \"{\\\"Username\\\":\\\"helper-user\\\",\\\"Secret\\\":\\\"helper-secret\\\"}\n\"\n"
	if err := os.WriteFile(helper, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	config := dockerConfigFile{CredHelpers: map[string]string{"registry.example": "fixture"}}
	username, secret, err := config.credentials("registry.example")
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	if username != "helper-user" || secret != "helper-secret" {
		t.Errorf("credentials=%q:%q, want helper-user:helper-secret", username, secret)
	}
}

func TestDockerCredentialCallbackUsesDockerConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCKER_CONFIG", dir)
	auth := base64.StdEncoding.EncodeToString([]byte("configured:secret"))
	data := []byte(fmt.Sprintf(`{"auths":{"registry.example":{"auth":%q}}}`, auth))
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	callback, configured, err := dockerCredentialCallback()
	if err != nil {
		t.Fatalf("dockerCredentialCallback: %v", err)
	}
	if !configured {
		t.Fatal("Docker configuration was not detected")
	}
	username, secret, err := callback("registry.example")
	if err != nil || username != "configured" || secret != "secret" {
		t.Fatalf("callback credentials=%q:%q err=%v", username, secret, err)
	}
}
