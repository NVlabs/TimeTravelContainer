// Copyright 2020 The gVisor Authors.
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

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build amd64
// +build amd64

package usertrap

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/time/rate"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/marshal/primitive"
	"gvisor.dev/gvisor/pkg/rand"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/usermem"
)

// trapNR is the maximum number of traps what can fit in the trap table.
const trapNR = 256

// trapSize is the size of one trap.
const trapSize = 80

var (
	// jmpInst is the binary code of "jmp *addr".
	jmpInst          = [7]byte{0xff, 0x24, 0x25, 0, 0, 0, 0}
	jmpInstOpcodeLen = 3
	// faultInst is the single byte invalid instruction.
	faultInst = [1]byte{0x6}
	// faultInstOffset is the offset of the syscall instruction.
	faultInstOffset = uintptr(5)
)

// tracerWarnLimiter limits how often the warning about attaching a tracer to a
// process with patched syscalls is logged. Without it, the warning is emitted
// on every syscall the traced process makes.
var tracerWarnLimiter = rate.NewLimiter(rate.Every(time.Minute), 1)

type memoryManager interface {
	usermem.IO
	MMap(ctx context.Context, opts memmap.MMapOpts) (hostarch.Addr, error)
	FindVMAByName(ar hostarch.AddrRange, hint string) (hostarch.Addr, uint64, error)
}

// State represents the current state of the trap table.
//
// +stateify savable
type State struct {
	mu        sync.RWMutex `state:"nosave"`
	nextTrap  uint32
	tableAddr hostarch.Addr
	// mode is one of the usertrapMode* values. It is reconstructed from the
	// usertrap table header after address-space fork or restore.
	mode uint32 `state:"nosave"`
}

const (
	usertrapModeUnknown uint32 = iota
	usertrapModeEnabled
	usertrapModeDisabled

	headerFlagDisabled = uint32(1 << 0)
)

// New returns the new state structure.
func New() *State {
	return &State{}
}

// +marshal
type header struct {
	nextTrap uint32
	flags    uint32
}

func (s *State) trapAddr(trap uint32) hostarch.Addr {
	return s.tableAddr + hostarch.Addr(trapSize*trap)
}

// initLocked reconstructs the usertrap state from the guest mapping, creating
// the mapping if create is true and it does not exist.
//
// Preconditions: s.mu must be locked.
func (s *State) initLocked(ctx context.Context, mm memoryManager, create bool) error {
	if s.nextTrap != 0 {
		return nil
	}
	task := kernel.TaskFromContext(ctx)
	if task == nil {
		return fmt.Errorf("no task found")
	}
	addr, off, err := mm.FindVMAByName(trapTableAddrRange, tableVMAName)
	if off != 0 {
		return fmt.Errorf("the usertrap vma has been overmounted")
	}
	if err != nil {
		if !create {
			atomic.StoreUint32(&s.mode, usertrapModeEnabled)
			return nil
		}
		addr = trapTableAddrRange.Start
		if task.MemoryManager().MmapLayoutRandomized() {
			addr += hostarch.Addr(rand.Int63n(int64(trapTableAddrRange.Length() - trapTableSize))).RoundDown()
		}
		ctx.Debugf("Map a usertrap vma at %x", addr)
		if err := loadUsertrap(ctx, mm, addr); err != nil {
			return err
		}
		s.nextTrap = 1
		s.tableAddr = addr
		atomic.StoreUint32(&s.mode, usertrapModeEnabled)
		hdr := header{nextTrap: s.nextTrap}
		_, err := hdr.CopyOut(task.OwnCopyContext(usermem.IOOpts{IgnorePermissions: true}), addr)
		return err
	}
	var hdr header
	if _, err := hdr.CopyIn(task.OwnCopyContext(usermem.IOOpts{}), addr); err != nil {
		return err
	}
	if hdr.nextTrap == 0 || hdr.nextTrap > trapNR {
		return fmt.Errorf("invalid next usertrap index %d", hdr.nextTrap)
	}
	s.nextTrap = hdr.nextTrap
	s.tableAddr = addr
	if hdr.flags&headerFlagDisabled != 0 {
		atomic.StoreUint32(&s.mode, usertrapModeDisabled)
	} else {
		atomic.StoreUint32(&s.mode, usertrapModeEnabled)
	}
	return nil
}

