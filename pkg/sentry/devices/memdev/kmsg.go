// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package memdev

import (
	"bytes"
	"fmt"
	"strconv"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	ktime "gvisor.dev/gvisor/pkg/sentry/ktime"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/usermem"
	"gvisor.dev/gvisor/pkg/waiter"
)

// kmsgDevMinor is /dev/kmsg's minor number. No /dev/kmsg is made by default,
// as Docker makes none; Android's init makes its own.
const kmsgDevMinor = 11

// kmsgDefaultFacility is the facility of records written without one: user,
// as Linux's devkmsg_write has it.
const kmsgDefaultFacility = 1

// kmsgDefaultLevel is the level of records written without one: Linux's
// default message level, KERN_WARNING.
const kmsgDefaultLevel = 4

// kmsgDevice implements vfs.Device for /dev/kmsg, the kernel log, as Linux's
// kernel/printk/printk.c does: each write is a record, and each read returns
// the next record of the reader's.
//
// +stateify savable
type kmsgDevice struct{}

// Open implements vfs.Device.Open.
func (kmsgDevice) Open(ctx context.Context, mnt *vfs.Mount, vfsd *vfs.Dentry, opts vfs.OpenOptions) (*vfs.FileDescription, error) {
	k := kernel.KernelFromContext(ctx)
	fd := &kmsgFD{k: k, seq: k.Syslog().FirstSeq()}
	if err := fd.vfsfd.Init(fd, opts.Flags, auth.CredentialsFromContext(ctx), mnt, vfsd, &vfs.FileDescriptionOptions{
		UseDentryMetadata: true,
		SpecialFile:       true,
	}); err != nil {
		return nil, err
	}
	return &fd.vfsfd, nil
}

// kmsgFD implements vfs.FileDescriptionImpl for /dev/kmsg.
//
// +stateify savable
type kmsgFD struct {
	vfsfd vfs.FileDescription
	vfs.FileDescriptionDefaultImpl
	vfs.DentryMetadataFileDescriptionImpl
	vfs.NoLockFD

	k *kernel.Kernel

	// mu protects seq.
	mu sync.Mutex `state:"nosave"`
	// seq is the sequence number of the next record that the file reads.
	seq uint64
}

// Release implements vfs.FileDescriptionImpl.Release.
func (fd *kmsgFD) Release(context.Context) {}

// Read implements vfs.FileDescriptionImpl.Read.
func (fd *kmsgFD) Read(ctx context.Context, dst usermem.IOSequence, opts vfs.ReadOptions) (int64, error) {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	r, next, ok := fd.k.Syslog().Record(fd.seq)
	if !ok {
		if next != fd.seq {
			// The log dropped the record: the reader learns that it
			// missed some, and reads on from the oldest.
			fd.seq = next
			return 0, linuxerr.EPIPE
		}
		return 0, linuxerr.ErrWouldBlock
	}
	var line bytes.Buffer
	fmt.Fprintf(&line, "%d,%d,%d,-;", r.Prio, r.Seq, r.Time.Microseconds())
	kmsgEscape(&line, r.Msg)
	line.WriteByte('\n')
	if int64(line.Len()) > dst.NumBytes() {
		return 0, linuxerr.EINVAL
	}
	n, err := dst.CopyOut(ctx, line.Bytes())
	if err != nil {
		return int64(n), err
	}
	fd.seq = next
	return int64(n), nil
}

// kmsgEscape writes msg as /dev/kmsg shows it: non-printable bytes, and
// backslashes, as \xNN.
func kmsgEscape(buf *bytes.Buffer, msg string) {
	for i := 0; i < len(msg); i++ {
		c := msg[i]
		if c < ' ' || c >= 127 || c == '\\' {
			fmt.Fprintf(buf, "\\x%02x", c)
			continue
		}
		buf.WriteByte(c)
	}
}

// Write implements vfs.FileDescriptionImpl.Write. A write is a record, whose
// message may start with its facility and level, "<N>".
func (fd *kmsgFD) Write(ctx context.Context, src usermem.IOSequence, opts vfs.WriteOptions) (int64, error) {
	n := src.NumBytes()
	if n > kernel.SyslogMaxMessage {
		return 0, linuxerr.EINVAL
	}
	msg := make([]byte, n)
	if _, err := src.CopyIn(ctx, msg); err != nil {
		return 0, err
	}
	prio := uint32(kmsgDefaultFacility<<3 | kmsgDefaultLevel)
	if len(msg) > 2 && msg[0] == '<' {
		if end := bytes.IndexByte(msg, '>'); end > 1 {
			if u, err := strconv.ParseUint(string(msg[1:end]), 10, 32); err == nil {
				facility := uint32(u) >> 3
				if facility == 0 {
					facility = kmsgDefaultFacility
				}
				prio = facility<<3 | uint32(u)&7
				msg = msg[end+1:]
			}
		}
	}
	msg = bytes.TrimRight(msg, "\n")
	now := ktime.NowFromContext(ctx).Sub(fd.k.Timekeeper().BootTime())
	fd.k.Syslog().Append(prio, now, string(msg))
	return n, nil
}

// Seek implements vfs.FileDescriptionImpl.Seek.
func (fd *kmsgFD) Seek(ctx context.Context, offset int64, whence int32) (int64, error) {
	if offset != 0 {
		return 0, linuxerr.ESPIPE
	}
	fd.mu.Lock()
	defer fd.mu.Unlock()
	switch whence {
	case linux.SEEK_SET, linux.SEEK_DATA:
		fd.seq = fd.k.Syslog().FirstSeq()
	case linux.SEEK_END:
		fd.seq = fd.k.Syslog().NextSeq()
	default:
		return 0, linuxerr.EINVAL
	}
	return 0, nil
}

// Readiness implements waiter.Waitable.Readiness.
func (fd *kmsgFD) Readiness(mask waiter.EventMask) waiter.EventMask {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	ready := waiter.WritableEvents
	if fd.seq < fd.k.Syslog().NextSeq() {
		ready |= waiter.ReadableEvents
	}
	return mask & ready
}

// EventRegister implements waiter.Waitable.EventRegister.
func (fd *kmsgFD) EventRegister(e *waiter.Entry) error {
	fd.k.Syslog().EventRegister(e)
	return nil
}

// EventUnregister implements waiter.Waitable.EventUnregister.
func (fd *kmsgFD) EventUnregister(e *waiter.Entry) {
	fd.k.Syslog().EventUnregister(e)
}
