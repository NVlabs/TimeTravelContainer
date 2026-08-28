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

import _ "unsafe" // Required for go:linkname.

// A resident Sentry continuation is invisible to the host Go scheduler while
// HVF executes EL0 and EL1. Match KVM's runtime contract so GC and asynchronous
// preemption never treat that continuation as ordinary host Go execution.

//go:linkname ring0EnterSyscall runtime.entersyscall
func ring0EnterSyscall()

//go:linkname ring0ExitSyscall runtime.exitsyscall
func ring0ExitSyscall()

// ring0SetVMMemoryHooks installs the allocation-free Darwin runtime notifier
// supplied by tools/go_darwin_vm_hooks.patch.
//
//go:linkname ring0SetVMMemoryHooks runtime.ttcSetVMMemoryHooks
func ring0SetVMMemoryHooks(version uint32, ensureHost, callback uintptr) int32

// ring0SetRuntimeTimeHook installs the allocation-free Darwin wall/monotonic
// clock hook supplied by tools/go_darwin_vm_hooks.patch.
//
//go:linkname ring0SetRuntimeTimeHook runtime.ttcSetRuntimeTimeHook
func ring0SetRuntimeTimeHook(version uint32, callback uintptr) int32
