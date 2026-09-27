// Copyright 2026 The gVisor Authors.
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
	if os.Getenv(helperProfileEnv) == "daemon" {
		// A daemon started by a confined one confines itself again.
		for i := 0; i < 2; i++ {
			if err := Apply(DaemonProfile); err != nil {
				fail("Apply(DaemonProfile): %v", err)
			}
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
