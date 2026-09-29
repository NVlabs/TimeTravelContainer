// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcremote

import (
	"fmt"
	"time"
)

// SimulationConfig configures the Phase 0 analytical pull simulator. It is not
// a replacement for network benchmarks; it makes policy sweeps deterministic
// and cheaply rejects obviously poor combinations.
type SimulationConfig struct {
	Pages                   uint64
	Policy                  Policy
	RoundTripLatency        time.Duration
	BandwidthBytesPerSecond float64
}

// SimulationResult summarizes one ordered target access trace.
type SimulationResult struct {
	Accesses            uint64
	Faults              uint64
	Requests            uint64
	TransferredPages    uint64
	TransferredBytes    uint64
	DemandedPages       uint64
	UsefulPrefetchPages uint64
	UnusedPages         uint64
	SerialStall         time.Duration
}

// PrefetchUsefulness returns useful prefetched pages divided by all pages that
// were transferred before demand. A value of one means no prefetched page was
// wasted. A value of zero is returned when no pages were prefetched.
func (r SimulationResult) PrefetchUsefulness() float64 {
	prefetched := r.TransferredPages - r.Faults
	if prefetched == 0 {
		return 0
	}
	return float64(r.UsefulPrefetchPages) / float64(prefetched)
}

// SimulatePulls simulates ordered, dependent target accesses. Each missing
// access stalls for one RTT plus range serialization. Concurrency and overlap
// are intentionally left to the network harness in later phases.
func SimulatePulls(cfg SimulationConfig, accesses []PageIndex) (SimulationResult, error) {
	if cfg.Pages == 0 {
		return SimulationResult{}, fmt.Errorf("simulation must contain at least one page")
	}
	if err := cfg.Policy.Validate(); err != nil {
		return SimulationResult{}, err
	}
	if cfg.RoundTripLatency < 0 {
		return SimulationResult{}, fmt.Errorf("round-trip latency must not be negative")
	}
	if cfg.BandwidthBytesPerSecond <= 0 {
		return SimulationResult{}, fmt.Errorf("bandwidth must be positive")
	}

	resident := make([]bool, cfg.Pages)
	prefetched := make([]bool, cfg.Pages)
	demanded := make([]bool, cfg.Pages)
	result := SimulationResult{Accesses: uint64(len(accesses))}

	for _, page := range accesses {
		if uint64(page) >= cfg.Pages {
			return SimulationResult{}, fmt.Errorf("access page %d out of range [0, %d)", page, cfg.Pages)
		}
		if !demanded[page] {
			demanded[page] = true
			result.DemandedPages++
			if prefetched[page] {
				result.UsefulPrefetchPages++
			}
		}
		if resident[page] {
			continue
		}

		result.Faults++
		result.Requests++
		start, end := simulationRange(cfg, page)
		var pages uint64
		for i := start; i < end; i++ {
			if resident[i] {
				continue
			}
			resident[i] = true
			pages++
			if PageIndex(i) != page && !demanded[i] {
				prefetched[i] = true
			}
		}
		bytes := pages * cfg.Policy.PageSize
		result.TransferredPages += pages
		result.TransferredBytes += bytes
		serializationSeconds := float64(bytes) / cfg.BandwidthBytesPerSecond
		result.SerialStall += cfg.RoundTripLatency + time.Duration(serializationSeconds*float64(time.Second))
	}

	for i, isResident := range resident {
		if isResident && !demanded[i] {
			result.UnusedPages++
		}
	}
	return result, nil
}

func simulationRange(cfg SimulationConfig, page PageIndex) (uint64, uint64) {
	if cfg.Policy.Prefetch == PrefetchNone {
		return uint64(page), uint64(page) + 1
	}
	pages := cfg.Policy.TransferPages()
	start := uint64(page)
	if cfg.Policy.Prefetch == PrefetchFixed {
		// Center a fixed window around the fault while preserving range
		// alignment where possible.
		half := pages / 2
		if start > half {
			start -= half
		} else {
			start = 0
		}
	} else {
		// Sequential and the initial adaptive policy align forward ranges.
		start = start / pages * pages
	}
	end := start + pages
	if end > cfg.Pages {
		end = cfg.Pages
	}
	return start, end
}
