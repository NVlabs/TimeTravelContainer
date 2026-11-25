// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package seatbelt

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// testProfile admits files, but not the network.
const testProfile = `(version 1)
(deny default)
(import "system.sb")
(allow mach-bootstrap)
(allow syscall*)
(allow file*)
(allow process-fork)
(allow process-exec)`

const (
	helperProfileEnv = "SEATBELT_TEST_PROFILE"
	helperDirEnv     = "SEATBELT_TEST_DIR"
	helperAddressEnv = "SEATBELT_TEST_ADDRESS"
)

// deniedServices are commands that use services of the system DaemonProfile
// denies: Disk Arbitration, and power management.
var deniedServices = [][]string{
	{"/usr/sbin/diskutil", "list"},
	{"/usr/bin/caffeinate", "-i", "-t", "1"},
}

// TestHelperProcess is the process the other tests confine.
func TestHelperProcess(t *testing.T) {
	dir, address := os.Getenv(helperDirEnv), os.Getenv(helperAddressEnv)
	if dir == "" {
		t.Skip("run by the tests that confine it")
	}
	fail := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
		os.Exit(1)
	}
	if os.Getenv(helperProfileEnv) == "renderer" {
		// The file is admitted by its path through a symbolic link, and by
		// its own, as the renderer admits its executable.
		linked := filepath.Join(dir, "link", "readable")
		readable := filepath.Join(dir, "real", "readable")
		cache := filepath.Join(dir, "cache")
		profile, err := RendererProfile([]string{linked, readable}, []string{cache})
		if err != nil {
			fail("RendererProfile: %v", err)
		}
		if err := Apply(profile); err != nil {
			fail("Apply(RendererProfile): %v", err)
		}
		if data, err := os.ReadFile(linked); err != nil || string(data) != "readable" {
			fail("reading a file the profile admits: %q, %v", data, err)
		}
		// Resolving the path reads the link: CoreFoundation does, for the
		// executable's.
		if resolved, err := filepath.EvalSymlinks(linked); err != nil || resolved != readable {
			fail("resolving the path of a file the profile admits: %q, %v", resolved, err)
		}
		if _, err := os.ReadFile(filepath.Join(dir, "secret")); !errors.Is(err, syscall.EPERM) {
			fail("reading a file the profile does not admit: %v, want EPERM", err)
		}
		if _, err := os.ReadDir(filepath.Join(dir, "real")); err == nil {
			fail("listed the directory of a file the profile admits")
		}
		if err := os.WriteFile(filepath.Join(dir, "written"), []byte("x"), 0600); err == nil {
			fail("wrote a file, which the profile denies")
		}
		// The cache, which the profile admits, but no sibling of it.
		cached := filepath.Join(cache, "com.apple.metal", "cached")
		if err := os.MkdirAll(filepath.Dir(cached), 0700); err != nil {
			fail("making a directory of the cache: %v", err)
		}
		if err := os.WriteFile(cached, []byte("cached"), 0600); err != nil {
			fail("writing a file of the cache: %v", err)
		}
		if data, err := os.ReadFile(cached); err != nil || string(data) != "cached" {
			fail("reading a file of the cache: %q, %v", data, err)
		}
		if err := os.WriteFile(cache+"-sibling", []byte("x"), 0600); err == nil {
			fail("wrote a sibling of the cache, which the profile denies")
		}
		if conn, err := net.Dial("tcp", address); err == nil {
			conn.Close()
			fail("connected to %s, which the profile denies", address)
		}
		if err := exec.Command("/usr/bin/true").Run(); err == nil {
			fail("started a process, which the profile denies")
		}
		os.Exit(0)
	}
	if os.Getenv(helperProfileEnv) == "inherited" {
		if !Confined() {
			fail("a process that a confined one started is not confined")
		}
		os.Exit(0)
	}
	if os.Getenv(helperProfileEnv) == "daemon" {
		if Confined() {
			fail("confined before Apply(DaemonProfile)")
		}
		if err := Apply(DaemonProfile); err != nil {
			fail("Apply(DaemonProfile): %v", err)
		}
		if !Confined() {
			fail("not confined after Apply(DaemonProfile)")
		}
		// A process that a confined daemon starts, such as a gateway's
		// control holder, is confined too, and can tell.
		inherited := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
		inherited.Env = append(os.Environ(), helperProfileEnv+"=inherited")
		if output, err := inherited.CombinedOutput(); err != nil {
			fail("a process that the confined one started: %v: %s", err, output)
		}
		if err := os.WriteFile(filepath.Join(dir, "written"), []byte("confined"), 0600); err != nil {
			fail("writing a file: %v", err)
		}
		conn, err := net.Dial("tcp", address)
		if err != nil {
			fail("connecting to %s: %v", address, err)
		}
		conn.Close()
		if output, err := exec.Command("/bin/sh", "-c", `echo child >"$0"`, filepath.Join(dir, "child")).CombinedOutput(); err != nil {
			fail("running a child: %v: %s", err, output)
		}
		for _, command := range deniedServices {
			if err := exec.Command(command[0], command[1:]...).Run(); err == nil {
				fail("%q succeeded, though its service is denied", command)
			}
		}
		os.Exit(0)
	}
	if err := Apply(`(version 1) (allow no-such-operation)`); err == nil || !strings.Contains(err.Error(), "no-such-operation") {
		fail("Apply(invalid profile) = %v, want an error naming the operation", err)
	}
	if err := Apply(testProfile); err != nil {
		fail("Apply: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "written"), []byte("confined"), 0600); err != nil {
		fail("writing a file the profile admits: %v", err)
	}
	if conn, err := net.Dial("tcp", address); err == nil {
		conn.Close()
		fail("connected to %s, which the profile denies", address)
	} else if !errors.Is(err, syscall.EPERM) {
		fail("connecting to %s: %v, want EPERM", address, err)
	}
	host, port, _ := net.SplitHostPort(address)
	if err := exec.Command("/usr/bin/nc", "-z", "-w", "2", host, port).Run(); err == nil {
		fail("a child connected to %s, which the profile denies it too", address)
	}
	os.Exit(0)
}

