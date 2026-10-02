// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sys

import (
	"bytes"
	"fmt"
	"strings"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/kernfs"
	"gvisor.dev/gvisor/pkg/sentry/inet"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
)

// netDeviceAttrs are the attributes of each network device directory, as
// Linux's net/core/net-sysfs.c names them.
var netDeviceAttrs = []string{
	"addr_len",
	"address",
	"broadcast",
	"carrier",
	"dev_id",
	"dev_port",
	"dormant",
	"flags",
	"ifalias",
	"ifindex",
	"iflink",
	"link_mode",
	"mtu",
	"netdev_group",
	"operstate",
	"proto_down",
	"tx_queue_len",
	"type",
	"uevent",
}

// newNetDirs returns the contents of /sys/devices/virtual/net, a directory
// for each network interface, and of /sys/class/net, symlinks to them, as
// Linux has for virtual devices. The interfaces are those of the root network
// namespace when sysfs is mounted; the attributes are read from the stack.
// Android's netd, for one, lists interfaces in /sys/class/net.
func (fs *filesystem) newNetDirs(ctx context.Context, creds *auth.Credentials) (devices, class map[string]kernfs.Inode) {
	devices = make(map[string]kernfs.Inode)
	class = make(map[string]kernfs.Inode)
	stack := rootNetStack(ctx)
	if stack == nil {
		return devices, class
	}
	for _, iface := range stack.Interfaces() {
		name := iface.Name
		attrs := make(map[string]kernfs.Inode, len(netDeviceAttrs)+1)
		for _, attr := range netDeviceAttrs {
			f := &netDeviceFile{name: name, attr: attr}
			f.DynamicBytesFile.Init(ctx, creds, linux.UNNAMED_MAJOR, fs.devMinor, fs.NextIno(), f, defaultSysMode)
			attrs[attr] = f
		}
		attrs["subsystem"] = kernfs.NewStaticSymlink(ctx, creds, linux.UNNAMED_MAJOR, fs.devMinor, fs.NextIno(), "../../../../class/net")
		devices[name] = fs.newDir(ctx, creds, defaultSysDirMode, attrs)
		class[name] = kernfs.NewStaticSymlink(ctx, creds, linux.UNNAMED_MAJOR, fs.devMinor, fs.NextIno(), "../../devices/virtual/net/"+name)
	}
	return devices, class
}

// rootNetStack returns the stack of the root network namespace, if any.
func rootNetStack(ctx context.Context) inet.Stack {
	k := kernel.KernelFromContext(ctx)
	if k == nil || k.RootNetworkNamespace() == nil {
		return nil
	}
	return k.RootNetworkNamespace().Stack()
}

// netDeviceFile is an attribute of a network device.
//
// +stateify savable
type netDeviceFile struct {
	implStatFS
	kernfs.DynamicBytesFile

	name string
	attr string
}

// Generate implements vfs.DynamicBytesSource.Generate.
func (f *netDeviceFile) Generate(ctx context.Context, buf *bytes.Buffer) error {
	stack := rootNetStack(ctx)
	if stack == nil {
		return linuxerr.ENODEV
	}
	for idx, iface := range stack.Interfaces() {
		if iface.Name == f.name {
			return f.generate(buf, idx, &iface)
		}
	}
	return linuxerr.ENODEV
}

func (f *netDeviceFile) generate(buf *bytes.Buffer, idx int32, iface *inet.Interface) error {
	running := iface.Flags&linux.IFF_RUNNING != 0
	switch f.attr {
	case "addr_len":
		fmt.Fprintf(buf, "%d\n", len(hardwareAddr(iface)))
	case "address":
		fmt.Fprintf(buf, "%s\n", formatHardwareAddr(hardwareAddr(iface)))
	case "broadcast":
		brd := make([]byte, len(hardwareAddr(iface)))
		if iface.Flags&linux.IFF_LOOPBACK == 0 {
			for i := range brd {
				brd[i] = 0xff
			}
		}
		fmt.Fprintf(buf, "%s\n", formatHardwareAddr(brd))
	case "carrier":
		// Linux reports no carrier for a device that is down.
		if !running {
			return linuxerr.EINVAL
		}
		buf.WriteString("1\n")
	case "dev_id":
		buf.WriteString("0x0\n")
	case "flags":
		// The flags the device was given, less those Linux derives from
		// its state (see net/core/dev.c:dev_get_flags()).
		const derived = linux.IFF_RUNNING | linux.IFF_LOWER_UP | linux.IFF_DORMANT
		fmt.Fprintf(buf, "0x%x\n", iface.Flags&^derived)
	case "ifalias":
		buf.WriteString("\n")
	case "ifindex", "iflink":
		fmt.Fprintf(buf, "%d\n", idx)
	case "mtu":
		fmt.Fprintf(buf, "%d\n", iface.MTU)
	case "operstate":
		switch {
		case iface.Flags&linux.IFF_LOOPBACK != 0:
			buf.WriteString("unknown\n")
		case running:
			buf.WriteString("up\n")
		default:
			buf.WriteString("down\n")
		}
	case "tx_queue_len":
		buf.WriteString("1000\n")
	case "type":
		fmt.Fprintf(buf, "%d\n", iface.DeviceType)
	case "uevent":
		fmt.Fprintf(buf, "INTERFACE=%s\nIFINDEX=%d\n", iface.Name, idx)
	default: // dev_port, dormant, link_mode, netdev_group, proto_down
		buf.WriteString("0\n")
	}
	return nil
}

// hardwareAddr returns an interface's hardware address, which is all zeros
// for a device, such as loopback, that has none.
func hardwareAddr(iface *inet.Interface) []byte {
	if len(iface.Addr) == 0 {
		return make([]byte, 6)
	}
	return iface.Addr
}

func formatHardwareAddr(addr []byte) string {
	parts := make([]string, len(addr))
	for i, b := range addr {
		parts[i] = fmt.Sprintf("%02x", b)
	}
	return strings.Join(parts, ":")
}
