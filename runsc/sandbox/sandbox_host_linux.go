//go:build linux

package sandbox

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/moby/sys/capability"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
)

func directIOFlag() int { return unix.O_DIRECT }

func hostFDDirectory() string { return "/proc/self/fd" }

func hostSocketPair() ([2]int, error) {
	return unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
}

func usableControlSocketPath(path string) (string, func(), error) {
	if len(path) < linux.UnixPathMax {
		return path, func() {}, nil
	}
	fd, err := unix.Open(path, unix.O_PATH, 0)
	if err != nil {
		return "", nil, fmt.Errorf("opening long control socket path %q: %w", path, err)
	}
	return filepath.Join("/proc/self/fd", strconv.Itoa(fd)), func() { unix.Close(fd) }, nil
}

func disableSetgroupsForIDMapping(cmd *exec.Cmd) error {
	cmd.SysProcAttr.GidMappingsEnableSetgroups = false
	return nil
}

func setCommandCredential(cmd *exec.Cmd, uid, gid uint32) error {
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uid, Gid: gid}
	return nil
}

func configureSandboxIDMapping(cmd *exec.Cmd, sandboxID uint32, hostUID, hostGID int) error {
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: sandboxID, Gid: sandboxID}
	cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{{ContainerID: int(sandboxID), HostID: hostUID, Size: 1}}
	cmd.SysProcAttr.GidMappings = []syscall.SysProcIDMap{{ContainerID: int(sandboxID), HostID: hostGID, Size: 1}}
	return nil
}

func addChrootAmbientCapabilities(cmd *exec.Cmd, ptrace bool) error {
	cmd.SysProcAttr.AmbientCaps = append(cmd.SysProcAttr.AmbientCaps,
		uintptr(capability.CAP_SYS_ADMIN),
		uintptr(capability.CAP_SYS_CHROOT),
		uintptr(capability.CAP_SETPCAP),
	)
	if ptrace {
		cmd.SysProcAttr.AmbientCaps = append(cmd.SysProcAttr.AmbientCaps, uintptr(capability.CAP_SYS_PTRACE))
	}
	return nil
}

func setCommandParentDeathSignal(cmd *exec.Cmd) { cmd.SysProcAttr.Pdeathsig = unix.SIGKILL }

func configureRootlessCommandCapabilities(cmd *exec.Cmd) error {
	cmd.SysProcAttr.AmbientCaps = []uintptr{
		unix.CAP_CHOWN,
		unix.CAP_DAC_OVERRIDE,
		unix.CAP_DAC_READ_SEARCH,
		unix.CAP_FOWNER,
		unix.CAP_FSETID,
		unix.CAP_SYS_CHROOT,
		unix.CAP_SETUID,
		unix.CAP_SETGID,
		unix.CAP_SYS_ADMIN,
		unix.CAP_SETPCAP,
	}
	return nil
}
