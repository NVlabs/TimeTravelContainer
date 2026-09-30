// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package binder

import (
	"encoding/binary"
)

// Linux's binder UAPI, include/uapi/linux/android/binder.h and binderfs.h,
// for 64-bit processes (binder_size_t and binder_uintptr_t are 64 bits).

// ProtocolVersion is BINDER_CURRENT_PROTOCOL_VERSION for 64-bit processes.
const ProtocolVersion = 8

// ioc encodes an ioctl number, as Linux's _IOC does.
func ioc(dir, typ, nr, size uint32) uint32 {
	return dir<<30 | size<<16 | typ<<8 | nr
}

const (
	iocNone  = 0
	iocWrite = 1
	iocRead  = 2
)

func ionone(typ, nr uint32) uint32     { return ioc(iocNone, typ, nr, 0) }
func iow(typ, nr, size uint32) uint32  { return ioc(iocWrite, typ, nr, size) }
func ior(typ, nr, size uint32) uint32  { return ioc(iocRead, typ, nr, size) }
func iowr(typ, nr, size uint32) uint32 { return ioc(iocWrite|iocRead, typ, nr, size) }

// iocSize returns the argument size of an ioctl number or command.
func iocSize(cmd uint32) uint32 {
	return cmd >> 16 & 0x3fff
}

// Sizes of the UAPI's structs.
const (
	sizeofWriteRead             = 48
	sizeofTransactionData       = 64
	sizeofTransactionDataSecctx = 72
	sizeofTransactionDataSG     = 72
	sizeofPtrCookie             = 16
	sizeofHandleCookie          = 12
	sizeofFlatObject            = 24
	sizeofFDObject              = 24
	sizeofBufferObject          = 40
	sizeofFDArrayObject         = 32
	sizeofNodeDebugInfo         = 24
	sizeofNodeInfoForRef        = 24
	sizeofFreezeInfo            = 12
	sizeofFrozenStatusInfo      = 12
	sizeofExtendedError         = 12
	sizeofBinderfsDevice        = 264
)

// The binder device's ioctls.
var (
	ioctlWriteRead                 = iowr('b', 1, sizeofWriteRead)
	ioctlSetIdleTimeout            = iow('b', 3, 8)
	ioctlSetMaxThreads             = iow('b', 5, 4)
	ioctlSetIdlePriority           = iow('b', 6, 4)
	ioctlSetContextMgr             = iow('b', 7, 4)
	ioctlThreadExit                = iow('b', 8, 4)
	ioctlVersion                   = iowr('b', 9, 4)
	ioctlGetNodeDebugInfo          = iowr('b', 11, sizeofNodeDebugInfo)
	ioctlGetNodeInfoForRef         = iowr('b', 12, sizeofNodeInfoForRef)
	ioctlSetContextMgrExt          = iow('b', 13, sizeofFlatObject)
	ioctlFreeze                    = iow('b', 14, sizeofFreezeInfo)
	ioctlGetFrozenInfo             = iowr('b', 15, sizeofFrozenStatusInfo)
	ioctlEnableOnewaySpamDetection = iow('b', 16, 4)
	ioctlGetExtendedError          = iowr('b', 17, sizeofExtendedError)
)

// ioctlCtlAdd is binderfs' BINDER_CTL_ADD, on binder-control.
var ioctlCtlAdd = iowr('b', 1, sizeofBinderfsDevice)

// Commands that processes write, BC_*.
var (
	bcTransaction               = iow('c', 0, sizeofTransactionData)
	bcReply                     = iow('c', 1, sizeofTransactionData)
	bcAcquireResult             = iow('c', 2, 4)
	bcFreeBuffer                = iow('c', 3, 8)
	bcIncrefs                   = iow('c', 4, 4)
	bcAcquire                   = iow('c', 5, 4)
	bcRelease                   = iow('c', 6, 4)
	bcDecrefs                   = iow('c', 7, 4)
	bcIncrefsDone               = iow('c', 8, sizeofPtrCookie)
	bcAcquireDone               = iow('c', 9, sizeofPtrCookie)
	bcAttemptAcquire            = iow('c', 10, 8)
	bcRegisterLooper            = ionone('c', 11)
	bcEnterLooper               = ionone('c', 12)
	bcExitLooper                = ionone('c', 13)
	bcRequestDeathNotification  = iow('c', 14, sizeofHandleCookie)
	bcClearDeathNotification    = iow('c', 15, sizeofHandleCookie)
	bcDeadBinderDone            = iow('c', 16, 8)
	bcTransactionSG             = iow('c', 17, sizeofTransactionDataSG)
	bcReplySG                   = iow('c', 18, sizeofTransactionDataSG)
	bcRequestFreezeNotification = iow('c', 19, sizeofHandleCookie)
	bcClearFreezeNotification   = iow('c', 20, sizeofHandleCookie)
	bcFreezeNotificationDone    = iow('c', 21, 8)
)

