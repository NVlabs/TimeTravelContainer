// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin && arm64 && cgo

package hvf

import (
	"os"
	"testing"

	"gvisor.dev/gvisor/pkg/sentry/platform"
)

func TestHVFApplicationVCPUs(t *testing.T) {
	for _, test := range []struct {
		cores int
		want  int
	}{
		{cores: 1, want: 3},
		{cores: 2, want: 6},
		{cores: 20, want: 60},
		{cores: 21, want: 63},
		{cores: 64, want: 63},
	} {
		if got := hvfApplicationVCPUs(test.cores); got != test.want {
			t.Errorf("hvfApplicationVCPUs(%d) = %d, want %d", test.cores, got, test.want)
		}
	}
}

func TestPlatformRegistrationConstructsSentryAtEL1Engine(t *testing.T) {
	constructor, err := platform.Lookup("hvf")
	if err != nil {
		t.Fatal(err)
	}
	p, err := constructor.New(platform.Options{ApplicationCores: 2})
	if err != nil {
		skipWithoutHypervisor(t, err)
	}
	if got := p.SeccompInfo().ConfigKey(); got != "hvf" {
		t.Errorf("platform config key = %q, want hvf", got)
	}
	if got := p.ConcurrencyCount(); got != 6 {
		t.Errorf("platform concurrency = %d, want 6", got)
	}
	closer, ok := p.(interface{ Close() error })
	if !ok {
		t.Fatal("hvf platform does not expose execution-cache cleanup")
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("closing hvf platform: %v", err)
	}
}

// skipWithoutHypervisor skips the test because Hypervisor.framework is
// unavailable, as it is to an unsigned test binary, unless TTC_REQUIRE_HVF is
// set, as it is when the binary has been signed to use it.
func skipWithoutHypervisor(t *testing.T, err error) {
	t.Helper()
	if os.Getenv("TTC_REQUIRE_HVF") != "" {
		t.Fatalf("Hypervisor.framework unavailable: %v", err)
	}
	t.Skipf("Hypervisor.framework unavailable: %v", err)
}
