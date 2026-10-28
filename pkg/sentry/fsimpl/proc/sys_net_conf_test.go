// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package proc

import (
	"bytes"
	"slices"
	"testing"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/sentry/contexttest"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/testutil"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/usermem"
)

// A directory of settings makes the file of one as it is looked up, and
// keeps what a file writes for the next.
func TestNetConfSet(t *testing.T) {
	ctx := contexttest.Context(t)
	fs := &filesystem{}
	set := fs.newNetConfSet(ctx, auth.CredentialsFromContext(ctx), netConfIPv6, map[string]int32{"mtu": 9000})
	lookup := func(name string) *netConfFile {
		t.Helper()
		inode, err := set.Lookup(ctx, name)
		if err != nil {
			t.Fatalf("Lookup(%q): %v", name, err)
		}
		return inode.(*netConfFile)
	}
	read := func(name string) string {
		t.Helper()
		var buf bytes.Buffer
		if err := lookup(name).Generate(ctx, &buf); err != nil {
			t.Fatalf("reading %q: %v", name, err)
		}
		return buf.String()
	}

	if got := read("mtu"); got != "9000\n" {
		t.Errorf("mtu = %q, want the interface's", got)
	}
	if got := read("hop_limit"); got != "64\n" {
		t.Errorf("hop_limit = %q, want Linux's default", got)
	}
	if _, err := lookup("hop_limit").Write(ctx, nil, usermem.BytesIOSequence([]byte("32\n")), 0); err != nil {
		t.Fatalf("writing hop_limit: %v", err)
	}
	if got := read("hop_limit"); got != "32\n" {
		t.Errorf("hop_limit after a write = %q, want 32", got)
	}
	if _, err := set.Lookup(ctx, "forwardin"); !linuxerr.Equals(linuxerr.ENOENT, err) {
		t.Errorf("Lookup of no setting = %v, want ENOENT", err)
	}
	if got := lookup("mc_forwarding").Mode().Permissions(); got != 0444 {
		t.Errorf("mc_forwarding's permissions = %o, want 0444", got)
	}

	names := netConfNames[netConfIPv6]
	if !slices.IsSorted(names) || len(names) != len(ipv6ConfDefaults) {
		t.Fatalf("names = %v, want each setting once, in order", names)
	}
	var all testutil.DirentCollector
	if _, err := set.IterDirents(ctx, nil, &all, 2, 0); err != nil {
		t.Fatal(err)
	}
	listed := all.OrderedDirents()
	if len(listed) != len(names) {
		t.Fatalf("listed %d settings, want %d", len(listed), len(names))
	}
	for i, d := range listed {
		if d.Name != names[i] || d.Type != linux.DT_REG || d.NextOff != int64(i)+3 || d.Ino != lookup(d.Name).Ino() {
			t.Errorf("entry %d = %+v, want %q, a regular file numbered as its file is, next at %d", i, *d, names[i], i+3)
		}
	}
	// A listing resumed partway goes on from there.
	var rest testutil.DirentCollector
	if _, err := set.IterDirents(ctx, nil, &rest, 12, 10); err != nil {
		t.Fatal(err)
	}
	if resumed := rest.OrderedDirents(); len(resumed) != len(names)-10 || resumed[0].Name != names[10] || resumed[0].NextOff != 13 {
		t.Errorf("a listing resumed at the 11th setting began with %+v, %d entries", *resumed[0], len(resumed))
	}
}
