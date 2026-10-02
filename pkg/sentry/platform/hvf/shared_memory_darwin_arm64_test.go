// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin && arm64

package hvf_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"sync"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
	gcontext "gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	sentryplatform "gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/platform/hvf"
	"gvisor.dev/gvisor/pkg/sentry/platform/hvf/ring0asm"
	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform"
	vmarm64 "gvisor.dev/gvisor/pkg/sentry/platform/vmplatform/arm64"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

// sharedTestMemory is a memmap.HostMemoryFile: a writable data file that the
// test maps shared, as the Sentry maps a GPU renderer's shared memory. Its
// pages are ordinary data pages, never executable in the host.
type sharedTestMemory struct {
	memmap.NoBufferedIOFallback
	memmap.DefaultMemoryType

	f       *os.File
	mapping []byte

	mu    sync.Mutex
	hooks []func()
}

func newSharedTestMemory(t *testing.T, size int) *sharedTestMemory {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "shared")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(int64(size)); err != nil {
		t.Fatal(err)
	}
	mapping, err := unix.Mmap(int(f.Fd()), 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		t.Fatal(err)
	}
	return &sharedTestMemory{f: f, mapping: mapping}
}

func (m *sharedTestMemory) IncRef(memmap.FileRange, uint32) {}
func (m *sharedTestMemory) DecRef(memmap.FileRange)         {}
func (m *sharedTestMemory) MapInternal(fr memmap.FileRange, _ hostarch.AccessType) (safemem.BlockSeq, error) {
	return safemem.BlockSeqOf(safemem.BlockFromSafeSlice(m.mapping[fr.Start:fr.End])), nil
}
func (m *sharedTestMemory) DataFD(memmap.FileRange) (int, error) { return int(m.f.Fd()), nil }
func (m *sharedTestMemory) FD() int                              { return int(m.f.Fd()) }
func (m *sharedTestMemory) HostAddress(fr memmap.FileRange) (uintptr, error) {
	if fr.End > uint64(len(m.mapping)) {
		return 0, fmt.Errorf("%v is beyond %d bytes", fr, len(m.mapping))
	}
	return uintptr(unsafe.Pointer(&m.mapping[0])) + uintptr(fr.Start), nil
}
func (m *sharedTestMemory) OnRelease(f func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hooks = append(m.hooks, f)
}

// release runs the release hooks, and unmaps the memory.
func (m *sharedTestMemory) release(t *testing.T) {
	m.mu.Lock()
	hooks := m.hooks
	m.hooks = nil
	m.mu.Unlock()
	for _, f := range hooks {
		f()
	}
	if err := unix.Munmap(m.mapping); err != nil {
		t.Error(err)
	}
	m.f.Close()
}

