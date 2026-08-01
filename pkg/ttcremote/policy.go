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
	"fmt"
	"time"
)

// PrefetchPolicy controls which neighboring pages accompany a faulted page.
type PrefetchPolicy string

const (
	PrefetchNone       PrefetchPolicy = "none"
	PrefetchSequential PrefetchPolicy = "sequential"
	PrefetchFixed      PrefetchPolicy = "fixed-window"
	PrefetchAdaptive   PrefetchPolicy = "adaptive"
)

// PushPolicy controls proactive source-to-target transfer.
type PushPolicy string

const (
	PushDemandOnly     PushPolicy = "demand-only"
	PushDirtyOrphan    PushPolicy = "dirty-orphan"
	PushMemoryPressure PushPolicy = "memory-pressure"
	PushEagerDrain     PushPolicy = "eager-drain"
)

// Policy configures remote-memory transfer. PageSize is the correctness unit;
// TransferBytes is only the preferred network range size.
type Policy struct {
	PageSize            uint64         `json:"page_size"`
	TransferBytes       uint64         `json:"transfer_bytes"`
	Prefetch            PrefetchPolicy `json:"prefetch"`
	Push                PushPolicy     `json:"push"`
	MaxOutstandingBytes uint64         `json:"max_outstanding_bytes"`
	MaxConcurrency      int            `json:"max_concurrency"`
	AckInterval         time.Duration  `json:"ack_interval"`
	AckBatchPages       int            `json:"ack_batch_pages"`
	Adaptive            bool           `json:"adaptive"`
}

// DefaultPolicy returns the initial experimental policy from the proposal.
func DefaultPolicy() Policy {
	return Policy{
		PageSize:            4 << 10,
		TransferBytes:       64 << 10,
		Prefetch:            PrefetchSequential,
		Push:                PushDirtyOrphan,
		MaxOutstandingBytes: 64 << 20,
		MaxConcurrency:      64,
		AckInterval:         50 * time.Microsecond,
		AckBatchPages:       256,
		Adaptive:            false,
	}
}

// Validate validates p without selecting a host-dependent default.
func (p Policy) Validate() error {
	if p.PageSize == 0 || p.PageSize&(p.PageSize-1) != 0 {
		return fmt.Errorf("page size %d is not a nonzero power of two", p.PageSize)
	}
	if p.TransferBytes < p.PageSize || p.TransferBytes%p.PageSize != 0 {
		return fmt.Errorf("transfer size %d is not a positive multiple of page size %d", p.TransferBytes, p.PageSize)
	}
	if p.TransferBytes > 2<<20 {
		return fmt.Errorf("transfer size %d exceeds the initial 2 MiB protocol limit", p.TransferBytes)
	}
	switch p.Prefetch {
	case PrefetchNone, PrefetchSequential, PrefetchFixed, PrefetchAdaptive:
	default:
		return fmt.Errorf("unknown prefetch policy %q", p.Prefetch)
	}
	switch p.Push {
	case PushDemandOnly, PushDirtyOrphan, PushMemoryPressure, PushEagerDrain:
	default:
		return fmt.Errorf("unknown push policy %q", p.Push)
	}
	if p.MaxOutstandingBytes < p.TransferBytes {
		return fmt.Errorf("maximum outstanding bytes %d is smaller than one transfer of %d bytes", p.MaxOutstandingBytes, p.TransferBytes)
	}
	if p.MaxConcurrency <= 0 {
		return fmt.Errorf("maximum concurrency must be positive, got %d", p.MaxConcurrency)
	}
	if p.AckInterval < 0 {
		return fmt.Errorf("ACK interval must not be negative, got %s", p.AckInterval)
	}
	if p.AckBatchPages <= 0 {
		return fmt.Errorf("ACK batch pages must be positive, got %d", p.AckBatchPages)
	}
	return nil
}

// TransferPages returns the preferred number of base pages in one transfer.
func (p Policy) TransferPages() uint64 {
	return p.TransferBytes / p.PageSize
}
