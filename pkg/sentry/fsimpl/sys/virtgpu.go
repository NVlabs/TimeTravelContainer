// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sys

import (
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/kernfs"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
)

// This file presents the virtual GPU of pkg/sentry/devices/virtgpu as Linux
// presents a virtio-gpu PCI device: a PCI function, the virtio device on it,
// and the DRM render node on that. libdrm's drmGetDevices2() finds the render
// node through /sys/dev/char/226:128, follows its device link to the virtio
// device, and from there to the PCI function for the vendor and device IDs
// that Mesa's drivers look for.
//
// The PCI domain is one that the host trees that sysfs mirrors (see pci.go and
// rdma.go) are unlikely to use.
const (
	virtGPURootComplex = "pci00ff:00"
	virtGPUPCISlot     = "00ff:00:00.0"
	virtGPUVirtio      = "virtio0"
	virtGPURenderNode  = "renderD128"
	virtGPURenderDev   = "226:128"
)

// virtGPUSysfs is the virtual GPU's subtrees, for GetFilesystem to graft into
// /sys.
type virtGPUSysfs struct {
	// devices is the root complex's subtree, for /sys/devices.
	devices kernfs.Inode
	// busPCIDevice and busVirtioDevice are the device's symlinks in
	// /sys/bus/pci/devices and /sys/bus/virtio/devices.
	busPCIDevice    kernfs.Inode
	busVirtioDevice kernfs.Inode
	// classDRM is /sys/class/drm.
	classDRM kernfs.Inode
	// devChar is the render node's symlink in /sys/dev/char.
	devChar kernfs.Inode
}

func (fs *filesystem) newVirtGPUSysfs(ctx context.Context, creds *auth.Credentials) *virtGPUSysfs {
	file := func(data string) kernfs.Inode {
		return fs.newStaticFile(ctx, creds, defaultSysMode, data)
	}
	symlink := func(target string) kernfs.Inode {
		return kernfs.NewStaticSymlink(ctx, creds, linux.UNNAMED_MAJOR, fs.devMinor, fs.NextIno(), target)
	}
	dir := func(contents map[string]kernfs.Inode) kernfs.Inode {
		return fs.newDir(ctx, creds, defaultSysDirMode, contents)
	}
	const pciPath = "devices/" + virtGPURootComplex + "/" + virtGPUPCISlot
	const virtioPath = pciPath + "/" + virtGPUVirtio
	const renderPath = virtioPath + "/drm/" + virtGPURenderNode

	renderNode := dir(map[string]kernfs.Inode{
		"dev":       file(virtGPURenderDev + "\n"),
		"uevent":    file("MAJOR=226\nMINOR=128\nDEVNAME=dri/" + virtGPURenderNode + "\nDEVTYPE=drm_minor\n"),
		"subsystem": symlink("../../../../../../class/drm"),
		"device":    symlink("../../../" + virtGPUVirtio),
	})
	virtio := dir(map[string]kernfs.Inode{
		// VIRTIO_ID_GPU.
		"device":    file("0x0010\n"),
		"vendor":    file("0x1af4\n"),
		"uevent":    file("DRIVER=virtio_gpu\nMODALIAS=virtio:d00000010v00001AF4\n"),
		"subsystem": symlink("../../../../bus/virtio"),
		"drm": dir(map[string]kernfs.Inode{
			virtGPURenderNode: renderNode,
		}),
	})
	// The IDs of QEMU's virtio-gpu-pci, whose class is "other display
	// controller".
	pci := dir(map[string]kernfs.Inode{
		"vendor":           file("0x1af4\n"),
		"device":           file("0x1050\n"),
		"subsystem_vendor": file("0x1af4\n"),
		"subsystem_device": file("0x1100\n"),
		"revision":         file("0x01\n"),
		"class":            file("0x038000\n"),
		"uevent": file("DRIVER=virtio-pci\nPCI_CLASS=38000\nPCI_ID=1AF4:1050\nPCI_SUBSYS_ID=1AF4:1100\n" +
			"PCI_SLOT_NAME=" + virtGPUPCISlot + "\nMODALIAS=pci:v00001AF4d00001050sv00001AF4sd00001100bc03sc80i00\n"),
		"subsystem":   symlink("../../../bus/pci"),
		virtGPUVirtio: virtio,
	})
	return &virtGPUSysfs{
		devices:         dir(map[string]kernfs.Inode{virtGPUPCISlot: pci}),
		busPCIDevice:    symlink("../../../" + pciPath),
		busVirtioDevice: symlink("../../../" + virtioPath),
		classDRM: dir(map[string]kernfs.Inode{
			virtGPURenderNode: symlink("../../" + renderPath),
			"version":         file("drm 1.1.0 20060810\n"),
		}),
		devChar: symlink("../../" + renderPath),
	}
}
