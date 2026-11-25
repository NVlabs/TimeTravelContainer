// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package cmd

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/runsc/ttc/seatbelt"
)

const defaultRuntimePlatform = "hvf"

// runtimeDiagnosticsPerContainer is whether a container's Sentry writes its
// panics to a log of the container's own, in which a failed command finds the
// HVF engine's errors; see withRuntimeDiagnostics.
const runtimeDiagnosticsPerContainer = true

func runtimePlatformFlagUsage() string { return "hvf" }

func runtimePlatformCompletions() []string { return []string{"hvf"} }

func normalizeHostRuntimePlatform(value string) (string, error) {
	if value == "hvf" {
		return value, nil
	}
	return "", fmt.Errorf("unsupported runtime platform %q on macOS; supported platform is hvf", value)
}

func preflightDaemonPlatform(runtimePlatform string) error {
	if runtimePlatform != "hvf" {
		return fmt.Errorf("native macOS execution requires platform hvf")
	}
	productVersion, err := unix.Sysctl("kern.osproductversion")
	if err != nil {
		return fmt.Errorf("reading macOS product version: %w", err)
	}
	majorText := strings.SplitN(productVersion, ".", 2)[0]
	major, err := strconv.Atoi(majorText)
	if err != nil || major < 15 {
		return fmt.Errorf("native TTC requires macOS 15.0 or newer, found %q", productVersion)
	}
	return nil
}

func evaluateCurrentHostCompatibility(release string) (hostKernelCompatibility, error) {
	version, err := parseHostKernelVersion(release)
	if err != nil {
		return hostKernelCompatibility{}, err
	}
	// disableHostGSO is a Linux-kernel compatibility workaround for hosts older
	// than 4.14.77. Applying it unconditionally on Darwin changes Sentry's
	// networking execution path even for --network=none and, on the HVF engine,
	// can terminate the sandbox while starting an exec process. Darwin's
	// userspace netstack/NAT selects its own supported offloads; leave the
	// generic runsc default intact here.
	return hostKernelCompatibility{release: release, version: version}, nil
}

func currentHostCompatibilityWarning(hostKernelCompatibility) string { return "" }

// daemonHostIsolationMode names the host isolation of a daemon started without
// --unsafe-no-host-isolation: the macOS sandbox, Seatbelt, which confines the
// daemon and every process it starts.
const daemonHostIsolationMode = "seatbelt"

// daemonNoHostIsolationWarning is the warning of a daemon started with
// --unsafe-no-host-isolation.
const daemonNoHostIsolationWarning = "the macOS sandbox, Seatbelt, is disabled; run trusted workloads only"

// preflightDaemonIsolation checks nothing: every macOS kernel has Seatbelt.
func preflightDaemonIsolation(bool) error { return nil }

// applySeatbelt confines this process to a profile, and seatbeltConfined
// reports whether it is confined; tests replace them.
var (
	applySeatbelt    = seatbelt.Apply
	seatbeltConfined = seatbelt.Confined
)

// enterDaemonHostIsolation confines the daemon, and so every process it
// starts, to seatbelt.DaemonProfile, unless unsafeNoHostIsolation.
//
// A control holder that is confined already applies nothing: it has the
// confinement of the gateway that started it, which is DaemonProfile when the
// gateway has host isolation, as applying DaemonProfile fails in a process
// that another profile confines. Profiles do not stack, and macOS 15 refuses
// to apply even the same profile again.
func enterDaemonHostIsolation(unsafeNoHostIsolation, controlHolder bool) error {
	if unsafeNoHostIsolation {
		return nil
	}
	if controlHolder && seatbeltConfined() {
		return nil
	}
	if err := applySeatbelt(seatbelt.DaemonProfile); err != nil {
		return fmt.Errorf("confining ttcd with the macOS sandbox, Seatbelt: %w; restart with --unsafe-no-host-isolation for trusted workloads", err)
	}
	return nil
}

// runscNoHostIsolation reports whether runsc runs its sandboxes without host
// isolation of its own. On macOS it always does: it has none, and the Seatbelt
// profile ttcd confines itself to confines them instead.
func runscNoHostIsolation(bool) bool { return true }

