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
	hvfRuntimeReleasedCount = metric.MustCreateNewUint64Metric(
		"/platform/hvf/runtime_vm/released_count",
		metric.Uint64Metadata{Cumulative: true, Description: "Number of Go runtime FREE_REUSABLE operations that unmapped live HVF backing so that Darwin could reclaim it."})
	hvfRuntimeReleasedBytes = metric.MustCreateNewUint64Metric(
		"/platform/hvf/runtime_vm/released_bytes",
		metric.Uint64Metadata{Cumulative: true, Description: "Bytes in Go runtime FREE_REUSABLE operations that unmapped live HVF backing so that Darwin could reclaim it."})
	hvfRuntimeSuppressedReuseCount = metric.MustCreateNewUint64Metric(
		"/platform/hvf/runtime_vm/suppressed_reuse_count",
		metric.Uint64Metadata{Cumulative: true, Description: "Number of unnecessary Go runtime FREE_REUSE operations suppressed for live HVF backing."})
	hvfRuntimeSuppressedReuseBytes = metric.MustCreateNewUint64Metric(
		"/platform/hvf/runtime_vm/suppressed_reuse_bytes",
		metric.Uint64Metadata{Cumulative: true, Description: "Bytes in unnecessary Go runtime FREE_REUSE operations suppressed for live HVF backing."})
	hvfRuntimeSuppressedUnmapCount = metric.MustCreateNewUint64Metric(
		"/platform/hvf/runtime_vm/suppressed_unmap_count",
		metric.Uint64Metadata{Cumulative: true, Description: "Number of Go runtime unmaps suppressed to retain live HVF backing for the VM epoch."})
	hvfRuntimeSuppressedUnmapBytes = metric.MustCreateNewUint64Metric(
		"/platform/hvf/runtime_vm/suppressed_unmap_bytes",
		metric.Uint64Metadata{Cumulative: true, Description: "Bytes in Go runtime unmaps suppressed to retain live HVF backing for the VM epoch."})
	hvfMonitorCommands = metric.MustCreateNewUint64Metric(
		"/platform/hvf/monitor/commands",
		metric.Uint64Metadata{Cumulative: true, Description: "Number of EL1 monitor commands requested."})
	hvfMonitorTranslationCommands = metric.MustCreateNewUint64Metric(
		"/platform/hvf/monitor/translation_commands",
		metric.Uint64Metadata{Cumulative: true, Description: "Number of translation-mutating EL1 monitor commands requested."})
	hvfMonitorResidentCommands = metric.MustCreateNewUint64Metric(
		"/platform/hvf/monitor/resident_commands",
		metric.Uint64Metadata{Cumulative: true, Description: "Number of translation commands completed directly by a resident Sentry EL1 continuation."})
	hvfMonitorResidentTotalNS = metric.MustCreateNewUint64Metric(
		"/platform/hvf/monitor/resident_ns",
		metric.Uint64Metadata{Cumulative: true, Description: "Total resident-Sentry EL1 monitor execution time, in nanoseconds."})
	hvfMonitorResidentMaxNS = metric.MustCreateNewUint64Metric(
		"/platform/hvf/monitor/resident_max_ns",
		metric.Uint64Metadata{Cumulative: true, Description: "Maximum resident-Sentry EL1 monitor execution time, in nanoseconds."})
	hvfMonitorHostCommands = metric.MustCreateNewUint64Metric(
		"/platform/hvf/monitor/host_commands",
		metric.Uint64Metadata{Cumulative: true, Description: "Number of EL1 monitor commands completed through the Darwin host path."})
	hvfMonitorHostTotalNS = metric.MustCreateNewUint64Metric(
		"/platform/hvf/monitor/host_ns",
		metric.Uint64Metadata{Cumulative: true, Description: "Total Darwin-host monitor execution time after redpill, in nanoseconds."})
	hvfMonitorHostMaxNS = metric.MustCreateNewUint64Metric(
		"/platform/hvf/monitor/host_max_ns",
		metric.Uint64Metadata{Cumulative: true, Description: "Maximum Darwin-host monitor execution time after redpill, in nanoseconds."})
	hvfMonitorFallbackHostEL = metric.MustCreateNewUint64Metric(
		"/platform/hvf/monitor/fallback_host_el",
		metric.Uint64Metadata{Cumulative: true, Description: "Resident monitor attempts rejected because the Sentry continuation was already at host EL0."})
	hvfMonitorFallbackUnsupported = metric.MustCreateNewUint64Metric(
		"/platform/hvf/monitor/fallback_unsupported",
		metric.Uint64Metadata{Cumulative: true, Description: "Resident monitor attempts rejected because the command cannot run on the resident path."})
	hvfMonitorFallbackEntryLock = metric.MustCreateNewUint64Metric(
		"/platform/hvf/monitor/fallback_entry_lock",
		metric.Uint64Metadata{Cumulative: true, Description: "Resident monitor attempts rejected by application-entry mutation lock contention."})
	hvfMonitorFallbackMonitorLock = metric.MustCreateNewUint64Metric(
		"/platform/hvf/monitor/fallback_monitor_lock",
		metric.Uint64Metadata{Cumulative: true, Description: "Resident monitor attempts rejected by monitor serialization lock contention."})
	hvfMonitorFallbackOwner = metric.MustCreateNewUint64Metric(
		"/platform/hvf/monitor/fallback_owner",
		metric.Uint64Metadata{Cumulative: true, Description: "Resident monitor attempts rejected because the current owner-affine vCPU was unavailable."})
	hvfMonitorFallbackConflict = metric.MustCreateNewUint64Metric(
		"/platform/hvf/monitor/fallback_conflict",
		metric.Uint64Metadata{Cumulative: true, Description: "Resident monitor attempts rejected by another active reader of the affected address space."})
	hvfMonitorFallbackResources = metric.MustCreateNewUint64Metric(
		"/platform/hvf/monitor/fallback_resources",
		metric.Uint64Metadata{Cumulative: true, Description: "Resident monitor attempts rejected by cache-maintenance or VM resource contention."})
	hvfMonitorApplicationMapPages = metric.MustCreateNewUint64Metric(
		"/platform/hvf/monitor/application_map_pages",
		metric.Uint64Metadata{Cumulative: true, Description: "Application 4K pages successfully mapped by EL1 monitor commands."})
	hvfMonitorApplicationUnmapPages = metric.MustCreateNewUint64Metric(
		"/platform/hvf/monitor/application_unmap_pages",
		metric.Uint64Metadata{Cumulative: true, Description: "Application 4K pages successfully unmapped by EL1 monitor commands."})
	hvfMonitorApplicationProtectPages = metric.MustCreateNewUint64Metric(
		"/platform/hvf/monitor/application_protect_pages",
		metric.Uint64Metadata{Cumulative: true, Description: "Application 4K pages successfully protected by EL1 monitor commands."})
	hvfVCPUsCreated = metric.MustCreateNewUint64Metric(
		"/platform/hvf/vcpu/created",
		metric.Uint64Metadata{Cumulative: true, Description: "Number of application vCPUs created."})
	hvfVCPUsReused = metric.MustCreateNewUint64Metric(
		"/platform/hvf/vcpu/reused",
		metric.Uint64Metadata{Cumulative: true, Description: "Number of residencies that took the vCPU parked on their host thread instead of creating one."})
	hvfVCPURetirementsRequested = metric.MustCreateNewUint64Metric(
		"/platform/hvf/vcpu/retirements_requested",
		metric.Uint64Metadata{Cumulative: true, Description: "Number of parked vCPUs whose host threads were asked to retire them."})
	hvfVCPUsRetiredBeforeSyscall = metric.MustCreateNewUint64Metric(
		"/platform/hvf/vcpu/retired_before_syscall",
		metric.Uint64Metadata{Cumulative: true, Description: "Number of parked vCPUs that their host threads retired before a system call of another goroutine."})
	hvfVCPUsRetiredAtExit = metric.MustCreateNewUint64Metric(
		"/platform/hvf/vcpu/retired_at_exit",
		metric.Uint64Metadata{Cumulative: true, Description: "Number of parked vCPUs that their host threads retired as they exited."})
	hvfVCPUsRetiredOnRequest = metric.MustCreateNewUint64Metric(
		"/platform/hvf/vcpu/retired_on_request",
		metric.Uint64Metadata{Cumulative: true, Description: "Number of parked vCPUs that their host threads retired on request, at a safe point."})
)

