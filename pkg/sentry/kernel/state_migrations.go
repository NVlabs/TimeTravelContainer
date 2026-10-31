// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kernel

import (
	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/state"
)

// How this package's types load from what builds before TTC's merge of
// upstream gVisor through 2026-10-28 (TTC's dailies through 20261023) saved;
// see pkg/state/migrate.go, and runsc/ttcd/schemas for their shapes.
func init() {
	// TTC sets neither new flag: the signal-unkillable policy is none, which is
	// zero, and no signal dumps goroutines, as 0 does not. Restores set each
	// container's environment from its spec.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/sentry/kernel.Kernel",
		Fields: []string{"liveForkQuarantine", "featureSet", "timekeeper", "tasks", "rootUserNamespace", "rootNetworkNamespace", "applicationCores", "useHostCores", "extraAuxv", "vdso", "vdsoParams", "rootUTSNamespace", "rootIPCNamespace", "rootCgroupNamespace", "cpuTopology", "cpuTopologyFingerprint", "futexes", "globalInit", "syslog", "internalConsoles", "nextInternalConsoleEpoch", "runningTasks", "cpuClockTickerRunning", "cpuClock", "userCPUClock", "userSysCPUClock", "uniqueID", "nextInotifyCookie", "netlinkPorts", "danglingEndpoints", "sockets", "nextSocketRecord", "SpecialOpts", "vfs", "hostMount", "pipeMount", "nsfsMount", "shmMount", "socketMount", "sysVShmDevID", "SleepForAddressSpaceActivation", "ptraceExceptions", "YAMAPtraceScope", "cgroupRegistry", "cgroupMountsMap", "userCountersMap", "MaxFDLimit", "RandomizeVASpace", "KptrRestrict", "DmesgRestrict", "MmapRndBits", "MmapRndCompatBits", "ebpf", "containerNames", "additionalCheckpointState", "CheckpointWait", "checkpointGen", "SaveRestoreExecConfig", "NvidiaDriverVersion", "AllowSUID", "IOUringEnabled", "LiveForkEligible", "MaxKeySetSize", "HostNamePoller", "DomainNamePoller"},
		Zero:   []string{"signalUnkillable", "dumpGoroutinesSignal", "containerEnvs"},
	})
	// Older builds had no PID namespace without an init.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/sentry/kernel.PIDNamespace",
		Fields: []string{"owner", "parent", "userns", "id", "last", "tasks", "tids", "tgids", "sessions", "sids", "processGroups", "pgids", "exiting", "extra", "inode"},
		Zero:   []string{"noInit"},
	})
	// gostate now packs the state with gostateTime. Older builds had no
	// cgroup.freeze. After load, a traced task gets its tracer's credentials as
	// ptracerCreds, which is nil only for a task that is not traced; the
	// restart_syscall bypass is armed as saves now arm it; and the task's
	// MemoryManager, which older builds saved without one, gets the task's user
	// namespace.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/sentry/kernel.Task",
		Fields: []string{"taskNode", "runState", "taskWorkCount", "taskWork", "haveSyscallReturn", "gostate", "gostateTime", "appCPUClock", "appSysCPUClock", "yieldCount", "pendingSignals", "signalMask", "realSignalMask", "haveSavedSignalMask", "savedSignalMask", "signalStack", "signalQueue", "groupStopPending", "groupStopAcknowledged", "trapStopPending", "trapNotifyPending", "stop", "exitStatus", "syscallRestartBlock", "k", "containerID", "image", "fsContext", "fdTable", "userDumpable", "vforkParent", "exitState", "exitTracerNotified", "exitTracerAcked", "exitParentNotified", "exitParentAcked", "ptraceTracer", "ptraceTracees", "ptraceSeized", "ptraceOpts", "ptraceSyscallMode", "ptraceSinglestep", "ptraceCode", "ptraceSiginfo", "ptraceEventMsg", "ptraceYAMAExceptionAdded", "ioUsage", "creds", "noNewPrivs", "utsns", "ipcns", "cgroupns", "mountNamespace", "parentDeathSignal", "seccomp", "cleartid", "allowedCPUMask", "cpu", "sched", "niceness", "ioprio", "numaPolicy", "numaNodeMask", "netns", "rseqCPU", "oldRSeqCPUAddr", "rseqAddr", "rseqSignature", "robustList", "startTime", "kcov", "cgroups", "cgroup2", "memCgID", "userCounters", "sessionKeyring", "personality", "Origin", "onDestroyAction", "execveCredsMutexOwner", "pid"},
		Zero:   []string{"freezeOrdered", "hasFreezeCredit", "freezeCreditCgroup"},
		Set:    []string{"gostate", "ptracerCreds", "bypassSeccompRestartSyscall"},
		Old:    map[string]any{"gostate": new(atomicbitops.Uint32), "gostateTime": new(atomicbitops.Int64)},
		Fix: func(obj any, old map[string]any) {
			st := old["gostate"].(*atomicbitops.Uint32).Load()
			when := old["gostateTime"].(*atomicbitops.Int64).Load()
			obj.(*Task).gostate.Store(uint64(when)<<gostateBits | uint64(st))
		},
		FixAfterLoad: func(obj any, old map[string]any) {
			t := obj.(*Task)
			if tracer := t.ptraceTracer.Load(); tracer != nil {
				t.ptracerCreds = tracer.Credentials()
			}
			if t.stop == nil {
				t.armSeccompRestartSyscallBypass()
			}
			if mm := t.image.MemoryManager; mm != nil && mm.UserNamespace() == nil {
				mm.SetUserNamespace(t.Credentials().UserNamespace)
			}
		},
	})
	// Older builds did not run execute-only binaries.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/sentry/kernel.TaskImage",
		Fields: []string{"Name", "Arch", "MemoryManager", "fu", "st"},
		Zero:   []string{"NotDumpable"},
	})
	// After load: a container's init is a thread group whose leader has no
	// parent and was not created by an exec, the rule by which older builds'
	// restores found it; and an init of a PID namespace is unkillable by its
	// peers, as newTask marks it.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/sentry/kernel.ThreadGroup",
		Fields: []string{"threadGroupNode", "signalHandlers", "pendingSignals", "groupStopDequeued", "groupStopSignal", "groupStopPendingCount", "groupStopComplete", "groupStopWaitable", "groupContNotify", "groupContInterrupted", "groupContWaitable", "exiting", "exitStatus", "terminationSignal", "itimerRealTimer", "itimerRealListener", "itimerVirtTimer", "itimerVirtListener", "itimerProfTimer", "itimerProfListener", "rlimitCPUSoftTimer", "rlimitCPUSoftListener", "rlimitCPUHardTimer", "rlimitCPUHardListener", "timers", "nextTimerID", "appCPUClockLast", "appCPUClock", "appSysCPUClockLast", "appSysCPUClock", "yieldCount", "childCPUStats", "ioUsage", "maxRSS", "childMaxRSS", "limits", "processGroup", "execed", "oldRSeqCritical", "tty", "oomScoreAdj", "isChildSubreaper", "hasChildSubreaper", "execveCredsMutexLocked", "execveCredsMutexWaiters", "sigsegvLockCount", "coredumpFilter"},
		Set:    []string{"containerInit", "signalUnkillable"},
		FixAfterLoad: func(obj any, old map[string]any) {
			tg := obj.(*ThreadGroup)
			leader := tg.leader
			if leader == nil {
				return
			}
			tg.containerInit = leader.parent == nil && leader.Origin != OriginExec
			if tg.pidns != nil && tg.pidns.tgids[tg] == initTID {
				tg.signalUnkillable.Store(true)
			}
		},
	})
}
