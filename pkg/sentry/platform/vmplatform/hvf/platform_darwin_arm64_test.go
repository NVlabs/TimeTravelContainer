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

//go:build darwin && arm64 && cgo

package hvf

import (
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
		t.Skipf("Hypervisor.framework unavailable: %v", err)
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
