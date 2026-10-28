// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package proc

import (
	"bytes"
	"fmt"
	"slices"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/kernfs"
	"gvisor.dev/gvisor/pkg/sentry/inet"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sync"
	netipv6 "gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/usermem"
)

// ipv4ConfDefaults are the settings of /proc/sys/net/ipv4/conf/*, as Linux's
// net/ipv4/devinet.c defaults them.
var ipv4ConfDefaults = map[string]int32{
	"accept_local":                       0,
	"accept_redirects":                   1,
	"accept_source_route":                0,
	"arp_accept":                         0,
	"arp_announce":                       0,
	"arp_filter":                         0,
	"arp_ignore":                         0,
	"arp_notify":                         0,
	"bootp_relay":                        0,
	"disable_policy":                     0,
	"disable_xfrm":                       0,
	"drop_gratuitous_arp":                0,
	"drop_unicast_in_l2_multicast":       0,
	"force_igmp_version":                 0,
	"forwarding":                         0,
	"igmpv2_unsolicited_report_interval": 10000,
	"igmpv3_unsolicited_report_interval": 1000,
	"ignore_routes_with_linkdown":        0,
	"log_martians":                       0,
	"mc_forwarding":                      0,
	"medium_id":                          0,
	"promote_secondaries":                0,
	"proxy_arp":                          0,
	"proxy_arp_pvlan":                    0,
	"route_localnet":                     0,
	"rp_filter":                          0,
	"secure_redirects":                   1,
	"send_redirects":                     1,
	"shared_media":                       1,
	"src_valid_mark":                     0,
	"tag":                                0,
}

// ipv6ConfDefaults are the settings of /proc/sys/net/ipv6/conf/*, as Linux's
// net/ipv6/addrconf.c defaults them.
var ipv6ConfDefaults = map[string]int32{
	"accept_dad":                        1,
	"accept_ra":                         1,
	"accept_ra_defrtr":                  1,
	"accept_ra_from_local":              0,
	"accept_ra_min_hop_limit":           1,
	"accept_ra_min_lft":                 0,
	"accept_ra_mtu":                     1,
	"accept_ra_pinfo":                   1,
	"accept_ra_rt_info_max_plen":        0,
	"accept_ra_rt_info_min_plen":        0,
	"accept_ra_rt_table":                0,
	"accept_redirects":                  1,
	"accept_source_route":               0,
	"addr_gen_mode":                     0,
	"autoconf":                          1,
	"dad_transmits":                     1,
	"disable_ipv6":                      0,
	"disable_policy":                    0,
	"drop_unicast_in_l2_multicast":      0,
	"drop_unsolicited_na":               0,
	"enhanced_dad":                      1,
	"force_mld_version":                 0,
	"force_tllao":                       0,
	"forwarding":                        0,
	"hop_limit":                         64,
	"ignore_routes_with_linkdown":       0,
	"keep_addr_on_down":                 0,
	"max_addresses":                     16,
	"max_desync_factor":                 600,
	"mc_forwarding":                     0,
	"mldv1_unsolicited_report_interval": 10000,
	"mldv2_unsolicited_report_interval": 1000,
	"mtu":                               1280,
	"ndisc_notify":                      0,
	"ndisc_tclass":                      0,
	"optimistic_dad":                    0,
	"proxy_ndp":                         0,
	"regen_max_retry":                   3,
	"router_probe_interval":             60,
	"router_solicitation_delay":         1,
	"router_solicitation_interval":      4,
	"router_solicitation_max_interval":  3600,
	"router_solicitations":              -1,
	"seg6_enabled":                      0,
	"suppress_frag_ndisc":               1,
	"temp_prefered_lft":                 86400,
	"temp_valid_lft":                    604800,
	"use_oif_addrs_only":                0,
	"use_optimistic":                    0,
	"use_tempaddr":                      0,
}

