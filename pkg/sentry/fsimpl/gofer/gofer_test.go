// Copyright 2020 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package gofer

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/lisafs"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/contexttest"
	"gvisor.dev/gvisor/pkg/sentry/ktime"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

func TestBoundedFillRange(t *testing.T) {
	const limit = uint64(1 << 20)
	for _, test := range []struct {
		name               string
		required, optional memmap.MappableRange
		want               memmap.MappableRange
	}{
		{
			name:     "bounded forward readahead",
			required: memmap.MappableRange{Start: 0x2000, End: 0x3000},
			optional: memmap.MappableRange{Start: 0, End: 4 << 20},
			want:     memmap.MappableRange{Start: 0x2000, End: 0x2000 + limit},
		},
		{
			name:     "small optional range",
			required: memmap.MappableRange{Start: 0x2000, End: 0x3000},
			optional: memmap.MappableRange{Start: 0x1000, End: 0x9000},
			want:     memmap.MappableRange{Start: 0x1000, End: 0x9000},
		},
		{
			name:     "large required range",
			required: memmap.MappableRange{Start: 0x2000, End: 0x2000 + 2*limit},
			optional: memmap.MappableRange{Start: 0, End: 4 << 20},
			want:     memmap.MappableRange{Start: 0x2000, End: 0x2000 + 2*limit},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := boundedFillRange(test.required, test.optional, limit); got != test.want {
				t.Fatalf("boundedFillRange(%v, %v, %#x) = %v, want %v", test.required, test.optional, limit, got, test.want)
			}
		})
	}
}

func TestLiveForkFilesystemEligibility(t *testing.T) {
	id := checkpoint.ResourceID{ContainerName: "container", Path: "/data"}
	tests := []struct {
		name     string
		fs       filesystem
		decision vfs.LiveForkFilesystemDecision
		reason   string
	}{
		{name: "directfs", fs: filesystem{opts: filesystemOptions{directfs: directfsOpts{enabled: true}}, iopts: InternalFilesystemOptions{ReadOnly: true, UniqueID: id}}, decision: vfs.LiveForkFilesystemReject, reason: "directfs"},
		{name: "writable server", fs: filesystem{iopts: InternalFilesystemOptions{UniqueID: id}}, decision: vfs.LiveForkFilesystemReject, reason: "not read-only"},
		{name: "missing identity", fs: filesystem{iopts: InternalFilesystemOptions{ReadOnly: true}}, decision: vfs.LiveForkFilesystemReject, reason: "no stable resource ID"},
		{name: "read-only lisafs", fs: filesystem{iopts: InternalFilesystemOptions{ReadOnly: true, UniqueID: id}}, decision: vfs.LiveForkFilesystemShareExternal},
	}
	for i := range tests {
		tc := &tests[i]
		t.Run(tc.name, func(t *testing.T) {
			got := tc.fs.LiveForkFilesystemEligibility()
			if got.Decision != tc.decision {
				t.Errorf("decision: got %q, want %q", got.Decision, tc.decision)
			}
			if tc.reason != "" && !strings.Contains(got.Reason, tc.reason) {
				t.Errorf("reason %q does not contain %q", got.Reason, tc.reason)
			}
			if tc.decision == vfs.LiveForkFilesystemShareExternal {
				if !got.ReadOnly || !got.RequiresReadOnlyMount || got.StableIdentity != id.String() || got.SourceClass != "external-gofer" {
					t.Errorf("share-external eligibility: %+v", got)
				}
			}
		})
	}
}

func TestLiveForkResourcePreservesSourceConnection(t *testing.T) {
	root := &lisafsInode{}
	fs := &filesystem{root: &dentry{inode: &root.inode}}
	root.impl = root
	resource := &liveForkFilesystemResource{source: fs}
	resource.preserveSharedConnection()
	if !fs.iopts.LeakConnection {
		t.Fatal("live-fork resource left the shared LISAFS connection branch-owned")
	}
	if !root.borrowedControlFD {
		t.Fatal("live-fork resource left the shared root FD branch-owned")
	}
}

