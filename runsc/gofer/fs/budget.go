// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package fs

// The gofer's host descriptors are what the files of its mounts cost it: a
// control FD keeps one for every dentry the Sentry holds, and an open FD may
// keep one more. A guest that holds a bind mount's files open, or walks more
// of it than the Sentry's caches forget, would so hold as many in the gofer,
// against a limit (RLIMIT_NOFILE) that the guest must never run into.
//
// So the gofer keeps a descriptor budget (see package fdbudget) as the Sentry
// does. Every descriptor that the gofer can open again is registered with it,
// and the budget closes the least recently used ones that no operation is
// using as the gofer nears its limit. The next operation on a control FD whose
// descriptor was closed opens it again, from the mount's root down the path
// that the gofer's node tree records for it, following no symbolic link on
// the way, and checks that what it found is the file it had. An open FD opens
// its own descriptor again through its control FD's, as Open did.
//
// What the gofer cannot open again stays open: the mount's root, which every
// reopening starts from; the descriptors of files that the guest removes
// while it holds them, which are pinned before they are removed; directories'
// open FDs, whose descriptors carry a getdents position; and FIFOs, devices
// and sockets, which opening has effects on. A file that something else on
// the host removes or replaces while its descriptor is closed is gone for the
// gofer, as it is for an NFS client: operations on it fail with ESTALE.

import (
	"strings"
	"sync"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/fdbudget"
	"gvisor.dev/gvisor/pkg/lisafs"
	"gvisor.dev/gvisor/pkg/log"
)

// budgetedFD is the state that the descriptor budget needs of a host
// descriptor that the gofer can close while it is idle.
type budgetedFD struct {
	// mu serializes closing the descriptor, opening it again, and the start
	// of each use of it.
	mu sync.Mutex

	// users counts the operations using the descriptor now. It rises with mu
	// locked, and the descriptor is closed only while it is zero.
	users atomicbitops.Int32

	// pinned is set if the descriptor must stay open.
	pinned bool

	// entry registers the descriptor with the budget.
	entry fdbudget.Entry
}

// reopenFlags are what reopening a descriptor opened with flags passes: never
// creating, truncating or requiring a new file a second time.
func reopenFlags(flags int) int {
	return flags &^ (unix.O_CREAT | unix.O_EXCL | unix.O_TRUNC)
}

// maxOpenRetries bounds how often an open that the host refuses for want of
// descriptors is tried again, each time after the budget closed idle ones.
const maxOpenRetries = 8

// openRetrying calls open, and again, once the budget has closed idle
// descriptors, while the host refuses the gofer a descriptor.
func openRetrying(open func() (int, error)) (int, error) {
	for i := 0; ; i++ {
		fd, err := open()
		if err == nil || i == maxOpenRetries || !fdbudget.Retry(lisafs.ExtractErrno(err)) {
			return fd, err
		}
	}
}

// evictable returns whether a control FD's descriptor on a file of type mode
// may be closed while it is idle: regular files, directories and symbolic
// links, which opening again has no effect on.
func evictable(mode uint32) bool {
	switch mode & unix.S_IFMT {
	case unix.S_IFREG, unix.S_IFDIR, unix.S_IFLNK:
		return true
	default:
		return false
	}
}

// fileID identifies a file, for checking that a descriptor opened again is on
// the file it was.
type fileID struct {
	devMajor, devMinor uint32
	ino                uint64
}

func fileIDOf(stat lisafs.Statx) fileID {
	return fileID{devMajor: stat.DevMajor, devMinor: stat.DevMinor, ino: stat.Ino}
}

// register enters fd's descriptor in the budget, or pins it if the budget may
// not close it. newControlFDLisa calls it once fd is in the node tree.
func (fd *controlFDLisa) register(mode uint32) {
	b := &fd.budgeted
	b.mu.Lock()
	defer b.mu.Unlock()
	if fd.isMountPoint || !evictable(mode) {
		b.pinned = true
		return
	}
	fdbudget.Opened(&b.entry, fd, fd.hostFD)
}

