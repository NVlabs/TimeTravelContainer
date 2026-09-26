//go:build linux

package host

import (
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sync"
)

func isEpollable(fd int) bool {
	epollfd, err := unix.EpollCreate1(0)
	if err != nil {
		return false
	}
	defer unix.Close(epollfd)
	event := unix.EpollEvent{Fd: int32(fd), Events: unix.EPOLLIN}
	return unix.EpollCtl(epollfd, unix.EPOLL_CTL_ADD, fd, &event) == nil
}

func hostStatx(fd int, mask uint32, syncFlags uint32) (linux.Statx, error) {
	var s unix.Statx_t
	err := unix.Statx(fd, "", int(unix.AT_EMPTY_PATH|syncFlags), int(mask), &s)
	if linuxerr.Equals(linuxerr.ENOSYS, err) {
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			return linux.Statx{}, err
		}
		return linux.Statx{
			Mask: linux.STATX_BASIC_STATS, Blksize: uint32(st.Blksize), Nlink: uint32(st.Nlink),
			UID: st.Uid, GID: st.Gid, Mode: uint16(st.Mode), Size: uint64(st.Size), Blocks: uint64(st.Blocks),
			Atime: linux.StatxTimestamp{Sec: st.Atim.Sec, Nsec: uint32(st.Atim.Nsec)},
			Ctime: linux.StatxTimestamp{Sec: st.Ctim.Sec, Nsec: uint32(st.Ctim.Nsec)},
			Mtime: linux.StatxTimestamp{Sec: st.Mtim.Sec, Nsec: uint32(st.Mtim.Nsec)},
		}, nil
	}
	if err != nil {
		return linux.Statx{}, err
	}
	return linux.Statx{
		Mask: s.Mask & linux.STATX_ALL, Blksize: s.Blksize, Attributes: s.Attributes,
		Nlink: s.Nlink, UID: s.Uid, GID: s.Gid, Mode: s.Mode, Ino: s.Ino,
		Size: s.Size, Blocks: s.Blocks, AttributesMask: s.Attributes_mask,
		Atime: linux.StatxTimestamp{Sec: s.Atime.Sec, Nsec: s.Atime.Nsec},
		Btime: linux.StatxTimestamp{Sec: s.Btime.Sec, Nsec: s.Btime.Nsec},
		Ctime: linux.StatxTimestamp{Sec: s.Ctime.Sec, Nsec: s.Ctime.Nsec},
		Mtime: linux.StatxTimestamp{Sec: s.Mtime.Sec, Nsec: s.Mtime.Nsec},
	}, nil
}

func setStatVirtualOwner(st *unix.Stat_t, uid, gid, mode uint32) {
	st.Uid, st.Gid, st.Mode = uid, gid, mode
}

func hostFallocate(fd int, mode uint32, offset, length int64) error {
	return unix.Fallocate(fd, mode, offset, length)
}

func hostDataSync(fd int) error { return unix.Fdatasync(fd) }

var hostEventFdDevice uint64
var hostEventFdDeviceOnce sync.Once

func isHostEventFdDevice(dev uint64) bool {
	hostEventFdDeviceOnce.Do(func() {
		efd, _, errno := unix.RawSyscall(unix.SYS_EVENTFD2, 0, 0, 0)
		if errno != 0 {
			log.Warningf("failed to create dummy eventfd: %v. Importing eventfds will fail", error(errno))
			return
		}
		defer unix.Close(int(efd))
		var st unix.Stat_t
		if err := unix.Fstat(int(efd), &st); err != nil {
			log.Warningf("failed to stat dummy eventfd: %v. Importing eventfds will fail", err)
			return
		}
		hostEventFdDevice = st.Dev
	})
	return hostEventFdDevice != 0 && dev == hostEventFdDevice
}