func TestDestroyIdempotent(t *testing.T) {
	ctx := contexttest.Context(t)
	fs := filesystem{
		mf:         pgalloc.MemoryFileFromContext(ctx),
		inoByKey:   make(map[inoKey]uint64),
		inodeByKey: make(map[inoKey]*inode),
		clock:      ktime.RealtimeClockFromContext(ctx),
		// Test relies on no dentry being held in the cache.
		dentryCache: &dentryCache{maxCachedDentries: 0},
		client:      &lisafs.Client{},
	}

	parentInode := lisafs.Inode{
		ControlFD: 1,
		Stat: lisafs.Statx{
			Mask: linux.STATX_TYPE | linux.STATX_MODE,
			Mode: linux.S_IFDIR | 0666,
		},
	}
	parent, err := fs.newLisafsDentry(ctx, &parentInode)
	if err != nil {
		t.Fatalf("fs.newDentry(): %v", err)
	}

	childInode := lisafs.Inode{
		ControlFD: 2,
		Stat: lisafs.Statx{
			Mask: linux.STATX_TYPE | linux.STATX_MODE | linux.STATX_SIZE,
			Mode: linux.S_IFREG | 0666,
			Size: 0,
		},
	}

	child, err := fs.newLisafsDentry(ctx, &childInode)
	if err != nil {
		t.Fatalf("fs.newDentry(): %v", err)
	}
	parent.opMu.Lock()
	parent.childrenMu.Lock()
	parent.cacheNewChildLocked(child, "child")
	parent.childrenMu.Unlock()
	parent.opMu.Unlock()

	fs.renameMu.Lock()
	defer fs.renameMu.Unlock()
	child.checkCachingLocked(ctx, true /* renameMuWriteLocked */)
	if got := child.refs.Load(); got != -1 {
		t.Fatalf("child.refs=%d, want: -1", got)
	}
	// Parent will also be destroyed when child reference is removed.
	if got := parent.refs.Load(); got != -1 {
		t.Fatalf("parent.refs=%d, want: -1", got)
	}
	child.checkCachingLocked(ctx, true /* renameMuWriteLocked */)
	child.checkCachingLocked(ctx, true /* renameMuWriteLocked */)
}

