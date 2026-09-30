// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package proc

import (
	"bytes"
	"fmt"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/kernfs"
	"gvisor.dev/gvisor/pkg/sentry/inet"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
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

// newNetConfDirs returns the conf and neigh directories of /proc/sys/net/ipv4
// or, given ipv6, of /proc/sys/net/ipv6: settings for all interfaces, the
// defaults of new ones, and those of each interface of stack. The sentry
// keeps the settings without effect. Android's netd and IpClient, for two,
// write them, and fail to bring a network up when they cannot.
func (fs *filesystem) newNetConfDirs(ctx context.Context, root *auth.Credentials, stack inet.Stack, ipv6 bool) (conf, neigh kernfs.Inode) {
	confDefaults := ipv4ConfDefaults
	if ipv6 {
		confDefaults = ipv6ConfDefaults
	}
	confDirs := map[string]kernfs.Inode{
		"all":     fs.newNetConfSet(ctx, root, confDefaults, nil),
		"default": fs.newNetConfSet(ctx, root, confDefaults, nil),
	}
	neighDirs := map[string]kernfs.Inode{
		"default": fs.newNetConfSet(ctx, root, neighDefaults, nil),
	}
	for _, iface := range stack.Interfaces() {
		var overrides map[string]int32
		if ipv6 {
			overrides = map[string]int32{"mtu": int32(iface.MTU)}
			if iface.Flags&linux.IFF_LOOPBACK != 0 {
				overrides["use_tempaddr"] = -1
			}
		}
		confDirs[iface.Name] = fs.newNetConfSet(ctx, root, confDefaults, overrides)
		neighDirs[iface.Name] = fs.newNetConfSet(ctx, root, neighDefaults, nil)
	}
	return fs.newStaticDir(ctx, root, confDirs), fs.newStaticDir(ctx, root, neighDirs)
}

func (fs *filesystem) newNetConfSet(ctx context.Context, root *auth.Credentials, defaults, overrides map[string]int32) kernfs.Inode {
	files := make(map[string]kernfs.Inode, len(defaults))
	for key, val := range defaults {
		if v, ok := overrides[key]; ok {
			val = v
		}
		f := &netConfFile{}
		f.val.Store(val)
		mode := linux.FileMode(0644)
		if key == "mc_forwarding" {
			mode = 0444
		}
		files[key] = fs.newInode(ctx, root, mode, f)
	}
	return fs.newStaticDir(ctx, root, files)
}

// netConfFile is a setting of an interface.
//
// +stateify savable
type netConfFile struct {
	kernfs.DynamicBytesFile

	val atomicbitops.Int32
}

var _ vfs.WritableDynamicBytesSource = (*netConfFile)(nil)

// Generate implements vfs.DynamicBytesSource.Generate.
func (f *netConfFile) Generate(ctx context.Context, buf *bytes.Buffer) error {
	_, err := fmt.Fprintf(buf, "%d\n", f.val.Load())
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
	f.val.Store(buf[0])
	return n, nil
}
