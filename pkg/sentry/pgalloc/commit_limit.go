// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pgalloc

// SetCommitLimit has OverCommitLimit return true once f may have more than
// limit bytes of pages committed: in use, or freed but not yet released. 0 is
// no limit.
//
// A page counts from its allocation, before its user commits it, so that a
// caller that checks OverCommitLimit before letting a page's user at it never
// lets f have more than limit bytes committed, but for pages already handed
// out.
func (f *MemoryFile) SetCommitLimit(limit uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commitLimit = limit
	f.accountLocked(0)
}

// OverCommitLimit returns true if f may have more pages committed than its
// commit limit; see SetCommitLimit.
func (f *MemoryFile) OverCommitLimit() bool {
	return f.overCommitLimit.Load()
}

// AccountedBytes returns the bytes of pages f may have committed: those in
// use, and those freed but not yet released.
func (f *MemoryFile) AccountedBytes() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accountedBytes
}

// HasWaste returns true if f may have waste: pages freed but not yet released,
// which its releaser releases soon after they are freed, unless it is held.
func (f *MemoryFile) HasWaste() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.haveWaste
}

// accountLocked adds delta to f.accountedBytes.
//
// Preconditions: f.mu must be locked.
func (f *MemoryFile) accountLocked(delta int64) {
	f.accountedBytes = uint64(int64(f.accountedBytes) + delta)
	f.overCommitLimit.Store(f.commitLimit != 0 && f.accountedBytes > f.commitLimit)
}

// recountAccountedLocked sets f.accountedBytes from f.memAcct, which a load
// replaced whole.
//
// Preconditions: f.mu must be locked.
func (f *MemoryFile) recountAccountedLocked() {
	f.accountedBytes = 0
	for seg := f.memAcct.FirstSegment(); seg.Ok(); seg = seg.NextSegment() {
		f.accountedBytes += seg.Range().Length()
	}
	f.accountLocked(0)
}