// neighDefaults are the settings of /proc/sys/net/ipv{4,6}/neigh/*, as
// Linux's net/core/neighbour.c defaults them.
var neighDefaults = map[string]int32{
	"app_solicit":            0,
	"base_reachable_time":    30,
	"base_reachable_time_ms": 30000,
	"delay_first_probe_time": 5,
	"gc_stale_time":          60,
	"locktime":               100,
	"mcast_resolicit":        0,
	"mcast_solicit":          3,
	"proxy_delay":            80,
	"proxy_qlen":             64,
	"retrans_time":           100,
	"retrans_time_ms":        1000,
	"ucast_solicit":          3,
	"unres_qlen":             101,
	"unres_qlen_bytes":       212992,
}

// netConfKind is the kind of settings a netConfSet holds.
type netConfKind uint8

const (
	netConfIPv4 netConfKind = iota
	netConfIPv6
	netConfNeigh
)

// netConfDefaults are the settings of each kind, and netConfNames their
// names, in order.
var (
	netConfDefaults = [...]map[string]int32{
		netConfIPv4:  ipv4ConfDefaults,
		netConfIPv6:  ipv6ConfDefaults,
		netConfNeigh: neighDefaults,
	}
	netConfNames = [...][]string{
		netConfIPv4:  sortedNetConfNames(ipv4ConfDefaults),
		netConfIPv6:  sortedNetConfNames(ipv6ConfDefaults),
		netConfNeigh: sortedNetConfNames(neighDefaults),
	}
)

