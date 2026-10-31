// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package fuse

import (
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/state"
)

// How this package's types load from what builds before TTC's merge of
// upstream gVisor through 2026-10-28 (TTC's dailies through 20261023) saved;
// see pkg/state/migrate.go, and runsc/ttcd/schemas for their shapes.
func init() {
	// Older builds never forced a request.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/sentry/fsimpl/fuse.Request",
		Fields: []string{"requestEntry", "id", "hdr", "data", "async", "noReply"},
		Zero:   []string{"force"},
	})
	// Older builds connected FUSE servers only through /dev/fuse and sent no
	// FUSE_INTERRUPT; a server without FUSE_CREATE says so again at the next
	// create. Their responses waiting for replies get the IDs that they wait
	// under, and are sent unless their requests are still queued.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/sentry/fsimpl/fuse.connection",
		Fields: []string{"connectionRefs", "initialized", "initializedChan", "waitQueue", "fullQueueCh", "numActiveRequests", "completions", "queue", "nextOpID", "writeBuf", "attributeVersion", "connected", "connInitError", "connInitSuccess", "noAccess", "noFlush", "aborted", "numWaiting", "asyncNum", "asyncCongestionThreshold", "asyncNumMax", "maxRead", "maxWrite", "maxPages", "maxActiveRequests", "minor", "atomicOTrunc", "asyncRead", "writebackCache", "bigWrites", "dontMask", "noOpen"},
		Zero:   []string{"hostTransport", "interrupts", "noInterrupt", "noCreate"},
		FixAfterLoad: func(obj any, old map[string]any) {
			conn := obj.(*connection)
			queued := make(map[linux.FUSEOpID]bool)
			for r := conn.queue.Front(); r != nil; r = r.Next() {
				queued[r.hdr.Unique] = true
			}
			for unique, fut := range conn.completions {
				fut.unique = unique
				fut.sent = !queued[unique]
			}
		},
	})
	// connection's migration sets unique and sent; older builds interrupted no
	// request.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/sentry/fsimpl/fuse.futureResponse",
		Fields: []string{"opcode", "hdr", "data", "async", "buf"},
		Zero:   []string{"interrupted", "intrReq"},
		Set:    []string{"unique", "sent"},
	})
	// st_blocks reads 0 until the server's next attribute reply.
	state.RegisterMigration(state.Migration{
		Type:   "pkg/sentry/fsimpl/fuse.inode",
		Fields: []string{"inodeRefs", "InodeNotAnonymous", "InodeNotSymlink", "InodeWatches", "OrderedChildren", "InodeFSOwned", "fs", "nodeID", "generation", "entryTime", "attrVersion", "attrTime", "link", "fh", "locks", "watches", "ino", "uid", "gid", "mode", "atime", "mtime", "ctime", "size", "nlink", "blockSize", "cache"},
		Zero:   []string{"blocks"},
	})
}
