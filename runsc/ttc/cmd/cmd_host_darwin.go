//go:build darwin

package cmd

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"

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
	return nil
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

// currentNodeMemoryBytes reports the daemon's resident high-water mark as a
// conservative process-local proxy. macOS has no Linux-style memory cgroup
// encompassing the daemon and its Sentry siblings. ru_maxrss is measured in
// bytes on Darwin; branch GuestRAM accounting remains authoritative for guest
// memory.
func currentNodeMemoryBytes() (uint64, error) {
	var usage unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &usage); err != nil {
		return 0, fmt.Errorf("reading current process memory usage: %w", err)
	}
	if usage.Maxrss <= 0 {
		return 0, fmt.Errorf("current process maximum resident memory is %d", usage.Maxrss)
	}
	return uint64(usage.Maxrss), nil
}