func publishRing0RuntimeMetrics(engine *ring0Engine) {
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
	hvfRuntimeReleasedCount.Set(stats.releasedCount)
	hvfRuntimeReleasedBytes.Set(stats.releasedBytes)
	hvfRuntimeSuppressedReuseCount.Set(stats.suppressedReuseCount)
	hvfRuntimeSuppressedReuseBytes.Set(stats.suppressedReuseBytes)
	hvfRuntimeSuppressedUnmapCount.Set(stats.suppressedUnmapCount)
	hvfRuntimeSuppressedUnmapBytes.Set(stats.suppressedUnmapBytes)
	hvfMonitorCommands.Set(engine.monitorCommands.Load())
	hvfMonitorTranslationCommands.Set(engine.monitorTranslationCommands.Load())
	hvfMonitorResidentCommands.Set(engine.monitorResidentCommands.Load())
	hvfMonitorResidentTotalNS.Set(engine.monitorResidentTotalNS.Load())
	hvfMonitorResidentMaxNS.Set(engine.monitorResidentMaxNS.Load())
	hvfMonitorHostCommands.Set(engine.monitorHostCommands.Load())
	hvfMonitorHostTotalNS.Set(engine.monitorHostTotalNS.Load())
	hvfMonitorHostMaxNS.Set(engine.monitorHostMaxNS.Load())
	hvfMonitorFallbackHostEL.Set(engine.monitorResidentFallbackHostEL.Load())
	hvfMonitorFallbackUnsupported.Set(engine.monitorResidentFallbackUnsupported.Load())
	hvfMonitorFallbackEntryLock.Set(engine.monitorResidentFallbackEntryLock.Load())
	hvfMonitorFallbackMonitorLock.Set(engine.monitorResidentFallbackMonitorLock.Load())
	hvfMonitorFallbackOwner.Set(engine.monitorResidentFallbackOwner.Load())
	hvfMonitorFallbackConflict.Set(engine.monitorResidentFallbackConflict.Load())
	hvfMonitorFallbackResources.Set(engine.monitorResidentFallbackResources.Load())
	hvfMonitorApplicationMapPages.Set(engine.monitorApplicationMapPages.Load())
	hvfMonitorApplicationUnmapPages.Set(engine.monitorApplicationUnmapPages.Load())
	hvfMonitorApplicationProtectPages.Set(engine.monitorApplicationProtectPages.Load())
	hvfVCPUsCreated.Set(engine.vcpusCreated.Load())
	hvfVCPUsReused.Set(engine.vcpusReused.Load())
	hvfVCPURetirementsRequested.Set(engine.vcpusRetirementsRequested.Load())
	hvfVCPUsRetiredBeforeSyscall.Set(stats.parkedRetiredBeforeSyscall)
	hvfVCPUsRetiredAtExit.Set(stats.parkedRetiredAtExit)
	hvfVCPUsRetiredOnRequest.Set(stats.parkedRetiredOnRequest)
}
