//go:build linux

package container

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/log"
)

func containerHostSocketPair() ([2]int, error) {
	return unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
}

func setGoferParentDeathSignal(cmd *exec.Cmd) { cmd.SysProcAttr.Pdeathsig = unix.SIGKILL }

func setGoferCommandCredential(cmd *exec.Cmd, uid, gid uint32) error {
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uid, Gid: gid}
	return nil
}

func disableGoferSetgroupsForIDMapping(cmd *exec.Cmd) error {
	cmd.SysProcAttr.GidMappingsEnableSetgroups = false
	return nil
}

func hostSetOOMScoreAdj(pid, scoreAdj int) error {
	f, err := os.OpenFile(fmt.Sprintf("/proc/%d/oom_score_adj", pid), os.O_WRONLY, 0644)
	if err != nil {
		if os.IsNotExist(err) {
			log.Warningf("Process (%d) not found setting oom_score_adj", pid)
			return nil
		}
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(strconv.Itoa(scoreAdj)); err != nil {
		if errors.Is(err, unix.ESRCH) {
			log.Warningf("Process (%d) exited while setting oom_score_adj", pid)
			return nil
		}
		return fmt.Errorf("setting oom_score_adj to %d: %v", scoreAdj, err)
	}
	return nil
}
