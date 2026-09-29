// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"reflect"
	"testing"
)

func TestRuntimeControlArgs(t *testing.T) {
	for _, test := range []struct {
		name    string
		backend runtimeBackend
		want    []string
	}{
		{
			name:    "isolated",
			backend: runtimeBackend{root: "/runtime"},
			want:    []string{"--root", "/runtime"},
		},
		{
			name:    "trusted host",
			backend: runtimeBackend{root: "/runtime", unsafeNoHostIsolation: true},
			want:    []string{"--root", "/runtime", "--rootless=false", "--unsafe-no-host-isolation=true"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.backend.runtimeControlArgs(); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("runtimeControlArgs() = %q, want %q", got, test.want)
			}
		})
	}
}