// GuestGSMode reports whether syscall patching has been retired for this
// address space and guest GS should be installed while application code runs.
func (s *State) GuestGSMode(ctx context.Context, mm memoryManager) (bool, error) {
	if mode := atomic.LoadUint32(&s.mode); mode != usertrapModeUnknown {
		return mode == usertrapModeDisabled, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if atomic.LoadUint32(&s.mode) == usertrapModeUnknown {
		if err := s.initLocked(ctx, mm, false); err != nil {
			return false, err
		}
	}
	return atomic.LoadUint32(&s.mode) == usertrapModeDisabled, nil
}

// newTrapLocked allocates a new trap entry.
//
// Preconditions: s.mu must be locked.
func (s *State) newTrapLocked(ctx context.Context, mm memoryManager) (hostarch.Addr, error) {
	var hdr header
	task := kernel.TaskFromContext(ctx)
	if task == nil {
		return 0, fmt.Errorf("no task found")
	}

	// nextTrap and the mode are saved in the usertrap VMA so address-space
	// fork and restore can reconstruct them.
	if err := s.initLocked(ctx, mm, true); err != nil {
		return 0, err
	}
	ctx.Debugf("Allocate a new trap: %p %d", s, s.nextTrap)
	if s.nextTrap >= trapNR {
		ctx.Warningf("No space in the trap table")
		return 0, fmt.Errorf("no space in the trap table")
	}
	trap := s.nextTrap
	s.nextTrap++

	// An entire trap has to be on the same page to avoid memory faults.
	addr := s.trapAddr(trap)
	if addr/hostarch.PageSize != (addr+trapSize)/hostarch.PageSize {
		trap = s.nextTrap
		s.nextTrap++
	}
	hdr = header{
		nextTrap: s.nextTrap,
	}
	if _, err := hdr.CopyOut(task.OwnCopyContext(usermem.IOOpts{IgnorePermissions: true}), s.tableAddr); err != nil {
		return 0, err
	}
	return s.trapAddr(trap), nil
}

// trapTableAddrRange is the range where a trap table can be placed.
//
// The value has to be below 2GB and the high two bytes has to be an invalid
// instruction.  In case of 0x60000, the high two bytes is 0x6. This is "push
// es" in x86 and the bad instruction on x64.
var trapTableAddrRange = hostarch.AddrRange{Start: 0x60000, End: 0x70000}

const (
	trapTableSize = hostarch.Addr(trapNR * trapSize)

	tableVMAName = "[usertrap]"
)

// LoadUsertrap maps the usertrap table into the address space.
func loadUsertrap(ctx context.Context, mm memoryManager, addr hostarch.Addr) error {
	size, _ := hostarch.Addr(trapTableSize).RoundUp()
	// Force is true because Addr is below MinUserAddress.
	_, err := mm.MMap(ctx, memmap.MMapOpts{
		Force:     true,
		Unmap:     true,
		Fixed:     true,
		Addr:      addr,
		Length:    uint64(size),
		Private:   true,
		Name:      tableVMAName,
		MLockMode: memmap.MLockEager,
		// App threads may be in the middle of executing trampoline code
		// when another thread patches a syscall, writing to the table.
		// If the table is marked COW (i.e. due to fork()), the pages
		// unmap under the threads in the middle of a trampoline, causing
		// a fault and corrupting state.
		EagerForkCopy: true,
		// Prevent users from tampering with the usertrap table.
		Sealed: true,
		Perms: hostarch.AccessType{
			Write:   false,
			Read:    true,
			Execute: true,
		},
		MaxPerms: hostarch.AccessType{
			Write:   true,
			Read:    true,
			Execute: true,
		},
	})
	if err != nil {
		return err
	}

	return nil
}

// PatchSyscall changes the syscall instruction into a function call.
func (s *State) PatchSyscall(ctx context.Context, ac *arch.Context64, mm memoryManager) error {
	task := kernel.TaskFromContext(ctx)
	if task == nil {
		return fmt.Errorf("no task found")
	}

	// Skip syscall patching when the task is being ptraced, because
	// single-stepping and other debugger features are incompatible with
	// the "syshandler" routine used to handle patched syscalls (see
	// syshandler_amd64.S). This incompatibility can result in inconsistent
	// process states and failures (e.g. SIGSEGV).
	// TODO(gvisor.dev/issue/11649): for a full fix we'd need to roll back
	//     existing patched syscalls, in case the traced program was patched
	//     before being traced (e.g. PTRACE_ATTACH on an already running
	//     process).
	s.mu.Lock()
	defer s.mu.Unlock()
	if atomic.LoadUint32(&s.mode) == usertrapModeDisabled {
		return nil
	}
	if task.Tracer() != nil {
		if s.nextTrap > 0 && tracerWarnLimiter.Allow() {
			ctx.Warningf("LIKELY ERROR: Attached tracer to process with patched syscalls (traps %d)! Systrap is not fully compatible with ptrace/debuggers, program may die unexpectedly soon! Use `--systrap-disable-syscall-patching` as a workaround.", s.nextTrap)
		}
		return nil
	}

	sysno := ac.SyscallNo()
	patchAddr := ac.IP() - uintptr(len(jmpInst))

	prevCode := make([]uint8, len(jmpInst))
	if _, err := primitive.CopyUint8SliceIn(task, hostarch.Addr(patchAddr), prevCode); err != nil {
		return err
	}

	// Only patch the exact instruction sequence "mov $sysno, %eax; syscall".
	// Checking only the first byte is unsafe: for a syscall reached through a
	// branch, the 5 bytes before the syscall instruction are unrelated code and
	// may coincidentally begin with 0xb8. This is especially common in
	// JIT-generated code. Replacing that code corrupts the application.
	if isPatchableSyscall(prevCode, sysno) {
		ctx.Debugf("Found the pattern at ip %x:sysno %d", patchAddr, sysno)

		trapAddr, err := s.addTrapLocked(ctx, ac, mm, uint32(sysno))
		if trapAddr == 0 || err != nil {
			ctx.Warningf("Failed to add a new trap: %v", err)
			return nil
		}

		// Replace "mov sysno, %eax; syscall" with "jmp trapAddr".
		newCode := make([]uint8, len(jmpInst))
		copy(newCode[:jmpInstOpcodeLen], jmpInst[:jmpInstOpcodeLen])
		binary.LittleEndian.PutUint32(newCode[jmpInstOpcodeLen:], uint32(trapAddr))

		ctx.Debugf("Apply the binary patch addr %x trap addr %x (%v -> %v)", patchAddr, trapAddr, prevCode, newCode)

		ignorePermContext := task.OwnCopyContext(usermem.IOOpts{IgnorePermissions: true})

		// The patch can't be applied atomically, so we need to
		// guarantee that in each moment other threads will read a
		// valid set of instructions, detect any inconsistent states
		// and restart the patched code if so.
		//
		// A subtle aspect is the address at which the user trap table
		// is always mapped which is 0x60000. The first byte of this is
		// 0x06 which is an invalid opcode. That’s why when we
		// overwrite all the bytes but the first 1 in the second step
		// it works fine since the jump address still writes a 0x6 at
		// the location of the first byte of syscall instruction that
		// we are removing and any threads reading the instructions
		// will still fault at the same place.
		//
		// Another subtle aspect is the second step is done using a
		// regular non-atomic write which means a thread decoding the
		// mov instruction could read a garbage value of the immediate
		// operand for the ‘mov sysyno, %eax” instruction. But it
		// doesn’t matter since we don’t change the first byte which is
		// the one that contains the opcode. Also since the thread will
		// fault on the 0x6 right after and will be restarted with the
		// patched code the mov reading a garbage immediate operand
		// doesn’t impact correctness.

		// The patch is applied in three steps:
		//
		// The first step is to replace the first byte of the syscall
		// instruction by one-byte invalid instruction (0x06), so that
		// other threads which have passed the mov instruction fault on
		// the invalid instruction and restart a patched code.
		faultInstB := primitive.ByteSlice(faultInst[:])
		if _, err := faultInstB.CopyOut(ignorePermContext, hostarch.Addr(patchAddr+faultInstOffset)); err != nil {
			return err
		}
		// The second step is to replace all bytes except the first one
		// which is the opcode of the mov instruction, so that the first
		// five bytes remain "mov XXX, %rax".
		if _, err := primitive.CopyUint8SliceOut(ignorePermContext, hostarch.Addr(patchAddr+1), newCode[1:]); err != nil {
			return err
		}
		// The final step is to replace the first byte of the patch.
		// After this point, all threads will read the valid jmp
		// instruction.
		if _, err := primitive.CopyUint8SliceOut(ignorePermContext, hostarch.Addr(patchAddr), newCode[0:1]); err != nil {
			return err
		}
	}
	return nil
}

// Disable atomically retires all syscall patches in mm and prevents creation
// of new ones. The disabled bit is stored in the usertrap VMA so it survives
// address-space fork, checkpoint/restore, TTC live fork, and dehydration.
func (s *State) Disable(ctx context.Context, mm memoryManager) error {
	task := kernel.TaskFromContext(ctx)
	if task == nil {
		return fmt.Errorf("no task found")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if atomic.LoadUint32(&s.mode) == usertrapModeDisabled {
		return nil
	}
	if err := s.initLocked(ctx, mm, true); err != nil {
		return err
	}

	ignorePermContext := task.OwnCopyContext(usermem.IOOpts{IgnorePermissions: true})
	for trap := uint32(1); trap < s.nextTrap; trap++ {
		trapAddr := s.trapAddr(trap)
		trapCode := make([]byte, trapSize)
		if _, err := primitive.CopyUint8SliceIn(task, trapAddr, trapCode); err != nil {
			return fmt.Errorf("reading usertrap %d: %w", trap, err)
		}
		// Padding entries and entries never fully initialized have no self
		// pointer. Fully validate the fields needed to reconstruct the patch;
		// the trap table is guest-mapped and therefore untrusted.
		if binary.LittleEndian.Uint64(trapCode[:8]) != uint64(trapAddr)+8 ||
			trapCode[57] != 0xb8 {
			continue
		}
		returnAddr := binary.LittleEndian.Uint64(trapCode[40:48])
		if returnAddr < uint64(len(jmpInst)) {
			continue
		}
		patchAddr := hostarch.Addr(returnAddr - uint64(len(jmpInst)))
		sysno := binary.LittleEndian.Uint32(trapCode[58:62])

		expectedPatch := jmpInst
		binary.LittleEndian.PutUint32(expectedPatch[jmpInstOpcodeLen:], uint32(trapAddr))
		current := make([]byte, len(jmpInst))
		if _, err := primitive.CopyUint8SliceIn(task, patchAddr, current); err != nil {
			// The patched mapping may have been legitimately unmapped since the
			// trap was allocated.
			continue
		}
		if !bytes.Equal(current, expectedPatch[:]) {
			// Do not overwrite code that the application changed after TTC
			// installed the patch.
			continue
		}

		original := [7]byte{0xb8, 0, 0, 0, 0, 0x0f, 0x05}
		binary.LittleEndian.PutUint32(original[1:5], sysno)
		// Reverse the installation protocol. The invalid first byte makes
		// concurrent entrants fault until the complete original instruction
		// sequence is visible; HandleFault restarts them after this lock drops.
		faultInstB := primitive.ByteSlice(faultInst[:])
		if _, err := faultInstB.CopyOut(ignorePermContext, patchAddr); err != nil {
			return fmt.Errorf("arming usertrap retirement at %#x: %w", patchAddr, err)
		}
		if _, err := primitive.CopyUint8SliceOut(ignorePermContext, patchAddr+1, original[1:]); err != nil {
			// Best effort: restore the old jump opcode so a failed transition
			// does not strand the instruction at the fault byte.
			_, _ = primitive.CopyUint8SliceOut(ignorePermContext, patchAddr, expectedPatch[:1])
			return fmt.Errorf("retiring usertrap at %#x: %w", patchAddr, err)
		}
		if _, err := primitive.CopyUint8SliceOut(ignorePermContext, patchAddr, original[:1]); err != nil {
			return fmt.Errorf("completing usertrap retirement at %#x: %w", patchAddr, err)
		}
	}

	hdr := header{nextTrap: s.nextTrap, flags: headerFlagDisabled}
	if _, err := hdr.CopyOut(ignorePermContext, s.tableAddr); err != nil {
		return fmt.Errorf("persisting disabled usertrap mode: %w", err)
	}
	atomic.StoreUint32(&s.mode, usertrapModeDisabled)
	return nil
}

func isPatchableSyscall(code []byte, sysno uintptr) bool {
	if len(code) != len(jmpInst) || sysno != uintptr(uint32(sysno)) {
		return false
	}
	return code[0] == 0xb8 &&
		binary.LittleEndian.Uint32(code[1:5]) == uint32(sysno) &&
		code[5] == 0x0f && code[6] == 0x05
}

// HandleFault handles a fault on a patched syscall instruction.
//
// When we replace a system call with a function call, we replace two
// instructions with one instruction. This means that here can be a thread
// which called the first instruction, then another thread applied a binary
// patch and the first thread calls the second instruction.
//
// To handle this case, the function call (jmp) instruction is constructed so
// that the first byte of the syscall instruction is changed with the one-byte
// invalid instruction (0x6).  And in case of the race, the first thread will
// fault on the invalid instruction and HandleFault will restart the function
// call.
func (s *State) HandleFault(ctx context.Context, ac *arch.Context64, mm memoryManager) error {
	task := kernel.TaskFromContext(ctx)
	if task == nil {
		return fmt.Errorf("no task found")
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	if atomic.LoadUint32(&s.mode) == usertrapModeDisabled {
		// During retirement, a concurrent thread may have decoded the invalid
		// guard byte before the original instruction was restored. It reaches
		// here only after Disable releases s.mu, so restart at the now-complete
		// original sequence. Handle both the inverse guard at patchAddr and the
		// forward-patching guard at patchAddr+faultInstOffset.
		for _, delta := range []uintptr{0, faultInstOffset} {
			if ac.IP() < delta {
				continue
			}
			ip := ac.IP() - delta
			code := make([]byte, len(jmpInst))
			if _, err := primitive.CopyUint8SliceIn(task, hostarch.Addr(ip), code); err == nil &&
				isPatchableSyscall(code, uintptr(binary.LittleEndian.Uint32(code[1:5]))) {
				ac.SetIP(ip)
				return ErrFaultRestart
			}
		}
	}

	code := make([]uint8, len(jmpInst))
	ip := ac.IP() - faultInstOffset
	if _, err := primitive.CopyUint8SliceIn(task, hostarch.Addr(ip), code); err != nil {
		return err
	}

	for i := 0; i < jmpInstOpcodeLen; i++ {
		if code[i] != jmpInst[i] {
			return nil
		}
	}
	for i := 0; i < len(faultInst); i++ {
		if code[i+int(faultInstOffset)] != faultInst[i] {
			return nil
		}
	}

	regs := &ac.StateData().Regs
	if regs.Rax == uint64(unix.SYS_RESTART_SYSCALL) {
		// restart_syscall is usually set by the Sentry to restart a
		// system call after interruption by a stop signal. The Sentry
		// sets RAX and moves RIP back on the size of the syscall
		// instruction.
		//
		// RAX can't be set to SYS_RESTART_SYSCALL due to a race with
		// injecting a function call, because neither of the two first
		// bytes are equal to proper bytes of jmpInst.
		regs.Orig_rax = regs.Rax
		regs.Rip += arch.SyscallWidth
		return ErrFaultSyscall
	}

	ac.SetIP(ip)
	return ErrFaultRestart
}

// PreFork locks the trap table for reading. This call guarantees that the trap
// table will not be changed before the next PostFork call.
// +checklocksacquireread:s.mu
func (s *State) PreFork() {
	s.mu.RLock()
}

// PostFork unlocks the trap table.
// +checklocksreleaseread:s.mu
func (s *State) PostFork() {
	s.mu.RUnlock()
}
