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

//go:build arm64

// Package arm64 adapts sentry ARM64 contexts to vmplatform's disposable VMM
// register transport.
package arm64

import (
	"fmt"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	sentryplatform "gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform"
	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform/vmm"
)

// Architecture implements vmplatform.Architecture for Linux AArch64.
type Architecture struct{}

func (Architecture) NewVCPUState() any { return &vmm.ARM64State{} }

func (Architecture) LoadApplicationState(dst any, src *arch.Context64) error {
	state, ok := dst.(*vmm.ARM64State)
	if !ok {
		return fmt.Errorf("ARM64 vmplatform state has type %T, want *vmm.ARM64State", dst)
	}
	state.X = src.Regs.Regs
	state.SP = src.Regs.Sp
	state.PC = src.Regs.Pc
	state.PState = src.Regs.Pstate &^ 0xf
	state.TPIDR = src.Regs.TPIDR_EL0
	if fp := src.FloatingPointData(); fp != nil && len(*fp) >= 528 {
		copy(state.FP[:], (*fp)[8:528])
	}
	return nil
}

func (Architecture) SaveApplicationState(dst *arch.Context64, src any, full bool) error {
	state, ok := src.(*vmm.ARM64State)
	if !ok {
		return fmt.Errorf("ARM64 vmplatform state has type %T, want *vmm.ARM64State", src)
	}
	dst.Regs.Regs = state.X
	dst.Regs.Sp = state.SP
	dst.Regs.Pc = state.PC
	dst.Regs.Pstate = state.PState &^ 0xf
	dst.Regs.TPIDR_EL0 = state.TPIDR
	if full {
		if fp := dst.FloatingPointData(); fp != nil && len(*fp) >= 528 {
			copy((*fp)[8:528], state.FP[:])
		}
	}
	return nil
}

func (Architecture) DecodeExit(exit vmplatform.Exit) (*linux.SignalInfo, hostarch.AccessType, error) {
	switch exit.Reason {
	case vmplatform.ExitFault:
		info := &linux.SignalInfo{Signo: int32(linux.SIGSEGV)}
		info.SetAddr(exit.Address)
		access := hostarch.AccessType{
			Read:    exit.Access&vmm.Stage2Read != 0,
			Write:   exit.Access&vmm.Stage2Write != 0,
			Execute: exit.Access&vmm.Stage2Execute != 0,
		}
		return info, access, sentryplatform.ErrContextSignal
	case vmplatform.ExitShutdown:
		return nil, hostarch.NoAccess, fmt.Errorf("ARM64 VM shut down")
	default:
		log.Debugf("vmplatform ARM64 unknown application exit: reason=%d syndrome=%#x address=%#x", exit.Reason, exit.Syndrome, exit.Address)
		info := &linux.SignalInfo{Signo: int32(linux.SIGILL)}
		info.SetAddr(exit.Address)
		return info, hostarch.NoAccess, sentryplatform.ErrContextSignal
	}
}