// acquire keeps fd's descriptor open until release, opening it again if the
// budget closed it. The server's rename lock is held for reading at least, as
// it is around every ControlFDImpl method, so that fd's path is fixed.
func (fd *controlFDLisa) acquire() error {
	b := &fd.budgeted
	b.mu.Lock()
	defer b.mu.Unlock()
	if fd.hostFD < 0 {
		if err := fd.reopenLocked(); err != nil {
			return err
		}
	}
	b.users.Add(1)
	b.entry.Used()
	return nil
}

// release ends a use of fd's descriptor that acquire began.
func (fd *controlFDLisa) release() {
	fd.budgeted.users.Add(-1)
}

// CloseIdleFD implements fdbudget.Holder.CloseIdleFD.
//
// +checklocksignore: checklocks does not follow TryLock.
func (fd *controlFDLisa) CloseIdleFD() bool {
	b := &fd.budgeted
	if !b.mu.TryLock() {
		return false
	}
	defer b.mu.Unlock()
	if b.pinned || b.users.Load() != 0 || fd.hostFD < 0 {
		return false
	}
	_ = unix.Close(fd.hostFD)
	fd.hostFD = -1
	// Nothing uses the writable descriptor either: it is used only by
	// operations that hold the control FD's.
	if w := fd.writableHostFD.RacyLoad(); w >= 0 {
		_ = unix.Close(int(w))
		fd.writableHostFD.Store(-1)
	}
	return true
}

// reopenLocked opens fd's file again from the mount's root, down the path that
// the node tree records for it, and checks that it is the file fd had.
//
// Precondition: fd.budgeted.mu is locked, and the server's rename lock is
// held for reading at least.
func (fd *controlFDLisa) reopenLocked() error {
	node := fd.Node()
	if node.IsDeleted() {
		// A file the guest removes is pinned before it goes; this one went
		// some other way.
		return unix.ESTALE
	}
	root := fd.root
	rootPath, filePath := root.Node().FilePath(), node.FilePath()
	var rel string
	switch {
	case rootPath == "/":
		rel = strings.TrimPrefix(filePath, "/")
	case strings.HasPrefix(filePath, rootPath+"/"):
		rel = filePath[len(rootPath)+1:]
	}
	if rel == "" {
		// The mount's root, or not below it: neither is ever closed.
		log.Warningf("gofer: cannot reopen %q, which is not below the mount at %q", filePath, rootPath)
		return unix.ESTALE
	}
	names := strings.Split(rel, "/")

	// Directories on the way are opened only to walk through, refusing
	// symbolic links; the root's descriptor is never closed.
	dirFD := root.hostFD
	closeDir := func() {
		if dirFD != root.hostFD {
			_ = unix.Close(dirFD)
		}
	}
	defer func() { closeDir() }()
	for _, name := range names[:len(names)-1] {
		next, err := openRetrying(func() (int, error) { return openDirAt(dirFD, name) })
		if err != nil {
			return staleUnlessShort(err)
		}
		closeDir()
		dirFD = next
	}
	last := names[len(names)-1]
	hostFD, err := openRetrying(func() (int, error) {
		return unix.Openat(dirFD, last, combineOpenFlags(reopenFlags(fd.openedFlags)&^openFlags), 0)
	})
	if err != nil {
		return staleUnlessShort(err)
	}
	stat, err := fstatTo(hostFD)
	if err != nil {
		_ = unix.Close(hostFD)
		return err
	}
	if fileIDOf(stat) != fd.id {
		_ = unix.Close(hostFD)
		log.Infof("gofer: %q is no longer the file it was when its descriptor was closed", filePath)
		return unix.ESTALE
	}
	fd.hostFD = hostFD
	fdbudget.Opened(&fd.budgeted.entry, fd, hostFD)
	return nil
}

// staleUnlessShort returns the error that an operation on a file whose path no
// longer leads to it fails with: ESTALE, as for NFS, unless the host refused
// descriptors or access, which the caller may report as they are.
func staleUnlessShort(err error) error {
	switch e := lisafs.ExtractErrno(err); e {
	case unix.EMFILE, unix.ENFILE, unix.EACCES, unix.EPERM, unix.ENOMEM:
		return e
	default:
		return unix.ESTALE
	}
}