func TestRing0PlatformMapsSharedHostMemoryInPlace(t *testing.T) {
	backend := hvf.NewBackend()
	vm, err := backend.CreateVM(context.Background())
	if err != nil {
		skipWithoutHypervisor(t, err)
	}
	p, err := vmplatform.New(vmplatform.Config{
		VM: vm, HostMemory: backend.HostMemory(), Architecture: vmarm64.Architecture{},
		VCPUs: 2, IPAStart: hvf.GuestRAMIPAStart, IPASize: 1 << 30,
		PlatformName: "hvf-test", VMEngineFactory: hvf.NewVMEngine,
	})
	if err != nil {
		_ = vm.Close()
		t.Fatal(err)
	}
	defer p.Close()

	mf, err := pgalloc.NewMemoryFile(nil, pgalloc.MemoryFileOpts{
		BackingProvider:         p.MemoryFileBackingProvider(),
		DelayedEviction:         pgalloc.DelayedEvictionDisabled,
		DisableMemoryAccounting: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mf.Destroy()
	codeRange, err := mf.Allocate(hostarch.PageSize, pgalloc.AllocOpts{Kind: usage.Anonymous, Mode: pgalloc.AllocateAndWritePopulate})
	if err != nil {
		t.Fatal(err)
	}
	defer mf.DecRef(codeRange)
	const (
		codeVA   = hostarch.Addr(0x600000)
		sharedVA = hostarch.Addr(0x800000)
	)
	// Make a syscall, then read the word at sharedVA, and store it plus one
	// after it.
	var code []byte
	for _, insn := range []uint32{
		0xd2801588, // movz x8, #172
		0xd4000001, // svc #0
		0xd2a01009, // movz x9, #0x80, lsl #16 (x9 = sharedVA)
		0xb940012a, // ldr w10, [x9]
		0x1100054a, // add w10, w10, #1
		0xb900052a, // str w10, [x9, #4]
		0xd2801588, // movz x8, #172
		0xd4000001, // svc #0
	} {
		code = binary.LittleEndian.AppendUint32(code, insn)
	}
	blocks, lease, err := memmap.MapInternalWithLease(mf, codeRange, hostarch.Write)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := safemem.CopySeq(blocks, safemem.BlockSeqOf(safemem.BlockFromSafeSlice(code))); err != nil {
		lease.Release()
		t.Fatal(err)
	}
	lease.Release()

	// Two host pages, as the renderer's blobs are whole host pages.
	shared := newSharedTestMemory(t, 2*unix.Getpagesize())
	as, err := p.NewAddressSpace()
	if err != nil {
		t.Fatal(err)
	}
	if err := as.MapFile(codeVA, mf, codeRange, hostarch.ReadExecute, false); err != nil {
		t.Fatal(err)
	}
	platformCtx := p.NewContext(gcontext.Background())
	defer platformCtx.Release()
	mm := &compactionTestMemoryManager{as: as}

	// As in a Sentry, the guest faults on the memory first, and the fault's
	// handler maps it wherever the continuation that ran the guest is. In a
	// Sentry that is guest EL1 after a syscall, where the Sentry can touch
	// only memory that it owns, or that stage 2 already maps; in this test's
	// process, whose runtime cannot stay at EL1, it is host EL0.
	binary.LittleEndian.PutUint32(shared.mapping[0:], 41)
	ac := arch.New(arch.ARM64)
	ac.SetIP(uintptr(codeVA))
	if _, _, err := platformCtx.Switch(gcontext.Background(), mm, ac, -1); err != nil || ac.Regs.Regs[8] != 172 {
		t.Fatalf("the guest's first syscall: %v, x8=%d", err, ac.Regs.Regs[8])
	}
	info, _, err := platformCtx.Switch(gcontext.Background(), mm, ac, -1)
	if err != sentryplatform.ErrContextSignal || info.Addr() != uint64(sharedVA) {
		t.Fatalf("running the guest before the memory is mapped: %v, %+v; want a fault at %#x", err, info, sharedVA)
	}
	t.Logf("mapping the memory at EL%d", ring0asm.CurrentEL())
	sharedRange := memmap.FileRange{Start: 0, End: hostarch.PageSize}
	if err := as.MapFile(sharedVA, shared, sharedRange, hostarch.ReadWrite, false); err != nil {
		t.Fatalf("mapping shared memory: %v", err)
	}
	if err := as.MapFile(sharedVA+hostarch.PageSize, shared, sharedRange, hostarch.AnyAccess, false); err == nil {
		t.Fatal("mapped shared memory executable")
	}
	if _, _, err := platformCtx.Switch(gcontext.Background(), mm, ac, -1); err != nil {
		t.Fatalf("resuming the guest: %v", err)
	}
	if got := binary.LittleEndian.Uint32(shared.mapping[4:]); ac.Regs.Regs[8] != 172 || got != 42 {
		t.Fatalf("after resuming, the guest wrote %d and made syscall %d; want 42 and 172", got, ac.Regs.Regs[8])
	}

	for _, value := range []uint32{1000, 7} {
		// The host's write is the guest's read, and the guest's write is the
		// host's read: nothing staged the memory.
		binary.LittleEndian.PutUint32(shared.mapping[0:], value)
		ac := arch.New(arch.ARM64)
		ac.SetIP(uintptr(codeVA) + 8)
		if _, _, err := platformCtx.Switch(gcontext.Background(), mm, ac, -1); err != nil {
			t.Fatalf("running the guest: %v", err)
		}
		if got := ac.Regs.Regs[8]; got != 172 {
			t.Fatalf("syscall sentinel = %d, want 172", got)
		}
		if got := binary.LittleEndian.Uint32(shared.mapping[4:]); got != value+1 {
			t.Fatalf("the guest wrote %d, want %d", got, value+1)
		}
	}

	as.Unmap(sharedVA, hostarch.PageSize)
	as.Unmap(codeVA, hostarch.PageSize)
	as.Release()
	shared.release(t)
}