func sortedNetConfNames(defaults map[string]int32) []string {
	names := make([]string, 0, len(defaults))
	for name := range defaults {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// newNetConfDirs returns the conf and neigh directories of /proc/sys/net/ipv4
// or, given ipv6, of /proc/sys/net/ipv6: settings for all interfaces, the
// defaults of new ones, and those of each interface of stack. The sentry
// keeps the settings without effect. Android's netd and IpClient, for two,
// write them, and fail to bring a network up when they cannot.
func (fs *filesystem) newNetConfDirs(ctx context.Context, root *auth.Credentials, stack inet.Stack, ipv6 bool) (conf, neigh kernfs.Inode) {
	kind := netConfIPv4
	if ipv6 {
		kind = netConfIPv6
	}
	all := fs.newNetConfSet(ctx, root, kind, nil)
	defaults := fs.newNetConfSet(ctx, root, kind, nil)
	// The stack keeps some of the settings for all interfaces itself: IPv4's
	// route_localnet, also the default of new interfaces, which has it accept
	// martian loopback packets on other interfaces (kube-proxy enables it),
	// and IPv6's forwarding.
	if ipv6 {
		all.link(ctx, "forwarding", &ipForwarding{stack: stack, protocol: netipv6.ProtocolNumber})
	} else {
		all.link(ctx, "route_localnet", &routeLocalnetData{stack: stack})
		defaults.link(ctx, "route_localnet", &routeLocalnetData{stack: stack})
	}
	confDirs := map[string]kernfs.Inode{
		"all":     all,
		"default": defaults,
	}
	neighDirs := map[string]kernfs.Inode{
		"default": fs.newNetConfSet(ctx, root, netConfNeigh, nil),
	}
	for _, iface := range stack.Interfaces() {
		var overrides map[string]int32
		if ipv6 {
			overrides = map[string]int32{"mtu": int32(iface.MTU)}
			if iface.Flags&linux.IFF_LOOPBACK != 0 {
				overrides["use_tempaddr"] = -1
			}
		}
		confDirs[iface.Name] = fs.newNetConfSet(ctx, root, kind, overrides)
		neighDirs[iface.Name] = fs.newNetConfSet(ctx, root, netConfNeigh, nil)
	}
	return fs.newStaticDir(ctx, root, confDirs), fs.newStaticDir(ctx, root, neighDirs)
}

// netConfSet is a directory of settings: an interface's, all interfaces', or
// the defaults of new ones. It keeps the settings, and makes the file of one
// as it is looked up, which goes once unused: little reads them, and a fork
// copies every inode the kernel has.
//
// +stateify savable
type netConfSet struct {
	kernfs.StaticDirectory

	fs    *filesystem
	owner *auth.Credentials
	kind  netConfKind
	// inos are the inode numbers of the settings' files, and vals the
	// settings, in the order of netConfNames[kind]. vals are protected by
	// valsMu.
	inos   []uint64
	valsMu sync.Mutex `state:"nosave"`
	vals   []int32
	// linked are the files of the settings that the stack keeps, by name:
	// see link.
	linked map[string]kernfs.Inode
}

func (fs *filesystem) newNetConfSet(ctx context.Context, root *auth.Credentials, kind netConfKind, overrides map[string]int32) *netConfSet {
	names := netConfNames[kind]
	set := &netConfSet{
		fs:    fs,
		owner: root,
		kind:  kind,
		inos:  make([]uint64, len(names)),
		vals:  make([]int32, len(names)),
	}
	for i, name := range names {
		val := netConfDefaults[kind][name]
		if v, ok := overrides[name]; ok {
			val = v
		}
		set.vals[i] = val
		set.inos[i] = fs.NextIno()
	}
	set.StaticDirectory.Init(ctx, root, linux.UNNAMED_MAJOR, fs.devMinor, fs.NextIno(), 0555, kernfs.GenericDirectoryFDOptions{
		SeekEnd: kernfs.SeekEndZero,
	})
	set.InitRefs()
	set.OrderedChildren.Init(kernfs.OrderedChildrenOptions{})
	return set
}

// link has the setting name of s kept by f, a file of the stack's own
// setting, instead of s.
func (s *netConfSet) link(ctx context.Context, name string, f dynamicInode) {
	i, ok := slices.BinarySearch(netConfNames[s.kind], name)
	if !ok {
		panic(fmt.Sprintf("no network setting %q", name))
	}
	f.Init(ctx, s.owner, linux.UNNAMED_MAJOR, s.fs.devMinor, s.inos[i], f, 0644)
	if s.linked == nil {
		s.linked = make(map[string]kernfs.Inode)
	}
	s.linked[name] = f
}

// Lookup implements kernfs.inodeDirectory.Lookup.
func (s *netConfSet) Lookup(ctx context.Context, name string) (kernfs.Inode, error) {
	i, ok := slices.BinarySearch(netConfNames[s.kind], name)
	if !ok {
		return nil, linuxerr.ENOENT
	}
	if f, ok := s.linked[name]; ok {
		return f, nil
	}
	mode := linux.FileMode(0644)
	if name == "mc_forwarding" {
		mode = 0444
	}
	f := &netConfFile{set: s, index: i}
	f.Init(ctx, s.owner, linux.UNNAMED_MAJOR, s.fs.devMinor, s.inos[i], f, mode)
	return f, nil
}

// IterDirents implements kernfs.inodeDirectory.IterDirents.
func (s *netConfSet) IterDirents(ctx context.Context, mnt *vfs.Mount, cb vfs.IterDirentsCallback, offset, relOffset int64) (int64, error) {
	names := netConfNames[s.kind]
	for i := max(relOffset, 0); i < int64(len(names)); i++ {
		if err := cb.Handle(vfs.Dirent{
			Name:    names[i],
			Type:    linux.DT_REG,
			Ino:     s.inos[i],
			NextOff: offset + 1,
		}); err != nil {
			return offset, err
		}
		offset++
	}
	return offset, nil
}

// netConfFile is the file of a setting of a netConfSet.
//
// +stateify savable
type netConfFile struct {
	kernfs.DynamicBytesFile

	set   *netConfSet
	index int
}

var _ vfs.WritableDynamicBytesSource = (*netConfFile)(nil)

// Generate implements vfs.DynamicBytesSource.Generate.
func (f *netConfFile) Generate(ctx context.Context, buf *bytes.Buffer) error {
	f.set.valsMu.Lock()
	val := f.set.vals[f.index]
	f.set.valsMu.Unlock()
	_, err := fmt.Fprintf(buf, "%d\n", val)
	return err
}

// Write implements vfs.WritableDynamicBytesSource.Write.
func (f *netConfFile) Write(ctx context.Context, _ *vfs.FileDescription, src usermem.IOSequence, offset int64) (int64, error) {
	if offset != 0 {
		// Ignore partial writes.
		return 0, linuxerr.EINVAL
	}
	buf := make([]int32, 1)
	n, err := ParseInt32Vec(ctx, src, buf)
	if err != nil || n == 0 {
		return 0, err
	}
	f.set.valsMu.Lock()
	f.set.vals[f.index] = buf[0]
	f.set.valsMu.Unlock()
	return n, nil
}
