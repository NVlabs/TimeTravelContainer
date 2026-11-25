// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package cmd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer"
	"gvisor.dev/gvisor/runsc/ttc/seatbelt"
)

func init() {
	// No test confines the test process itself; TestDaemonConfinesItself
	// checks what a daemon applies.
	applySeatbelt = func(string) error { return nil }
	seatbeltConfined = func() bool { return false }
	// Nor does a test start the GPU renderers' launcher, which is the
	// daemon's own executable: a test's is the test binary, which would run
	// the tests again.
	startGFXLauncher = func(gfxrenderer.BrokerOptions) (*gfxrenderer.Launcher, error) {
		return nil, errors.New("tests start no GPU renderers' launcher")
	}
}

func TestDaemonConfinesItself(t *testing.T) {
	originalApply, originalConfined, originalLauncher := applySeatbelt, seatbeltConfined, startGFXLauncher
	t.Cleanup(func() {
		applySeatbelt, seatbeltConfined, startGFXLauncher = originalApply, originalConfined, originalLauncher
	})
	var applied []string
	applySeatbelt = func(profile string) error {
		applied = append(applied, profile)
		return nil
	}
	if err := enterDaemonHostIsolation(true, false); err != nil || len(applied) != 0 {
		t.Fatalf("daemon without host isolation: err=%v, applied %d profiles, want none", err, len(applied))
	}
	if err := enterDaemonHostIsolation(false, false); err != nil || len(applied) != 1 || applied[0] != seatbelt.DaemonProfile {
		t.Fatalf("daemon: err=%v, applied %d profiles, want seatbelt.DaemonProfile", err, len(applied))
	}
	// A control holder that its gateway confined applies nothing: profiles do
	// not stack. One that runs unconfined confines itself, as does a daemon
	// that is no control holder.
	seatbeltConfined = func() bool { return true }
	applied = nil
	if err := enterDaemonHostIsolation(false, true); err != nil || len(applied) != 0 {
		t.Fatalf("confined control holder: err=%v, applied %d profiles, want none", err, len(applied))
	}
	if err := enterDaemonHostIsolation(false, false); err != nil || len(applied) != 1 || applied[0] != seatbelt.DaemonProfile {
		t.Fatalf("confined daemon: err=%v, applied %d profiles, want seatbelt.DaemonProfile", err, len(applied))
	}
	seatbeltConfined = func() bool { return false }
	applied = nil
	if err := enterDaemonHostIsolation(false, true); err != nil || len(applied) != 1 || applied[0] != seatbelt.DaemonProfile {
		t.Fatalf("unconfined control holder: err=%v, applied %d profiles, want seatbelt.DaemonProfile", err, len(applied))
	}
	if got := daemonIsolationMode(false); got != "seatbelt" {
		t.Errorf("daemonIsolationMode(false) = %q, want seatbelt", got)
	}
	// runsc has no host isolation of its own on macOS: the daemon's confines
	// it.
	server := &daemonServer{root: "/r"}
	if opts := server.optionsFromRequest(); !opts.unsafeNoHostIsolation {
		t.Error("a daemon with host isolation runs runsc with isolation of runsc's own, which macOS lacks")
	}

	// A daemon that cannot confine itself serves nothing, and touches no root.
	// It starts the GPU renderers' launcher first, which the confinement
	// would bar from the GPU.
	var steps []string
	startGFXLauncher = func(gfxrenderer.BrokerOptions) (*gfxrenderer.Launcher, error) {
		steps = append(steps, "launcher")
		return nil, errors.New("no launcher")
	}
	applySeatbelt = func(string) error {
		steps = append(steps, "seatbelt")
		return errors.New("Operation not permitted")
	}
	root := shortDaemonTestRoot(t)
	t.Setenv("TTC_ROOT", root)
	t.Setenv("TTC_LOCAL_ROOT", "")
	var stdout, stderr bytes.Buffer
	if status := RunDaemon(nil, &stdout, &stderr); status != 1 || !strings.Contains(stderr.String(), "--unsafe-no-host-isolation") {
		t.Fatalf("daemon that cannot confine itself: status=%d stderr=%q", status, stderr.String())
	}
	if want := []string{"launcher", "seatbelt"}; !slices.Equal(steps, want) {
		t.Errorf("daemon took steps %q, want %q", steps, want)
	}
	if _, err := os.Stat(filepath.Join(root, ".ttc-root-format")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("daemon that cannot confine itself prepared its root: %v", err)
	}
}

func TestDarwinHostCompatibilityDoesNotApplyLinuxGSOFallback(t *testing.T) {
	compatibility, err := evaluateCurrentHostCompatibility("24.6.0")
	if err != nil {
		t.Fatalf("evaluateCurrentHostCompatibility: %v", err)
	}
	if compatibility.disableHostGSO {
		t.Fatal("Darwin compatibility unexpectedly enabled the Linux host-GSO fallback")
	}
}

func TestDarwinHypervisorEntitlement(t *testing.T) {
	const entitlement = "com.apple.security.hypervisor"
	for _, test := range []struct {
		name  string
		plist string
		want  bool
	}{
		{
			name:  "present",
			plist: `<?xml version="1.0"?><plist><dict><key>com.apple.security.hypervisor</key><true/></dict></plist>`,
			want:  true,
		},
		{
			name:  "false",
			plist: `<plist><dict><key>com.apple.security.hypervisor</key><false/></dict></plist>`,
		},
		{
			name:  "unrelated true",
			plist: `<plist><dict><key>com.apple.security.hypervisor</key><false/><key>other</key><true/></dict></plist>`,
		},
		{
			name:  "missing",
			plist: `<plist><dict><key>other</key><true/></dict></plist>`,
		},
		{
			name:  "malformed",
			plist: `<plist><dict><key>com.apple.security.hypervisor</key>`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := hasDarwinBooleanEntitlement([]byte(test.plist), entitlement); got != test.want {
				t.Fatalf("hasDarwinBooleanEntitlement(%q) = %t, want %t", test.plist, got, test.want)
			}
		})
	}
}
