// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package proc

import (
	"bytes"
	"fmt"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/kernfs"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/usermem"
	"gvisor.dev/gvisor/pkg/waiter"
)

// Limits of pressure triggers' windows, as Linux's psi_trigger_create has
// them, in microseconds.
const (
	psiWindowMaxUS = 10_000_000
	// psiUnprivilegedWindowUS divides the windows of the triggers of users
	// without CAP_SYS_RESOURCE.
	psiUnprivilegedWindowUS = 2_000_000
)

// newPressureDir returns /proc/pressure, whose files are those of Linux's
// pressure stall information (PSI). The sentry doesn't account stalls: no
// task ever stalls, so averages and totals are 0, and triggers, which
// programs such as Android's lmkd set to be woken of pressure, never fire.
func (fs *filesystem) newPressureDir(ctx context.Context, root *auth.Credentials) kernfs.Inode {
	return fs.newStaticDir(ctx, root, map[string]kernfs.Inode{
		"cpu":    fs.newPressureInode(ctx, root, true),
		"io":     fs.newPressureInode(ctx, root, true),
		"irq":    fs.newPressureInode(ctx, root, false),
		"memory": fs.newPressureInode(ctx, root, true),
	})
}

// pressureInode is a file of /proc/pressure.
//
// +stateify savable
type pressureInode struct {
	kernfs.DynamicBytesFile

	// some is whether the resource has partial stalls; irq has only full
	// ones.
	some bool
}

var _ vfs.DynamicBytesSource = (*pressureInode)(nil)

func (fs *filesystem) newPressureInode(ctx context.Context, creds *auth.Credentials, some bool) kernfs.Inode {
	i := &pressureInode{some: some}
	i.DynamicBytesFile.Init(ctx, creds, linux.UNNAMED_MAJOR, fs.devMinor, fs.NextIno(), i, 0666)
	return i
}

// Generate implements vfs.DynamicBytesSource.Generate.
func (i *pressureInode) Generate(ctx context.Context, buf *bytes.Buffer) error {
	if i.some {
		buf.WriteString("some avg10=0.00 avg60=0.00 avg300=0.00 total=0\n")
	}
	buf.WriteString("full avg10=0.00 avg60=0.00 avg300=0.00 total=0\n")
	return nil
}

// Open implements kernfs.Inode.Open.
func (i *pressureInode) Open(ctx context.Context, rp *vfs.ResolvingPath, d *kernfs.Dentry, opts vfs.OpenOptions) (*vfs.FileDescription, error) {
	fd := &pressureFD{some: i.some}
	if err := fd.DynamicBytesFD.InitWithImpl(fd, rp.Mount(), d, i, i.Locks(), opts.Flags, rp.Credentials()); err != nil {
		return nil, err
	}
	return fd.VFSFileDescription(), nil
}

// pressureFD is an open file of /proc/pressure, which may have a trigger.
//
// +stateify savable
type pressureFD struct {
	kernfs.DynamicBytesFD

	some bool

	mu sync.Mutex `state:"nosave"`
	// hasTrigger is whether a trigger was written, which is once at most.
	//
	// +checklocks:mu
	hasTrigger bool
}

// Write implements vfs.FileDescriptionImpl.Write. It sets the file's
// trigger, "some|full <threshold us> <window us>", as Linux's psi_write
// does.
func (fd *pressureFD) Write(ctx context.Context, src usermem.IOSequence, opts vfs.WriteOptions) (int64, error) {
	n := src.NumBytes()
	if n == 0 {
		return 0, linuxerr.EINVAL
	}
	buf := make([]byte, min(n, 32))
	if _, err := src.CopyIn(ctx, buf); err != nil {
		return 0, err
	}
	buf[len(buf)-1] = 0
	if i := bytes.IndexByte(buf, 0); i >= 0 {
		buf = buf[:i]
	}

	fd.mu.Lock()
	defer fd.mu.Unlock()
	if fd.hasTrigger {
		return 0, linuxerr.EBUSY
	}
	var threshold, window uint32
	var some bool
	if _, err := fmt.Sscanf(string(buf), "some %d %d", &threshold, &window); err == nil {
		some = true
	} else if _, err := fmt.Sscanf(string(buf), "full %d %d", &threshold, &window); err != nil {
		return 0, linuxerr.EINVAL
	}
	if some && !fd.some {
		return 0, linuxerr.EINVAL
	}
	if window == 0 || window > psiWindowMaxUS {
		return 0, linuxerr.EINVAL
	}
	// The file's opener's capabilities, not the writer's, count.
	if fd.VFSFileDescription().Credentials().EffectiveCaps&auth.CapabilitySetOf(linux.CAP_SYS_RESOURCE) == 0 && window%psiUnprivilegedWindowUS != 0 {
		return 0, linuxerr.EINVAL
	}
	if threshold == 0 || threshold > window {
		return 0, linuxerr.EINVAL
	}
	fd.hasTrigger = true
	return n, nil
}

// PWrite implements vfs.FileDescriptionImpl.PWrite. Linux ignores offsets.
func (fd *pressureFD) PWrite(ctx context.Context, src usermem.IOSequence, offset int64, opts vfs.WriteOptions) (int64, error) {
	return fd.Write(ctx, src, opts)
}

// Readiness implements waiter.Waitable.Readiness, as Linux's
// psi_trigger_poll does: without a trigger, the file has an error; with one,
// it has priority data when the trigger fires, which is never.
func (fd *pressureFD) Readiness(mask waiter.EventMask) waiter.EventMask {
	ready := waiter.ReadableEvents | waiter.WritableEvents
	fd.mu.Lock()
	if !fd.hasTrigger {
		ready |= waiter.EventErr | waiter.EventPri
	}
	fd.mu.Unlock()
	return mask & ready
}

// Epollable implements vfs.FileDescriptionImpl.Epollable.
func (fd *pressureFD) Epollable() bool {
	return true
}

// EventRegister implements waiter.Waitable.EventRegister. Readiness never
// changes, but for the one trigger that a write may set.
func (fd *pressureFD) EventRegister(e *waiter.Entry) error {
	return nil
}

// EventUnregister implements waiter.Waitable.EventUnregister.
func (fd *pressureFD) EventUnregister(e *waiter.Entry) {}