func TestApplyConfinesProcessAndChildren(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	address := listener.Addr().String()
	host, port, _ := net.SplitHostPort(address)
	if output, err := exec.Command("/usr/bin/nc", "-z", "-w", "2", host, port).CombinedOutput(); err != nil {
		t.Fatalf("unconfined nc cannot connect to %s: %v: %s", address, err, output)
	}

	dir := t.TempDir()
	child := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	child.Env = append(os.Environ(), helperDirEnv+"="+dir, helperAddressEnv+"="+address)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("confined process: %v\n%s", err, output)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "written")); err != nil || string(data) != "confined" {
		t.Fatalf("file the confined process wrote: %q, %v", data, err)
	}
	// The confinement is the child's alone.
	if conn, err := net.Dial("tcp", address); err != nil {
		t.Fatalf("the test process was confined too: %v", err)
	} else {
		conn.Close()
	}
}

func TestDaemonProfile(t *testing.T) {
	for _, command := range deniedServices {
		if output, err := exec.Command(command[0], command[1:]...).CombinedOutput(); err != nil {
			t.Skipf("%q fails unconfined: %v: %s", command, err, output)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	dir := t.TempDir()
	child := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	child.Env = append(os.Environ(), helperProfileEnv+"=daemon", helperDirEnv+"="+dir, helperAddressEnv+"="+listener.Addr().String())
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("process confined to DaemonProfile: %v\n%s", err, output)
	}
	for _, name := range []string{"written", "child"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("file the confined process was to write: %v", err)
		}
	}
}

func TestRendererProfile(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	// Resolve the directory, as the renderer resolves its files: macOS's
	// temporary directories are under a symbolic link, /var.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "real"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"real/readable", "secret"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(filepath.Base(name)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	child := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	child.Env = append(os.Environ(), helperProfileEnv+"=renderer", helperDirEnv+"="+dir, helperAddressEnv+"="+listener.Addr().String())
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("process confined to RendererProfile: %v\n%s", err, output)
	}
}

func TestRendererProfileQuotes(t *testing.T) {
	profile, err := RendererProfile([]string{`/a "b" \c`}, []string{`/d "e"`})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`(literal "/a \"b\" \\c")`, `(subpath "/d \"e\"")`} {
		if !strings.Contains(profile, want) {
			t.Errorf("profile has no %s:\n%s", want, profile)
		}
	}
	if _, err := RendererProfile([]string{"/a\nb"}, nil); err == nil {
		t.Error("a path with a newline was accepted")
	}
	if _, err := RendererProfile(nil, []string{"/a\nb"}); err == nil {
		t.Error("a cache with a newline was accepted")
	}
}

// TestBundleCacheDir finds no cache directory of the test, whose binary
// embeds no Info.plist, and so has no bundle identifier.
func TestBundleCacheDir(t *testing.T) {
	if dir, err := BundleCacheDir(); dir != "" || err != nil {
		t.Errorf("BundleCacheDir() = %q, %v, want \"\", nil", dir, err)
	}
}
