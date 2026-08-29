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

import "gvisor.dev/gvisor/pkg/metric"

var (
	hvfRendezvousCount = metric.MustCreateNewUint64Metric(
		"/platform/hvf/rendezvous/count",
		metric.Uint64Metadata{Cumulative: true, Description: "Number of VM-wide HVF guest execution rendezvous."})
	hvfRuntimeRendezvousRequests = metric.MustCreateNewUint64Metric(
		"/platform/hvf/rendezvous/runtime_requests",
		metric.Uint64Metadata{Cumulative: true, Description: "Number of Go runtime VM mutations that requested an HVF guest rendezvous."})
	hvfRendezvousDrainTotalNS = metric.MustCreateNewUint64Metric(
		"/platform/hvf/rendezvous/drain_ns",
		metric.Uint64Metadata{Cumulative: true, Description: "Total time spent draining guest vCPUs for HVF rendezvous, in nanoseconds."})
	hvfRendezvousDrainMaxNS = metric.MustCreateNewUint64Metric(
		"/platform/hvf/rendezvous/drain_max_ns",
		metric.Uint64Metadata{Cumulative: true, Description: "Maximum observed guest-vCPU drain time for one HVF rendezvous, in nanoseconds."})
	hvfRendezvousPauseTotalNS = metric.MustCreateNewUint64Metric(
		"/platform/hvf/rendezvous/pause_ns",
		metric.Uint64Metadata{Cumulative: true, Description: "Total duration of VM-wide HVF guest execution pauses, in nanoseconds."})
	hvfRendezvousPauseMaxNS = metric.MustCreateNewUint64Metric(
		"/platform/hvf/rendezvous/pause_max_ns",
		metric.Uint64Metadata{Cumulative: true, Description: "Maximum observed duration of one VM-wide HVF guest execution pause, in nanoseconds."})
	hvfRendezvousVCPUExitCalls = metric.MustCreateNewUint64Metric(
		"/platform/hvf/rendezvous/vcpu_exit_calls",
		metric.Uint64Metadata{Cumulative: true, Description: "Number of Hypervisor.framework multi-vCPU exit requests issued by rendezvous."})
	hvfRendezvousVCPUExitTargets = metric.MustCreateNewUint64Metric(
		"/platform/hvf/rendezvous/vcpu_exit_targets",
		metric.Uint64Metadata{Cumulative: true, Description: "Total vCPU targets included in Hypervisor.framework rendezvous exit requests."})
	hvfRendezvousFailures = metric.MustCreateNewUint64Metric(
		"/platform/hvf/rendezvous/failures",
		metric.Uint64Metadata{Cumulative: true, Description: "Number of HVF guest rendezvous that failed or timed out."})
	hvfRuntimeVMHookCalls = metric.MustCreateNewUint64Metric(
		"/platform/hvf/runtime_vm/calls",
		metric.Uint64Metadata{Cumulative: true, Description: "Number of Darwin Go runtime VM notifier callbacks."})
	hvfRuntimeSuppressedFreeCount = metric.MustCreateNewUint64Metric(
		"/platform/hvf/runtime_vm/suppressed_free_count",
		metric.Uint64Metadata{Cumulative: true, Description: "Number of Go runtime FREE_REUSABLE operations suppressed to retain live HVF backing."})
	hvfRuntimeSuppressedFreeBytes = metric.MustCreateNewUint64Metric(
		"/platform/hvf/runtime_vm/suppressed_free_bytes",
		metric.Uint64Metadata{Cumulative: true, Description: "Bytes in Go runtime FREE_REUSABLE operations suppressed to retain live HVF backing."})
	hvfRuntimeSuppressedReuseCount = metric.MustCreateNewUint64Metric(
		"/platform/hvf/runtime_vm/suppressed_reuse_count",
		metric.Uint64Metadata{Cumulative: true, Description: "Number of unnecessary Go runtime FREE_REUSE operations suppressed for live HVF backing."})
	hvfRuntimeSuppressedReuseBytes = metric.MustCreateNewUint64Metric(
		"/platform/hvf/runtime_vm/suppressed_reuse_bytes",
		metric.Uint64Metadata{Cumulative: true, Description: "Bytes in unnecessary Go runtime FREE_REUSE operations suppressed for live HVF backing."})
)

func publishRing0RuntimeMetrics() {
	stats := readRing0RuntimeStats()
	hvfRendezvousCount.Set(stats.rendezvousCount)
	hvfRuntimeRendezvousRequests.Set(stats.runtimeRendezvousRequests)
	hvfRendezvousDrainTotalNS.Set(stats.rendezvousDrainTotalNS)
	hvfRendezvousDrainMaxNS.Set(stats.rendezvousDrainMaxNS)
	hvfRendezvousPauseTotalNS.Set(stats.rendezvousPauseTotalNS)
	hvfRendezvousPauseMaxNS.Set(stats.rendezvousPauseMaxNS)
	hvfRendezvousVCPUExitCalls.Set(stats.rendezvousVCPUExitCalls)
	hvfRendezvousVCPUExitTargets.Set(stats.rendezvousVCPUExitTargets)
	hvfRendezvousFailures.Set(stats.rendezvousFailures)
	hvfRuntimeVMHookCalls.Set(stats.runtimeVMHookCalls)
	hvfRuntimeSuppressedFreeCount.Set(stats.suppressedFreeCount)
	hvfRuntimeSuppressedFreeBytes.Set(stats.suppressedFreeBytes)
	hvfRuntimeSuppressedReuseCount.Set(stats.suppressedReuseCount)
	hvfRuntimeSuppressedReuseBytes.Set(stats.suppressedReuseBytes)
}
