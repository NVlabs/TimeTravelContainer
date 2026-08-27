//go:build linux

package cmd

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
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