// A gofer with no descriptors left gets some back.
//
// The cache is where they are: the gofer holds one host descriptor for every
// dentry cached here, and it cannot close what this side still refers to.
// Evicting only when the cache passed its own count left a gofer at EMFILE
// sitting on thousands of dentries nobody was using, and the sandbox failed
// every path it touched from then until it was restarted.
func TestAGoferOutOfDescriptorsHasTheCacheGivenBack(t *testing.T) {
	ctx := contexttest.Context(t)
	fs := filesystem{
		mf:          pgalloc.MemoryFileFromContext(ctx),
		inoByKey:    make(map[inoKey]uint64),
		inodeByKey:  make(map[inoKey]*inode),
		clock:       ktime.RealtimeClockFromContext(ctx),
		dentryCache: &dentryCache{maxCachedDentries: 8},
		client:      &lisafs.Client{},
	}
	parent, err := fs.newLisafsDentry(ctx, &lisafs.Inode{
		ControlFD: 1,
		Stat: lisafs.Statx{
			Mask: linux.STATX_TYPE | linux.STATX_MODE,
			Mode: linux.S_IFDIR | 0666,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Enough idle children to fill the cache.
	for i := 0; i < 8; i++ {
		child, err := fs.newLisafsDentry(ctx, &lisafs.Inode{
			ControlFD: lisafs.FDID(i + 2),
			Stat: lisafs.Statx{
				Mask: linux.STATX_TYPE | linux.STATX_MODE | linux.STATX_SIZE,
				Mode: linux.S_IFREG | 0666,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		parent.opMu.Lock()
		parent.childrenMu.Lock()
		parent.cacheNewChildLocked(child, fmt.Sprintf("child%d", i))
		parent.childrenMu.Unlock()
		parent.opMu.Unlock()
		fs.renameMu.Lock()
		child.checkCachingLocked(ctx, true /* renameMuWriteLocked */)
		fs.renameMu.Unlock()
	}
	fs.dentryCache.mu.Lock()
	filled := fs.dentryCache.dentriesLen
	fs.dentryCache.mu.Unlock()
	if filled == 0 {
		t.Fatal("nothing was cached, so there is nothing this test can be about")
	}

	// Nothing has said anything is wrong, so the cache stays.
	fs.renameMu.Lock()
	parent.checkCachingLocked(ctx, true /* renameMuWriteLocked */)
	fs.renameMu.Unlock()
	fs.dentryCache.mu.Lock()
	quiet := fs.dentryCache.dentriesLen
	fs.dentryCache.mu.Unlock()
	if quiet < filled {
		t.Fatalf("the cache dropped from %d to %d with no shortage reported", filled, quiet)
	}

	// A gofer says it is out. The next dentry that goes idle gives half back —
	// and it need not be one on the connection that was refused, which is the
	// point of keeping this beside the cache.
	fs.dentryCache.starved.Store(true)
	fs.renameMu.Lock()
	parent.checkCachingLocked(ctx, true /* renameMuWriteLocked */)
	fs.renameMu.Unlock()
	fs.dentryCache.mu.Lock()
	after := fs.dentryCache.dentriesLen
	fs.dentryCache.mu.Unlock()
	if after > fs.dentryCache.maxCachedDentries/2 {
		t.Fatalf("a gofer out of descriptors was left holding %d of %d cached dentries", after, quiet)
	}
}

func TestStringFixedCache(t *testing.T) {
	names := []string{"a", "b", "c"}
	cache := stringFixedCache{}

	cache.init(uint64(len(names)))
	if inited := cache.isInited(); !inited {
		t.Fatalf("cache.isInited(): %v, want: true", inited)
	}
	for _, s := range names {
		victim := cache.add(s)
		if victim != "" {
			t.Fatalf("cache.add(): %v, want: \"\"", victim)
		}
	}
	for _, s := range names {
		victim := cache.add("something")
		if victim != s {
			t.Fatalf("cache.add(): %v, want: %v", victim, s)
		}
	}
}

func TestXattrCacheValues(t *testing.T) {
	ino := &inode{}

	// 1. Initial state
	if _, _, found := ino.xattrCache.get("user.foo"); found {
		t.Errorf("get(user.foo) got found=true, want false")
	}

	// 2. Cache present xattr
	ino.xattrCache.add("user.foo", "bar")
	val, negative, found := ino.xattrCache.get("user.foo")
	if !found || negative || val != "bar" {
		t.Errorf("get(user.foo) = (%q, %v, %v), want (%q, false, true)", val, negative, found, "bar")
	}

	// 3. Cache negative xattr
	ino.xattrCache.addNegative("user.neg")
	val, negative, found = ino.xattrCache.get("user.neg")
	if !found || !negative || val != "" {
		t.Errorf("get(user.neg) = (%q, %v, %v), want (%q, true, true)", val, negative, found, "")
	}

	// Overwrite negative xattr with present xattr
	ino.xattrCache.add("user.neg", "baz")
	val, negative, found = ino.xattrCache.get("user.neg")
	if !found || negative || val != "baz" {
		t.Errorf("get(user.neg) = (%q, %v, %v), want (%q, false, true)", val, negative, found, "baz")
	}

	// 4. Overlay opaque xattr
	ino.xattrCache.addNegative(xattrOverlayOpaque)
	val, negative, found = ino.xattrCache.get(xattrOverlayOpaque)
	if !found || !negative || val != "" {
		t.Errorf("get(xattrOverlayOpaque) = (%q, %v, %v), want (%q, true, true)", val, negative, found, "")
	}

	// Overwrite overlay opaque xattr with present xattr
	ino.xattrCache.add(xattrOverlayOpaque, "y")
	val, negative, found = ino.xattrCache.get(xattrOverlayOpaque)
	if !found || negative || val != "y" {
		t.Errorf("get(xattrOverlayOpaque) = (%q, %v, %v), want (%q, false, true)", val, negative, found, "y")
	}
}

func TestXattrCacheList(t *testing.T) {
	ino := &inode{}

	// 1. Initial state
	if _, found := ino.xattrCache.getList(); found {
		t.Errorf("getList() got found=true, want false")
	}

	// 2. Cache empty list
	ino.xattrCache.setList([]string{})
	list, found := ino.xattrCache.getList()
	if !found || len(list) != 0 {
		t.Errorf("getList() = (%v, %v), want ([], true)", list, found)
	}

	// 3. Cache non-empty list
	initialList := []string{"user.foo", "user.bar"}
	ino.xattrCache.setList(initialList)
	list, found = ino.xattrCache.getList()
	if !found || !slices.Equal(list, initialList) {
		t.Errorf("getList() = (%v, %v), want (%v, true)", list, found, initialList)
	}

	// 4. Cache new xattr updates list
	ino.xattrCache.add("user.baz", "val")
	wantList := []string{"user.foo", "user.bar", "user.baz"}
	list, found = ino.xattrCache.getList()
	if !found || !slices.Equal(list, wantList) {
		t.Errorf("getList() = (%v, %v), want (%v, true)", list, found, wantList)
	}

	// 5. Cache existing xattr does not duplicate in list
	ino.xattrCache.add("user.foo", "val2")
	list, found = ino.xattrCache.getList()
	if !found || !slices.Equal(list, wantList) {
		t.Errorf("getList() = (%v, %v), want (%v, true)", list, found, wantList)
	}

	// 6. Cache negative xattr removes from list
	ino.xattrCache.addNegative("user.bar")
	wantList = []string{"user.foo", "user.baz"}
	list, found = ino.xattrCache.getList()
	if !found || !slices.Equal(list, wantList) {
		t.Errorf("getList() = (%v, %v), want (%v, true)", list, found, wantList)
	}
}
