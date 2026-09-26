//go:build linux

package cmd

import (
	"bytes"
	"errors"
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

const defaultRuntimePlatform = "systrap"

func runtimePlatformFlagUsage() string { return "systrap|kvm" }

func runtimePlatformCompletions() []string { return []string{"kvm", "systrap"} }

func normalizeHostRuntimePlatform(value string) (string, error) {
	switch value {
	case "systrap", "kvm":
		return value, nil
	default:
		return "", fmt.Errorf("unsupported runtime platform %q; supported platforms are systrap and kvm", value)
	}
}

func preflightDaemonPlatform(runtimePlatform string) error {
	if runtimePlatform != "kvm" {
		return nil
	}
	const (
		kvmGetAPIVersion = uintptr(0xae00)
		kvmCreateVM      = uintptr(0xae01)
		kvmAPIVersion    = uintptr(12)
	)
	device, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("runtime platform kvm requires read/write access to /dev/kvm: %w", err)
	}
	defer device.Close()
	version, _, errno := unix.Syscall(unix.SYS_IOCTL, device.Fd(), kvmGetAPIVersion, 0)
	if errno != 0 {
		return fmt.Errorf("querying /dev/kvm API version: %w", errno)
	}
	if version != kvmAPIVersion {
		return fmt.Errorf("unsupported /dev/kvm API version %d; want %d", version, kvmAPIVersion)
	}
	vmFD, _, errno := unix.Syscall(unix.SYS_IOCTL, device.Fd(), kvmCreateVM, 0)
	if errno != 0 {
		return fmt.Errorf("creating a KVM VM: %w", errno)
	}
	return unix.Close(int(vmFD))
}

func evaluateCurrentHostCompatibility(release string) (hostKernelCompatibility, error) {
	return evaluateHostKernelCompatibility(release)
}

func currentHostCompatibilityWarning(c hostKernelCompatibility) string {
	if !c.disableHostGSO {
		return ""
	}
	return fmt.Sprintf("host Linux kernel %s predates 4.14.77; disabling host GSO for compatibility; network performance, especially for large payloads, will be reduced", c.release)
}

func preflightDaemonIsolation(unsafeNoHostIsolation bool) error {
	if unsafeNoHostIsolation || daemonEffectiveUID() == 0 {
		return nil
	}
	if err := createDaemonRootlessProbe(); err != nil {
		hint := "permit unprivileged user namespaces in the host kernel and LSM policy"
		if data, readErr := os.ReadFile("/proc/sys/kernel/apparmor_restrict_unprivileged_userns"); readErr == nil && strings.TrimSpace(string(data)) == "1" {
			hint = "the host AppArmor policy restricts unprivileged user namespaces; grant userns permission to TTC/runsc"
		}
		return fmt.Errorf("rootless mode is unavailable: creating a disposable user/mount namespace: %w; %s, or restart with --unsafe-no-host-isolation for trusted workloads", err, hint)
	}
	return nil
}

func preflightDaemonExecutable(string) error { return nil }

func createRootlessProbe() error {
	command := exec.Command("/proc/self/exe", daemonRootlessProbeArg)
	command.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		Credential:                 &syscall.Credential{Uid: 0, Gid: 0},
		GidMappingsEnableSetgroups: false,
		Pdeathsig:                  syscall.SIGKILL,
	}
	return command.Run()
}

func daemonStatfsIsNFS(statfs *unix.Statfs_t) bool {
	return int64(statfs.Type) == int64(unix.NFS_SUPER_MAGIC)
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
	var credential *syscall.Ucred
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		credential, socketErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if socketErr != nil {
		return socketErr
	}
	if credential == nil {
		return errors.New("daemon peer credentials are unavailable")
	}
	if int(credential.Uid) != os.Geteuid() {
		return fmt.Errorf("daemon peer UID %d does not match server UID %d", credential.Uid, os.Geteuid())
	}
	return nil
}

func setTTCCommandParentDeathSignal(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}

func portForwardProcessMatches(pid int, container, branch, endpoint string) bool {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	fields := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
	want := map[string]bool{
		"port-forward":       false,
		"--branch=" + branch: false,
		container:            false,
		endpoint:             false,
	}
	for _, field := range fields {
		if _, ok := want[field]; ok {
			want[field] = true
		}
	}
	for _, found := range want {
		if !found {
			return false
		}
	}
	return true
}

func legacyPortLockHolder(lockPath, container, branch, endpoint string) (int, error) {
	want, err := os.Stat(lockPath)
	if err != nil {
		return 0, err
	}
	processes, err := os.ReadDir("/proc")
	if err != nil {
		return 0, err
	}
	for _, process := range processes {
		pid, err := strconv.Atoi(process.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		fds, err := os.ReadDir(filepath.Join("/proc", process.Name(), "fd"))
		if err != nil {
			continue
		}
		for _, fd := range fds {
			info, err := os.Stat(filepath.Join("/proc", process.Name(), "fd", fd.Name()))
			if err == nil && os.SameFile(want, info) && portForwardProcessMatches(pid, container, branch, endpoint) {
				return pid, nil
			}
		}
	}
	return 0, fmt.Errorf("published-port lock is held by an unknown process")
}

// currentNodeMemoryBytes accounts only memory controlled by this TTC node.
//
// A daemon is often launched directly from a login shell, systemd --user, or a
// batch allocation. Its containing cgroup can therefore include unrelated
// jobs, caches, and even other TTC roots. Using memory.current from that cgroup
// made an idle daemon appear pressured and caused placement churn that TTC
// could not relieve. Sentries are daemon siblings, but every runtime and
// helper created for this node carries its node-local root in argv. Scan those
// process leaders and add the daemon itself explicitly.
func currentNodeMemoryBytes(localRoot string) (uint64, error) {
	self := os.Getpid()
	total, err := processResidentMemoryBytes(self)
	if err != nil {
		return 0, err
	}
	if localRoot == "" {
		return total, nil
	}
	localRoot = filepath.Clean(localRoot)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, err
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == self {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil || !bytes.Contains(cmdline, []byte(localRoot)) {
			continue
		}
		resident, err := processResidentMemoryBytes(pid)
		if err != nil {
			// Processes naturally race the scan by exiting. An unreadable entry
			// no longer contributes to current resident usage.
			continue
		}
		total += resident
	}
	return total, nil
}

// processResidentMemoryBytes returns the resident bytes charged to one host
// process. /proc contains process leaders, rather than every thread, so this
// does not multiply the Sentry's shared address space by its systrap threads.
func processResidentMemoryBytes(pid int) (uint64, error) {
	statm, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(statm))
	if len(fields) < 2 {
		return 0, errors.New("/proc/self/statm has no resident page count")
	}
	residentPages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0, err
	}
	return residentPages * uint64(os.Getpagesize()), nil
}

// cloexecStreamSocketpair creates a close-on-exec AF_UNIX stream pair.
func cloexecStreamSocketpair() ([2]int, error) {
	return unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
}
