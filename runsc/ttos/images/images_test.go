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

import "testing"

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
