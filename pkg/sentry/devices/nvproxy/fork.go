// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvproxy

import (
	"errors"
	"fmt"

	"gvisor.dev/gvisor/pkg/abi/nvgpu"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/mm"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

// ForkPolicy is what a fork, or another save, does with NVIDIA GPUs that are
// in use.
type ForkPolicy int

const (
	// ForkIneligible refuses live forks while the GPUs are in use. Other
	// saves copy the GPUs' state only from processes that cuda-checkpoint
	// checkpointed, as gVisor's checkpoints do.
	ForkIneligible ForkPolicy = iota

	// ForkDeviceLost forks, and the copy loses the GPUs' state, as a
	// process does when its GPU falls off the bus: its files of the driver
	// fail every call, and its mappings of the GPUs' memory raise SIGBUS.
	// What it opens anew works.
	ForkDeviceLost

	// ForkSnapshot forks the GPUs' state, which the processes that use them
	// hold in their own memory while cuda-checkpoint has them checkpointed.
	ForkSnapshot
)

// ParseForkPolicy parses a ForkPolicy as runsc's --nvproxy-fork spells it.
func ParseForkPolicy(s string) (ForkPolicy, error) {
	switch s {
	case "", "ineligible":
		return ForkIneligible, nil
	case "device-lost":
		return ForkDeviceLost, nil
	case "snapshot":
		return ForkSnapshot, nil
	default:
		return 0, fmt.Errorf("unknown NVIDIA GPU fork policy %q: want ineligible, device-lost, or snapshot", s)
	}
}

// String returns the policy's name, as --nvproxy-fork spells it.
func (p ForkPolicy) String() string {
	switch p {
	case ForkIneligible:
		return "ineligible"
	case ForkDeviceLost:
		return "device-lost"
	case ForkSnapshot:
		return "snapshot"
	default:
		return fmt.Sprintf("ForkPolicy(%d)", int(p))
	}
}

var errForkIneligible = errors.New("NVIDIA GPUs that are in use refuse live forks unless the family says what a fork does with them")

// validateLiveFork returns an error unless a live fork may copy the files and
// mappings of nvp's devices.
func (nvp *nvproxy) validateLiveFork() error {
	if nvp.forkPolicy == ForkIneligible {
		return errForkIneligible
	}
	return nil
}

var (
	_ vfs.LiveForkFileDescription     = (*frontendFD)(nil)
	_ vfs.LiveForkFileDescription     = (*uvmFD)(nil)
	_ vfs.LiveForkFileDescription     = (*uvmToolsFD)(nil)
	_ memmap.LiveForkMappingValidator = (*frontendFD)(nil)
	_ memmap.LiveForkMappingValidator = (*uvmFD)(nil)
	_ memmap.LiveForkUnsharedFile     = (*frontendFDMemmapFile)(nil)
	_ memmap.LiveForkUnsharedFile     = (*uvmFDMemmapFile)(nil)
	_ vfs.DeviceSaveRestoreExtension  = (*uvmDevice)(nil)
)

// ValidateLiveFork implements vfs.LiveForkFileDescription.ValidateLiveFork.
func (fd *frontendFD) ValidateLiveFork() ([]*vfs.FileDescription, error) {
	return nil, fd.dev.nvp.validateLiveFork()
}

// ValidateLiveFork implements vfs.LiveForkFileDescription.ValidateLiveFork.
func (fd *uvmFD) ValidateLiveFork() ([]*vfs.FileDescription, error) {
	return nil, fd.dev.nvp.validateLiveFork()
}

// ValidateLiveFork implements vfs.LiveForkFileDescription.ValidateLiveFork.
func (fd *uvmToolsFD) ValidateLiveFork() ([]*vfs.FileDescription, error) {
	return nil, fd.dev.nvp.validateLiveFork()
}

// ValidateLiveForkMapping implements
// memmap.LiveForkMappingValidator.ValidateLiveForkMapping.
func (fd *frontendFD) ValidateLiveForkMapping() error {
	return fd.dev.nvp.validateLiveFork()
}

// ValidateLiveForkMapping implements
// memmap.LiveForkMappingValidator.ValidateLiveForkMapping.
func (fd *uvmFD) ValidateLiveForkMapping() error {
	return fd.dev.nvp.validateLiveFork()
}

// LiveForkUnshared implements memmap.LiveForkUnsharedFile.LiveForkUnshared.
// The memory is the GPU's, or the driver's, for the source's channels.
func (*frontendFDMemmapFile) LiveForkUnshared() {}

// LiveForkUnshared implements memmap.LiveForkUnsharedFile.LiveForkUnshared.
func (*uvmFDMemmapFile) LiveForkUnshared() {}

// PrepareSave implements vfs.DeviceSaveRestoreExtension.PrepareSave. With
// ForkDeviceLost, what loads the save loses the GPUs' state, and the save
// leaves out the driver's objects, which are the source's. Otherwise, the
// load re-creates the objects, which it can do only for those that processes
// keep while cuda-checkpoint has them checkpointed: the save fails if others
// remain.
func (dev *uvmDevice) PrepareSave(ctx context.Context) error {
	nvp := dev.nvp
	nvp.loseOnLoad = nvp.forkPolicy == ForkDeviceLost
	if nvp.loseOnLoad {
		nvp.stashClients()
		return nil
	}
	// The sandbox is paused, which no lock would make safer: see beforeSave.
	for _, client := range nvp.clients {
		for _, obj := range client.resources {
			if _, ok := obj.impl.(restorableObjectImpl); !ok {
				return fmt.Errorf("the NVIDIA driver's object %v:%v (class %v) cannot be re-created: processes that use the GPUs must be checkpointed with cuda-checkpoint first", client.handle, obj.handle, obj.class)
			}
		}
	}
	return nil
}

// BeforeResume implements vfs.DeviceSaveRestoreExtension.BeforeResume.
func (dev *uvmDevice) BeforeResume(ctx context.Context) {
	dev.nvp.loseOnLoad = false
	dev.nvp.unstashClients()
}

// stashClients takes the driver's objects, nvp's clients and those of its
// files, out of what a save saves, until unstashClients puts them back. The
// sandbox is paused, so no call of the driver uses them meanwhile.
func (nvp *nvproxy) stashClients() {
	if nvp.stashed {
		return
	}
	nvp.stashed = true
	nvp.stashedClients = nvp.clients
	nvp.clients = make(map[nvgpu.Handle]*rootClient)
	nvp.fdsMu.Lock()
	defer nvp.fdsMu.Unlock()
	for fd := range nvp.frontendFDs {
		fd.stashedClients = fd.clients
		fd.clients = nil
	}
}

// unstashClients undoes stashClients.
func (nvp *nvproxy) unstashClients() {
	if !nvp.stashed {
		return
	}
	nvp.stashed = false
	nvp.clients = nvp.stashedClients
	nvp.stashedClients = nil
	nvp.fdsMu.Lock()
	defer nvp.fdsMu.Unlock()
	for fd := range nvp.frontendFDs {
		fd.clients = fd.stashedClients
		fd.stashedClients = nil
	}
}

// pinForDMA records that the GPUs' DMA uses the memory of prs, which a live
// fork's source then keeps (see pgalloc.MemoryFile.PinForDMA).
func pinForDMA(prs []mm.PinnedRange) {
	for _, pr := range prs {
		if mf, ok := pr.File.(*pgalloc.MemoryFile); ok {
			mf.PinForDMA(pr.FileRange())
		}
	}
}

// unpinForDMA undoes pinForDMA(prs).
func unpinForDMA(prs []mm.PinnedRange) {
	for _, pr := range prs {
		if mf, ok := pr.File.(*pgalloc.MemoryFile); ok {
			mf.UnpinForDMA(pr.FileRange())
		}
	}
}

// lose loses the GPU state that nvp's files hold, as their load does with
// ForkDeviceLost: the objects of the driver that they created are the
// source's.
func (nvp *nvproxy) lose() {
	nvp.clients = make(map[nvgpu.Handle]*rootClient)
	nvp.fdsMu.Lock()
	defer nvp.fdsMu.Unlock()
	for fd := range nvp.frontendFDs {
		fd.lose()
	}
}

// lose makes fd lost: its calls fail, and its mappings raise SIGBUS.
func (fd *frontendFD) lose() {
	fd.lost = true
	fd.hostFD = -1
	fd.memmapFile.hostFD = -1
	fd.clients = nil
}

// lose makes fd lost: its calls fail, and its mappings raise SIGBUS.
func (fd *uvmFD) lose() {
	fd.lost = true
	fd.hostFD = -1
	fd.memmapFile.SetFD(-1)
}

// lose makes fd lost: its calls fail. The host driver's event trackers, whose
// buffers it pinned, are the source's.
func (fd *uvmToolsFD) lose() {
	fd.lost = true
	fd.hostFD = -1
}