// pin opens fd's descriptor again if the budget closed it, and keeps it open
// from now on: fd's file is about to lose its path, which nothing could open
// it from again.
//
// Precondition: the server's rename lock is held for reading at least.
func (fd *controlFDLisa) pin() error {
	b := &fd.budgeted
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pinned {
		return nil
	}
	if fd.hostFD < 0 {
		if err := fd.reopenLocked(); err != nil {
			return err
		}
	}
	b.pinned = true
	fdbudget.Closed(&b.entry)
	return nil
}

// pinChild pins the descriptors of every control FD on the child named name
// of fd's directory, which is about to be removed or replaced.
//
// Precondition: the server's rename lock is held for reading at least.
func (fd *controlFDLisa) pinChild(name string) error {
	var child *lisafs.Node
	fd.Node().WithChildrenMu(func() {
		child = fd.Node().LookupChildLocked(name)
	})
	if child == nil {
		return nil
	}
	var err error
	child.ForEachControlFD(func(impl lisafs.ControlFDImpl) {
		if e := impl.(*controlFDLisa).pin(); e != nil && err == nil {
			err = e
		}
	})
	return err
}

// closeForGood closes fd's descriptor when fd is destroyed.
func (fd *controlFDLisa) closeForGood() {
	b := &fd.budgeted
	b.mu.Lock()
	defer b.mu.Unlock()
	if fd.hostFD >= 0 {
		_ = unix.Close(fd.hostFD)
		fd.hostFD = -1
	}
	if w := fd.writableHostFD.RacyLoad(); w >= 0 {
		_ = unix.Close(int(w))
		fd.writableHostFD.Store(-1)
	}
	fdbudget.Closed(&b.entry)
}

// register enters fd's own descriptor in the budget, or pins it: only regular
// files' are closed while idle. A borrowed descriptor is its control FD's.
func (fd *openFDLisa) register() {
	if fd.borrowed {
		return
	}
	b := &fd.budgeted
	b.mu.Lock()
	defer b.mu.Unlock()
	if fd.ControlFD().FD().FileType() != unix.S_IFREG {
		b.pinned = true
		return
	}
	fdbudget.Opened(&b.entry, fd, fd.hostFD)
}

// acquire keeps fd's descriptor open until release, and returns it, opening
// it again through its control FD's if the budget closed it.
func (fd *openFDLisa) acquire() (int, error) {
	cfd := fd.ControlFD().(*controlFDLisa)
	if fd.borrowed {
		if err := cfd.acquire(); err != nil {
			return -1, err
		}
		return cfd.hostFD, nil
	}
	b := &fd.budgeted
	b.mu.Lock()
	defer b.mu.Unlock()
	if fd.hostFD < 0 {
		if err := cfd.acquire(); err != nil {
			return -1, err
		}
		hostFD, err := openRetrying(func() (int, error) {
			return reopenFD(cfd.hostFD, reopenFlags(int(fd.flags))&^unix.O_NOFOLLOW)
		})
		cfd.release()
		if err != nil {
			return -1, err
		}
		fd.hostFD = hostFD
		fdbudget.Opened(&b.entry, fd, hostFD)
	}
	b.users.Add(1)
	b.entry.Used()
	return fd.hostFD, nil
}

// release ends a use of fd's descriptor that acquire began.
func (fd *openFDLisa) release() {
	if fd.borrowed {
		fd.ControlFD().(*controlFDLisa).release()
		return
	}
	fd.budgeted.users.Add(-1)
}

// CloseIdleFD implements fdbudget.Holder.CloseIdleFD.
//
// +checklocksignore: checklocks does not follow TryLock.
func (fd *openFDLisa) CloseIdleFD() bool {
	b := &fd.budgeted
	if !b.mu.TryLock() {
		return false
	}
	defer b.mu.Unlock()
	if b.pinned || b.users.Load() != 0 || fd.hostFD < 0 {
		return false
	}
	_ = unix.Close(fd.hostFD)
	fd.hostFD = -1
	return true
}