func preflightDaemonExecutable(path string) error {
	command := exec.Command(path, daemonRootlessProbeArg)
	if err := command.Run(); err != nil {
		return fmt.Errorf("cannot execute: %w; ad-hoc macOS builds must be signed at their final installed path with the com.apple.security.hypervisor entitlement", err)
	}
	entitlements, err := darwinExecutableEntitlements(path)
	if err != nil {
		return err
	}
	if !hasDarwinBooleanEntitlement(entitlements, "com.apple.security.hypervisor") {
		return fmt.Errorf("missing required com.apple.security.hypervisor entitlement in %q; after copying ttcd to its final path, sign it with the hvf.entitlements of its release's zip, or runsc/hvf.entitlements of a source tree: codesign --force --sign - --entitlements hvf.entitlements %q", path, path)
	}
	return nil
}

func darwinExecutableEntitlements(path string) ([]byte, error) {
	command := exec.Command("/usr/bin/codesign", "-d", "--entitlements", ":-", path)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	entitlements, err := command.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return nil, fmt.Errorf("reading macOS entitlements from %q: %w: %s", path, err, detail)
		}
		return nil, fmt.Errorf("reading macOS entitlements from %q: %w", path, err)
	}
	return entitlements, nil
}

func hasDarwinBooleanEntitlement(plist []byte, name string) bool {
	decoder := xml.NewDecoder(bytes.NewReader(plist))
	for {
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "key" {
			continue
		}
		var key string
		if err := decoder.DecodeElement(&key, &start); err != nil || key != name {
			continue
		}
		for {
			token, err := decoder.Token()
			if err != nil {
				return false
			}
			switch token := token.(type) {
			case xml.CharData:
				if len(bytes.TrimSpace(token)) != 0 {
					return false
				}
			case xml.StartElement:
				return token.Name.Local == "true"
			default:
				return false
			}
		}
	}
}

func createRootlessProbe() error {
	return fmt.Errorf("Linux user namespaces are unavailable on native macOS: %w", unix.EOPNOTSUPP)
}

// daemonStatfsNetworkFilesystem returns the name of the network filesystem
// statfs describes, or "" if it describes a local one: macOS marks every
// local filesystem MNT_LOCAL.
func daemonStatfsNetworkFilesystem(statfs *unix.Statfs_t) string {
	name := make([]byte, 0, len(statfs.Fstypename))
	for _, c := range statfs.Fstypename {
		if c == 0 {
			break
		}
		name = append(name, byte(c))
	}
	switch string(name) {
	case "nfs", "smbfs", "afpfs", "webdav", "cifs", "ftp":
		return string(name)
	}
	if statfs.Flags&unix.MNT_LOCAL == 0 {
		return string(name)
	}
	return ""
}

// daemonUnixPathMax is the size of sockaddr_un.sun_path, terminator included.
const daemonUnixPathMax = 104

// readHostBootID returns an identifier of the host's current boot.
func readHostBootID() (string, error) {
	return unix.Sysctl("kern.bootsessionuuid")
}

// syncFileData makes what was written to file durable. Darwin's full sync,
// which File.Sync is, is the one that reaches the disk.
func syncFileData(file *os.File) error {
	return file.Sync()
}

// growPipe does nothing: a Darwin pipe's buffer is not set.
func growPipe(*os.File) {}

// checkDaemonPeer names the client of conn, which access must admit.
func checkDaemonPeer(conn net.Conn, access daemonAccess) (daemonPeer, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return daemonPeer{}, fmt.Errorf("non-Unix daemon connection %T", conn)
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return daemonPeer{}, err
	}
	var credential *unix.Xucred
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		credential, socketErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return daemonPeer{}, err
	}
	if socketErr != nil {
		return daemonPeer{}, socketErr
	}
	if credential == nil {
		return daemonPeer{}, fmt.Errorf("daemon peer credentials are unavailable")
	}
	// The first of the peer's groups is its effective group.
	peer := daemonPeer{UID: int(credential.Uid), GID: -1}
	for i := 0; i < int(credential.Ngroups) && i < len(credential.Groups); i++ {
		if i == 0 {
			peer.GID = int(credential.Groups[i])
		}
		peer.Groups = append(peer.Groups, int(credential.Groups[i]))
	}
	if !access.admits(peer) {
		return peer, access.peerRefusal(peer)
	}
	return peer, nil
}

// The child re-arms an EVFILT_PROC parent watcher during its own startup.
func setTTCCommandParentDeathSignal(command *exec.Cmd) {
	command.SysProcAttr = &unix.SysProcAttr{}
}

// selfExecutable is the path of this process's own binary.
func selfExecutable() (string, error) { return os.Executable() }

// setControlHolderProcess starts a login node's control holder in a session of
// its own; armControlHolderParentDeath ends it with the gateway that starts it.
func setControlHolderProcess(command *exec.Cmd) {
	command.SysProcAttr = &unix.SysProcAttr{Setsid: true}
}

