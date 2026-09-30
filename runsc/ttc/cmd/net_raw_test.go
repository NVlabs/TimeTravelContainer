// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import "testing"

func TestRawSocketsRequested(t *testing.T) {
	for _, tc := range []struct {
		add, drop []string
		want      bool
	}{
		{nil, nil, false},
		{[]string{"NET_ADMIN"}, nil, false},
		{[]string{"NET_RAW"}, nil, true},
		{[]string{"ALL"}, nil, true},
		{[]string{"ALL"}, []string{"NET_RAW"}, false},
		{[]string{"NET_RAW"}, []string{"ALL"}, false},
	} {
		if got := rawSocketsRequested(tc.add, tc.drop); got != tc.want {
			t.Errorf("rawSocketsRequested(%v, %v) = %v, want %v", tc.add, tc.drop, got, tc.want)
		}
	}
}
