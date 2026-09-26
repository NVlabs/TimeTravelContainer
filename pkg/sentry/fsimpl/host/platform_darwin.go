//go:build darwin

package host

import (
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
)

// isEpollable returns true if Linux epoll could wait on a file like fd.
// kqueue also watches regular files and directories, whose readiness it
// defines by the file offset, but epoll refuses them, and poll(2) always
// reports them ready.
func isEpollable(fd int) bool {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return false
	}
	if mode := st.Mode & unix.S_IFMT; mode == unix.S_IFREG || mode == unix.S_IFDIR {
		return false
	}
	kq, err := unix.Kqueue()
	if err != nil {
		return false
	}
	defer unix.Close(kq)
	_, err = unix.Kevent(kq, []unix.Kevent_t{{Ident: uint64(fd), Filter: unix.EVFILT_READ, Flags: unix.EV_ADD | unix.EV_CLEAR}}, nil, nil)
	return err == nil
}

func hostStatx(fd int, mask uint32, syncFlags uint32) (linux.Statx, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return linux.Statx{}, err
	}
	return linux.Statx{
		Mask:    linux.STATX_BASIC_STATS | linux.STATX_BTIME,
		Blksize: uint32(st.Blksize), Nlink: uint32(st.Nlink), UID: st.Uid, GID: st.Gid,
		Mode: uint16(st.Mode), Size: uint64(st.Size), Blocks: uint64(st.Blocks),
		Atime: linux.StatxTimestamp{Sec: st.Atim.Sec, Nsec: uint32(st.Atim.Nsec)},
		Btime: linux.StatxTimestamp{Sec: st.Btim.Sec, Nsec: uint32(st.Btim.Nsec)},
		Ctime: linux.StatxTimestamp{Sec: st.Ctim.Sec, Nsec: uint32(st.Ctim.Nsec)},
		Mtime: linux.StatxTimestamp{Sec: st.Mtim.Sec, Nsec: uint32(st.Mtim.Nsec)},
	}, nil
}

func setStatVirtualOwner(st *unix.Stat_t, uid, gid, mode uint32) {
	st.Uid, st.Gid, st.Mode = uid, gid, uint16(mode)
}

func hostFallocate(fd int, mode uint32, offset, length int64) error {
	if mode&^uint32(linux.FALLOC_FL_KEEP_SIZE) != 0 || offset < 0 || length <= 0 {
		return unix.EOPNOTSUPP
	}
	end := offset + length
	if end < offset {
		return unix.EINVAL
	}
	fstore := unix.Fstore_t{Flags: unix.F_ALLOCATEALL, Posmode: unix.F_PEOFPOSMODE, Offset: 0, Length: end}
	if err := unix.FcntlFstore(uintptr(fd), unix.F_PREALLOCATE, &fstore); err != nil {
		return err
	}
	if mode&uint32(linux.FALLOC_FL_KEEP_SIZE) == 0 {
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			return err
		}
		if end > st.Size {
			return unix.Ftruncate(fd, end)
		}
	}
	return nil
}

// Darwin has no fdatasync; fsync is a stronger compatible operation.
func hostDataSync(fd int) error { return unix.Fsync(fd) }

func isHostEventFdDevice(dev uint64) bool { return false }