// armControlHolderParentDeath ends this control holder with the gateway that
// started it, as Pdeathsig does on Linux, with an EVFILT_PROC watcher.
func armControlHolderParentDeath() error {
	parentPID := os.Getppid()
	kq, err := unix.Kqueue()
	if err != nil {
		return err
	}
	change := unix.Kevent_t{
		Ident:  uint64(parentPID),
		Filter: unix.EVFILT_PROC,
		Flags:  unix.EV_ADD | unix.EV_ENABLE | unix.EV_ONESHOT,
		Fflags: unix.NOTE_EXIT,
	}
	if _, err := unix.Kevent(kq, []unix.Kevent_t{change}, nil, nil); err != nil {
		unix.Close(kq)
		return err
	}
	// The gateway may have ended before the watcher was armed.
	if parentPID <= 1 || os.Getppid() != parentPID {
		unix.Close(kq)
		return unix.ESRCH
	}
	go func() {
		defer unix.Close(kq)
		events := make([]unix.Kevent_t, 1)
		for {
			if n, err := unix.Kevent(kq, nil, events, nil); err == unix.EINTR {
				continue
			} else if err == nil && n != 0 {
				_ = unix.Kill(os.Getpid(), unix.SIGTERM)
			}
			return
		}
	}()
	return nil
}

func portForwardProcessMatches(pid int, _, _, _ string) bool {
	return unix.Kill(pid, 0) == nil
}

func legacyPortLockHolder(_, _, _, _ string) (int, error) {
	return 0, fmt.Errorf("published-port lock is held by a legacy listener without a recorded process ID")
}

// currentNodeMemoryBytes reports the resident memory of this daemon and of
// every runtime or helper process whose argv names the node-local root. Darwin
// has no /proc, so the process table comes from ps(1), which reports RSS in KiB.
func currentNodeMemoryBytes(localRoot string) (uint64, error) {
	output, err := exec.Command("/bin/ps", "-axww", "-o", "pid=,rss=,command=").Output()
	if err != nil {
		return 0, fmt.Errorf("listing host processes: %w", err)
	}
	if localRoot != "" {
		localRoot = filepath.Clean(localRoot)
	}
	self := os.Getpid()
	var total uint64
	foundSelf := false
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		residentKiB, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		if pid == self {
			foundSelf = true
		} else if localRoot == "" || !strings.Contains(strings.Join(fields[2:], " "), localRoot) {
			continue
		}
		total += residentKiB << 10
	}
	if !foundSelf {
		return 0, fmt.Errorf("ps did not report daemon process %d", self)
	}
	return total, nil
}

// runtimeMemoryBytes returns, by runtime, the resident memory of the
// processes of each of runtimes, container IDs, on the node with local root
// localRoot, as currentNodeMemoryBytes counts it: those whose argv names both
// the root and the runtime.
func runtimeMemoryBytes(localRoot string, runtimes []string) (map[string]uint64, error) {
	output, err := exec.Command("/bin/ps", "-axww", "-o", "pid=,rss=,command=").Output()
	if err != nil {
		return nil, fmt.Errorf("listing host processes: %w", err)
	}
	localRoot = filepath.Clean(localRoot)
	held := make(map[string]uint64, len(runtimes))
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		residentKiB, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		command := strings.Join(fields[2:], " ")
		if !strings.Contains(command, localRoot) {
			continue
		}
		for _, runtime := range runtimes {
			if runtime != "" && strings.Contains(command, runtime) {
				held[runtime] += residentKiB << 10
				break
			}
		}
	}
	return held, nil
}

// runnableExecutable writes content, an executable named name, in dir, and
// returns its path. ttcd carries no executable on macOS, where it is its
// renderer.
func runnableExecutable(name string, content []byte, dir string) (string, error) {
	return writeContainerFile(dir, name, content, 0555)
}

// cloexecStreamSocketpair creates a close-on-exec AF_UNIX stream pair. Darwin
// has no SOCK_CLOEXEC; ForkLock keeps a concurrent exec from inheriting the
// descriptors before they are marked, as os.Pipe does here.
func cloexecStreamSocketpair() ([2]int, error) {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return fds, err
	}
	unix.CloseOnExec(fds[0])
	unix.CloseOnExec(fds[1])
	return fds, nil
}

// awaitExit returns at once: a Wait waits for the process.
func awaitExit(int) {}
