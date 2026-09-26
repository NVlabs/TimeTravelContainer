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
)

const defaultRuntimePlatform = "hvf"

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

func preflightDaemonIsolation(unsafeNoHostIsolation bool) error {
	if !unsafeNoHostIsolation {
		return fmt.Errorf("native macOS execution requires --unsafe-no-host-isolation")
	}
	return nil
}

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
		return fmt.Errorf("missing required com.apple.security.hypervisor entitlement in %q; after copying ttcd to its final path, run: codesign --force --sign - --entitlements /path/to/runsc/hvf.entitlements %q", path, path)
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

func daemonStatfsIsNFS(statfs *unix.Statfs_t) bool {
	name := make([]byte, 0, len(statfs.Fstypename))
	for _, c := range statfs.Fstypename {
		if c == 0 {
			break
		}
		name = append(name, byte(c))
	}
	return string(name) == "nfs"
}

func checkDaemonPeer(conn net.Conn) error {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("non-Unix daemon connection %T", conn)
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return err
	}
	var credential *unix.Xucred
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		credential, socketErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return err
	}
	if socketErr != nil {
		return socketErr
	}
	if credential == nil {
		return fmt.Errorf("daemon peer credentials are unavailable")
	}
	if int(credential.Uid) != os.Geteuid() {
		return fmt.Errorf("daemon peer UID %d does not match server UID %d", credential.Uid, os.Geteuid())
	}
	return nil
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
