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

package ttcremote

import (
	"testing"
	"time"
)

func TestPolicyValidate(t *testing.T) {
	p := DefaultPolicy()
	if err := p.Validate(); err != nil {
		t.Fatalf("DefaultPolicy.Validate(): %v", err)
	}
	if p.PageSize != 4<<10 || p.TransferBytes != 1<<20 || p.Prefetch != PrefetchNone ||
		p.Push != PushDemandOnly || p.Adaptive {
		t.Fatalf("DefaultPolicy() = %+v, want fixed 1 MiB logical-chunk demand policy", p)
	}
	for _, test := range []struct {
		name   string
		mutate func(*Policy)
	}{
		{"non-power-of-two page", func(p *Policy) { p.PageSize = 6000 }},
		{"unaligned transfer", func(p *Policy) { p.TransferBytes++ }},
		{"oversized transfer", func(p *Policy) { p.TransferBytes = 4 << 20 }},
		{"unknown prefetch", func(p *Policy) { p.Prefetch = "guess" }},
		{"unknown push", func(p *Policy) { p.Push = "hope" }},
		{"undersized window", func(p *Policy) { p.MaxOutstandingBytes = p.TransferBytes / 2 }},
		{"zero concurrency", func(p *Policy) { p.MaxConcurrency = 0 }},
		{"negative ACK interval", func(p *Policy) { p.AckInterval = -time.Second }},
		{"zero ACK batch", func(p *Policy) { p.AckBatchPages = 0 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := DefaultPolicy()
			test.mutate(&p)
			if err := p.Validate(); err == nil {
				t.Fatalf("Policy.Validate() succeeded: %+v", p)
			}
		})
	}
}

func TestSimulateSequentialPrefetch(t *testing.T) {
	p := DefaultPolicy()
	p.TransferBytes = 16 << 10 // Four pages.
	p.Prefetch = PrefetchAdaptive
	result, err := SimulatePulls(SimulationConfig{
		Pages:                   8,
		Policy:                  p,
		RoundTripLatency:        time.Millisecond,
		BandwidthBytesPerSecond: 1 << 30,
	}, []PageIndex{0, 1, 2, 3, 4, 5, 6, 7})
	if err != nil {
		t.Fatal(err)
	}
	if result.Faults != 2 || result.Requests != 2 || result.TransferredPages != 8 {
		t.Fatalf("unexpected transfer counts: %+v", result)
	}
	if result.UsefulPrefetchPages != 6 || result.UnusedPages != 0 || result.PrefetchUsefulness() != 1 {
		t.Fatalf("unexpected prefetch accounting: %+v", result)
	}
}

func TestSimulateSparseAccess(t *testing.T) {
	p := DefaultPolicy()
	p.TransferBytes = 64 << 10 // Sixteen pages.
	p.Prefetch = PrefetchAdaptive
	result, err := SimulatePulls(SimulationConfig{
		Pages:                   64,
		Policy:                  p,
		RoundTripLatency:        10 * time.Millisecond,
		BandwidthBytesPerSecond: 100 << 20,
	}, []PageIndex{0, 32})
	if err != nil {
		t.Fatal(err)
	}
	if result.Faults != 2 || result.TransferredPages != 32 || result.UnusedPages != 30 {
		t.Fatalf("unexpected sparse result: %+v", result)
	}
	if got := result.PrefetchUsefulness(); got != 0 {
		t.Fatalf("PrefetchUsefulness()=%f, want 0", got)
	}
}

func TestSimulateNoPrefetch(t *testing.T) {
	p := DefaultPolicy()
	p.Prefetch = PrefetchNone
	result, err := SimulatePulls(SimulationConfig{
		Pages:                   8,
		Policy:                  p,
		RoundTripLatency:        time.Millisecond,
		BandwidthBytesPerSecond: 1 << 30,
	}, []PageIndex{1, 1, 7})
	if err != nil {
		t.Fatal(err)
	}
	if result.Faults != 2 || result.TransferredPages != 2 || result.DemandedPages != 2 || result.UnusedPages != 0 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func BenchmarkSimulationSequential(b *testing.B) {
	p := DefaultPolicy()
	accesses := make([]PageIndex, 1<<16)
	for i := range accesses {
		accesses[i] = PageIndex(i)
	}
	cfg := SimulationConfig{
		Pages:                   uint64(len(accesses)),
		Policy:                  p,
		RoundTripLatency:        time.Millisecond,
		BandwidthBytesPerSecond: 10e9,
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := SimulatePulls(cfg, accesses); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSimulationRandom(b *testing.B) {
	p := DefaultPolicy()
	const pages = 1 << 16
	accesses := make([]PageIndex, pages)
	var state uint64 = 1
	for i := range accesses {
		// Deterministic xorshift avoids measuring rand's locking or setup.
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		accesses[i] = PageIndex(state % pages)
	}
	cfg := SimulationConfig{
		Pages:                   pages,
		Policy:                  p,
		RoundTripLatency:        time.Millisecond,
		BandwidthBytesPerSecond: 10e9,
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := SimulatePulls(cfg, accesses); err != nil {
			b.Fatal(err)
		}
	}
}
