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

package protocol

import (
	"strings"
	"testing"
)

func TestDaemonRoot(t *testing.T) {
	for _, test := range []struct {
		name string
		env  map[string]string
		want string
		err  string
	}{
		{name: "standalone", env: map[string]string{"TTC_ROOT": "/data/ttc/"}, want: "/data/ttc"},
		{name: "cluster node", env: map[string]string{"TTC_ROOT": "/shared/ttc", "TTC_LOCAL_ROOT": " /local/ttc "}, want: "/local/ttc"},
		{name: "unset", env: map[string]string{}, err: "TTC_ROOT is not set"},
		{name: "relative", env: map[string]string{"TTC_ROOT": "ttc"}, err: "not an absolute path"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := DaemonRoot(func(name string) string { return test.env[name] })
			if test.err != "" {
				if err == nil || !strings.Contains(err.Error(), test.err) {
					t.Fatalf("DaemonRoot() = %q, %v; want an error containing %q", got, err, test.err)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("DaemonRoot() = %q, %v; want %q", got, err, test.want)
			}
			if socket := DaemonSocket(got); socket != test.want+"/ttcd.sock" {
				t.Errorf("DaemonSocket(%q) = %q", got, socket)
			}
		})
	}
}
