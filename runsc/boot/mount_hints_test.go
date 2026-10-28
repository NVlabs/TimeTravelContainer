// Copyright 2022 The gVisor Authors.
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

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package boot

import (
	"strings"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/runsc/boot/sentryapi"
)

func TestHintsCheckCompatible(t *testing.T) {
	for _, tc := range []struct {
		name        string
		masterOpts  []string
		replicaOpts []string
		err         string
	}{
		{
			name: "empty",
		},
		{
			name:        "same",
			masterOpts:  []string{"ro", "noatime", "noexec", "nodev"},
			replicaOpts: []string{"ro", "noatime", "noexec", "nodev"},
		},
		{
			name:        "compatible",
			masterOpts:  []string{"rw", "atime", "exec", "dev"},
			replicaOpts: []string{"ro", "noatime", "noexec", "nodev"},
		},
		{
			name:        "unsupported",
			masterOpts:  []string{"nofoo"},
			replicaOpts: []string{"foo"},
		},
		{
			name:        "incompatible-ro",
			masterOpts:  []string{"ro"},
			replicaOpts: []string{"rw"},
			err:         "read-write",
		},
		{
			name:        "incompatible-atime",
			masterOpts:  []string{"noatime"},
			replicaOpts: []string{"atime"},
			err:         "noatime",
		},
		{
			name:        "incompatible-exec",
			masterOpts:  []string{"noexec"},
			replicaOpts: []string{"exec"},
			err:         "noexec",
		},
		{
			name:        "incompatible-nodev",
			masterOpts:  []string{"nodev"},
			replicaOpts: []string{"dev"},
			err:         "nodev",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			master := sentryapi.MountHint{Mount: specs.Mount{Options: tc.masterOpts}}
			replica := specs.Mount{Options: tc.replicaOpts}
			if err := checkCompatible(&master, &replica); err != nil {
				if !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("wrong error, want: %q, got: %q", tc.err, err)
				}
			} else {
				if len(tc.err) > 0 {
					t.Fatalf("error %q expected", tc.err)
				}
			}
		})
	}
}
