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
	return hostKernelCompatibility{release: release, version: version, disableHostGSO: true}, nil
}

func currentHostCompatibilityWarning(hostKernelCompatibility) string { return "" }

func preflightDaemonIsolation(unsafeNoHostIsolation bool) error {
	if !unsafeNoHostIsolation {
		return fmt.Errorf("native macOS execution requires --unsafe-no-host-isolation")
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