// Returns that the driver writes, BR_*.
var (
	brError                      = ior('r', 0, 4)
	brOK                         = ionone('r', 1)
	brTransactionSecCtx          = ior('r', 2, sizeofTransactionDataSecctx)
	brTransaction                = ior('r', 2, sizeofTransactionData)
	brReply                      = ior('r', 3, sizeofTransactionData)
	brDeadReply                  = ionone('r', 5)
	brTransactionComplete        = ionone('r', 6)
	brIncrefs                    = ior('r', 7, sizeofPtrCookie)
	brAcquire                    = ior('r', 8, sizeofPtrCookie)
	brRelease                    = ior('r', 9, sizeofPtrCookie)
	brDecrefs                    = ior('r', 10, sizeofPtrCookie)
	brNoop                       = ionone('r', 12)
	brSpawnLooper                = ionone('r', 13)
	brDeadBinder                 = ior('r', 15, 8)
	brClearDeathNotificationDone = ior('r', 16, 8)
	brFailedReply                = ionone('r', 17)
	brFrozenReply                = ionone('r', 18)
	brOnewaySpamSuspect          = ionone('r', 19)
)

// Object types, BINDER_TYPE_*.
func packChars(c1, c2, c3, c4 byte) uint32 {
	return uint32(c1)<<24 | uint32(c2)<<16 | uint32(c3)<<8 | uint32(c4)
}

const typeLarge = 0x85

var (
	typeBinder     = packChars('s', 'b', '*', typeLarge)
	typeWeakBinder = packChars('w', 'b', '*', typeLarge)
	typeHandle     = packChars('s', 'h', '*', typeLarge)
	typeWeakHandle = packChars('w', 'h', '*', typeLarge)
	typeFD         = packChars('f', 'd', '*', typeLarge)
	typeFDA        = packChars('f', 'd', 'a', typeLarge)
	typePtr        = packChars('p', 't', '*', typeLarge)
)

// Flags of flat_binder_object.
const (
	flatFlagPriorityMask   = 0xff
	flatFlagAcceptsFDs     = 0x100
	flatFlagTxnSecurityCtx = 0x1000
)

// bufferFlagHasParent is binder_buffer_object's BINDER_BUFFER_FLAG_HAS_PARENT.
const bufferFlagHasParent = 0x01

// Transaction flags.
const (
	tfOneWay     = 0x01
	tfRootObject = 0x04
	tfStatusCode = 0x08
	tfAcceptFDs  = 0x10
	tfClearBuf   = 0x20
	tfUpdateTxn  = 0x40
)

// Looper states of threads, BINDER_LOOPER_STATE_*.
const (
	looperRegistered = 0x01
	looperEntered    = 0x02
	looperExited     = 0x04
	looperInvalid    = 0x08
	looperWaiting    = 0x10
	looperPoll       = 0x20
)

// maxBufferSize bounds a process' buffer mapping, as Linux's
// binder_alloc_mmap_handler does.
const maxBufferSize = 4 << 20

var le = binary.LittleEndian

// writeRead is struct binder_write_read.
type writeRead struct {
	writeSize, writeConsumed, writeBuffer uint64
	readSize, readConsumed, readBuffer    uint64
}

func (w *writeRead) decode(b []byte) {
	w.writeSize = le.Uint64(b[0:])
	w.writeConsumed = le.Uint64(b[8:])
	w.writeBuffer = le.Uint64(b[16:])
	w.readSize = le.Uint64(b[24:])
	w.readConsumed = le.Uint64(b[32:])
	w.readBuffer = le.Uint64(b[40:])
}

func (w *writeRead) encode(b []byte) {
	le.PutUint64(b[0:], w.writeSize)
	le.PutUint64(b[8:], w.writeConsumed)
	le.PutUint64(b[16:], w.writeBuffer)
	le.PutUint64(b[24:], w.readSize)
	le.PutUint64(b[32:], w.readConsumed)
	le.PutUint64(b[40:], w.readBuffer)
}

// transactionData is struct binder_transaction_data.
type transactionData struct {
	// target is the handle, for BC_TRANSACTION, or the node's pointer, for
	// BR_TRANSACTION.
	target      uint64
	cookie      uint64
	code        uint32
	flags       uint32
	senderPID   int32
	senderEUID  uint32
	dataSize    uint64
	offsetsSize uint64
	buffer      uint64
	offsets     uint64
}

func (d *transactionData) decode(b []byte) {
	d.target = le.Uint64(b[0:])
	d.cookie = le.Uint64(b[8:])
	d.code = le.Uint32(b[16:])
	d.flags = le.Uint32(b[20:])
	d.senderPID = int32(le.Uint32(b[24:]))
	d.senderEUID = le.Uint32(b[28:])
	d.dataSize = le.Uint64(b[32:])
	d.offsetsSize = le.Uint64(b[40:])
	d.buffer = le.Uint64(b[48:])
	d.offsets = le.Uint64(b[56:])
}

func (d *transactionData) encode(b []byte) {
	le.PutUint64(b[0:], d.target)
	le.PutUint64(b[8:], d.cookie)
	le.PutUint32(b[16:], d.code)
	le.PutUint32(b[20:], d.flags)
	le.PutUint32(b[24:], uint32(d.senderPID))
	le.PutUint32(b[28:], d.senderEUID)
	le.PutUint64(b[32:], d.dataSize)
	le.PutUint64(b[40:], d.offsetsSize)
	le.PutUint64(b[48:], d.buffer)
	le.PutUint64(b[56:], d.offsets)
}

// align8 rounds n up to a multiple of 8.
func align8(n uint64) uint64 {
	return (n + 7) &^ 7
}
