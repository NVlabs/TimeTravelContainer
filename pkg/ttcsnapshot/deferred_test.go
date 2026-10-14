// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcsnapshot

import (
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeferredFilesystemRootLoadsOnlyTraversedMetadata(t *testing.T) {
	source, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	sourceOrigin, err := source.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	operations := []FilesystemOperation{{PutInode: &InodeRecord{Number: 1, Type: "directory", Mode: 0755}}}
	for i := uint64(2); i < 258; i++ {
		operations = append(operations,
			FilesystemOperation{PutInode: &InodeRecord{Number: i, Type: "regular", Mode: 0644}},
			FilesystemOperation{PutEntry: &DirectoryEntryRecord{Parent: 1, Name: fmt.Sprintf("file-%03d", i), Inode: i}},
		)
	}
	sourceRoot, err := source.ApplyFilesystem(sourceOrigin, "source", operations)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Seal(sourceOrigin, SnapshotSealedLocal); err != nil {
		t.Fatal(err)
	}
	sourceObjects := source.Stats().Objects

	targetDir := t.TempDir()
	target, err := Open(targetDir)
	if err != nil {
		t.Fatal(err)
	}
	targetOrigin, err := target.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	targetRoot, err := target.ImportDeferredFilesystemRoot(targetOrigin, "target", "peer-A/tx-1", sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if got := target.Stats().Objects; got != 1 {
		t.Fatalf("initial target objects=%d, want one root stub", got)
	}
	var loads atomic.Int64
	if err := target.AttachDeferredResolver("peer-A/tx-1", targetOrigin, func(id ObjectID) (Object, error) {
		loads.Add(1)
		object, ok := source.Object(id)
		if !ok {
			return Object{}, fmt.Errorf("source object %s is absent", id)
		}
		return object, nil
	}); err != nil {
		t.Fatal(err)
	}
	entry, ok, err := target.FilesystemLookup(targetRoot, 1, "file-129")
	if err != nil || !ok || entry.Inode != 129 {
		t.Fatalf("lookup=(%+v,%t,%v), want inode 129", entry, ok, err)
	}
	if got := target.Stats().Objects; got >= sourceObjects {
		t.Fatalf("one lookup imported %d objects, source graph has %d", got, sourceObjects)
	}
	if got := loads.Load(); got == 0 || got > 32 {
		t.Fatalf("one lookup loaded %d metadata objects, want bounded tree paths", got)
	}

	updated, ok, err := target.FilesystemInode(targetRoot, 129)
	if err != nil || !ok {
		t.Fatalf("inode lookup: found=%t err=%v", ok, err)
	}
	updated.Mode = 0600
	mutatedRoot, err := target.ApplyFilesystem(targetOrigin, "target", []FilesystemOperation{{PutInode: &updated}})
	if err != nil {
		t.Fatalf("mutating deferred filesystem: %v", err)
	}
	got, ok, err := target.FilesystemInode(mutatedRoot, 129)
	if err != nil || !ok || got.Mode != 0600 {
		t.Fatalf("mutated inode=(%+v,%t,%v), want mode 0600", got, ok, err)
	}
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}

	target, err = Open(targetDir)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	root, ok := target.Root("target")
	if !ok {
		t.Fatal("reopened target has no filesystem root")
	}
	if _, _, err := target.FilesystemLookup(root, 1, "file-257"); err == nil {
		t.Fatal("unresolved path succeeded without reattaching its authoritative source")
	}
	if err := target.AttachDeferredResolver("peer-A/tx-1", targetOrigin, func(id ObjectID) (Object, error) {
		object, ok := source.Object(id)
		if !ok {
			return Object{}, fmt.Errorf("source object %s is absent", id)
		}
		return object, nil
	}); err != nil {
		t.Fatal(err)
	}
	if entry, ok, err := target.FilesystemLookup(root, 1, "file-257"); err != nil || !ok || entry.Inode != 257 {
		t.Fatalf("reattached lookup=(%+v,%t,%v), want inode 257", entry, ok, err)
	}
}

// A mutation of a lazily imported filesystem replays from the journal to the
// root it committed, whatever origin the branch writes in. The tree's edges
// are not on the mutation's paths, which it expands before it begins: the
// builder that checks a tree's balance at its edges expanded them as it went,
// and drew the mutation's objects' IDs past those expansions, where the
// replay, which finds the expansions done, did not.
func TestDeferredFilesystemMutationReplaysWithDeferredEdges(t *testing.T) {
	source, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	sourceOrigin, err := source.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	operations := []FilesystemOperation{{PutInode: &InodeRecord{Number: 1, Type: "directory", Mode: 0755}}}
	for i := uint64(2); i < 258; i++ {
		operations = append(operations,
			FilesystemOperation{PutInode: &InodeRecord{Number: i, Type: "regular", Mode: 0644}},
			FilesystemOperation{PutEntry: &DirectoryEntryRecord{Parent: 1, Name: fmt.Sprintf("file-%03d", i), Inode: i}},
		)
	}
	sourceRoot, err := source.ApplyFilesystem(sourceOrigin, "source", operations)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Seal(sourceOrigin, SnapshotSealedLocal); err != nil {
		t.Fatal(err)
	}
	load := func(id ObjectID) (Object, error) {
		object, ok := source.Object(id)
		if !ok {
			return Object{}, fmt.Errorf("source object %s is absent", id)
		}
		return object, nil
	}
	for _, branchOwnOrigin := range []bool{false, true} {
		targetDir := t.TempDir()
		target, err := Open(targetDir)
		if err != nil {
			t.Fatal(err)
		}
		// A remote fork's branch writes in a working origin of its own, made
		// before the import's expansions draw on theirs.
		var branchOrigin SnapshotID
		if branchOwnOrigin {
			if branchOrigin, err = target.AllocateWorking(); err != nil {
				t.Fatal(err)
			}
		}
		importOrigin, err := target.AllocateWorking()
		if err != nil {
			t.Fatal(err)
		}
		if !branchOwnOrigin {
			branchOrigin = importOrigin
		}
		targetRoot, err := target.ImportDeferredFilesystemRoot(importOrigin, "target", "peer-A/tx-1", sourceRoot)
		if err != nil {
			t.Fatal(err)
		}
		if err := target.AttachDeferredResolver("peer-A/tx-1", importOrigin, load); err != nil {
			t.Fatal(err)
		}
		var mutated ObjectID
		for _, number := range []uint64{129, 130} {
			updated, ok, err := target.FilesystemInode(targetRoot, number)
			if err != nil || !ok {
				t.Fatalf("inode %d: found=%t err=%v", number, ok, err)
			}
			updated.Mode = 0600
			if mutated, err = target.ApplyFilesystem(branchOrigin, "target", []FilesystemOperation{{PutInode: &updated}}); err != nil {
				t.Fatalf("mutating inode %d: %v", number, err)
			}
			targetRoot = mutated
		}
		// The graph, copied before its writer closes it, replays the journal,
		// as ttcd does a Sentry's.
		replayed, err := Open(copyGraphForTest(t, targetDir))
		if err != nil {
			t.Fatalf("branch in its own origin %t: replaying the journal: %v", branchOwnOrigin, err)
		}
		if root, ok := replayed.Root("target"); !ok || root != mutated {
			t.Errorf("branch in its own origin %t: replayed root=(%s, %t), want %s", branchOwnOrigin, root, ok, mutated)
		}
		if err := replayed.Close(); err != nil {
			t.Fatal(err)
		}
		if err := target.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// A mutation that leaves a lazily imported tree's root one child, which no
// edited path reached, expands the child to read it. It is built again, after
// the expansion, and replays from the journal to the root it committed.
func TestDeferredFilesystemMutationReplaysAfterExpandingOffItsPaths(t *testing.T) {
	source, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	sourceOrigin, err := source.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	// Inodes 1 to 31 fill the inode tree's first leaf, and 32 is its second's.
	operations := []FilesystemOperation{{PutInode: &InodeRecord{Number: 1, Type: "directory", Mode: 0755}}}
	for i := uint64(2); i <= bTreeMaxEntries+1; i++ {
		operations = append(operations,
			FilesystemOperation{PutInode: &InodeRecord{Number: i, Type: "regular", Mode: 0644}},
			FilesystemOperation{PutEntry: &DirectoryEntryRecord{Parent: 1, Name: fmt.Sprintf("file-%03d", i), Inode: i}},
		)
	}
	sourceRoot, err := source.ApplyFilesystem(sourceOrigin, "source", operations)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Seal(sourceOrigin, SnapshotSealedLocal); err != nil {
		t.Fatal(err)
	}
	targetDir := t.TempDir()
	target, err := Open(targetDir)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	// The expansion draws on the import's origin, whose counter is past the
	// branch's.
	branchOrigin, err := target.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	importOrigin, err := target.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.ImportDeferredFilesystemRoot(importOrigin, "target", "peer-A/tx-1", sourceRoot); err != nil {
		t.Fatal(err)
	}
	if err := target.AttachDeferredResolver("peer-A/tx-1", importOrigin, func(id ObjectID) (Object, error) {
		object, ok := source.Object(id)
		if !ok {
			return Object{}, fmt.Errorf("source object %s is absent", id)
		}
		return object, nil
	}); err != nil {
		t.Fatal(err)
	}
	mutated, err := target.ApplyFilesystem(branchOrigin, "target", []FilesystemOperation{{DeleteInode: bTreeMaxEntries + 1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := target.FilesystemInode(mutated, bTreeMaxEntries+1); err != nil || ok {
		t.Fatalf("deleted inode: found=%t err=%v", ok, err)
	}
	if inode, ok, err := target.FilesystemInode(mutated, bTreeMaxEntries); err != nil || !ok || inode.Mode != 0644 {
		t.Fatalf("inode %d=(%+v, %t, %v), want it kept", bTreeMaxEntries, inode, ok, err)
	}
	replayed, err := Open(copyGraphForTest(t, targetDir))
	if err != nil {
		t.Fatalf("replaying the journal: %v", err)
	}
	defer replayed.Close()
	if root, ok := replayed.Root("target"); !ok || root != mutated {
		t.Errorf("replayed root=(%s, %t), want %s", root, ok, mutated)
	}
}

func TestDeferredExpansionWaitsForExternalWriterFence(t *testing.T) {
	source, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	sourceOrigin, err := source.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	sourceRoot, err := source.CreateObject(sourceOrigin, "source-root", ObjectKindMetadata, []byte("root"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.SetRoot("source", sourceRoot); err != nil {
		t.Fatal(err)
	}
	if err := source.DeletePin("source-root"); err != nil {
		t.Fatal(err)
	}
	if err := source.Seal(sourceOrigin, SnapshotSealedLocal); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	writer, err := OpenExternallyCoordinated(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := writer.BeginExternalTransaction(); err != nil {
		t.Fatal(err)
	}
	targetOrigin, err := writer.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	targetRoot, err := writer.ImportDeferredFilesystemRoot(targetOrigin, "target", "peer", sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.EndExternalTransaction(); err != nil {
		t.Fatal(err)
	}

	reader, err := OpenExternallyCoordinated(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err := reader.AttachDeferredResolver("peer", targetOrigin, func(id ObjectID) (Object, error) {
		object, ok := source.Object(id)
		if !ok {
			return Object{}, fmt.Errorf("source object %s is absent", id)
		}
		return object, nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := writer.BeginExternalTransaction(); err != nil {
		t.Fatal(err)
	}
	next, err := writer.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := reader.ResolveObject(targetRoot)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("deferred expansion bypassed active writer fence: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := writer.EndExternalTransaction(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deferred expansion did not proceed after writer fence release")
	}
	if err := reader.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, ok := reader.Snapshot(next); !ok {
		t.Fatalf("deferred expansion lost concurrently committed snapshot %d", next)
	}
	if err := reader.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestDeferredResolutionAllowsReentrantAncestorLookup(t *testing.T) {
	source, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	sourceOrigin, err := source.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	sourceRoot, err := source.ApplyFilesystem(sourceOrigin, "source", []FilesystemOperation{
		{PutInode: &InodeRecord{Number: 1, Type: "directory", Mode: 0755}},
		{PutInode: &InodeRecord{Number: 2, Type: "regular", Mode: 0644}},
		{PutEntry: &DirectoryEntryRecord{Parent: 1, Name: "value", Inode: 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Seal(sourceOrigin, SnapshotSealedLocal); err != nil {
		t.Fatal(err)
	}

	target, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	targetOrigin, err := target.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	innerRoot, err := target.ImportDeferredFilesystemRoot(targetOrigin, "inner", "peer-inner", sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	outerRoot, err := target.ImportDeferredFilesystemRoot(targetOrigin, "outer", "peer-outer", sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	loadSource := func(id ObjectID) (Object, error) {
		object, ok := source.Object(id)
		if !ok {
			return Object{}, fmt.Errorf("source object %s is absent", id)
		}
		return object, nil
	}
	if err := target.AttachDeferredResolver("peer-inner", targetOrigin, loadSource); err != nil {
		t.Fatal(err)
	}
	var outerLoads atomic.Int64
	if err := target.AttachDeferredResolver("peer-outer", targetOrigin, func(id ObjectID) (Object, error) {
		outerLoads.Add(1)
		// A multi-hop provider may need to expand a different local stub in
		// this same graph before it can answer the outer request.
		if _, err := target.ResolveObject(innerRoot); err != nil {
			return Object{}, err
		}
		return loadSource(id)
	}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		entry, ok, err := target.FilesystemLookup(outerRoot, 1, "value")
		if err == nil && (!ok || entry.Inode != 2) {
			err = fmt.Errorf("lookup=(%+v,%t), want inode 2", entry, ok)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reentrant deferred resolution deadlocked")
	}
	if outerLoads.Load() == 0 {
		t.Fatal("outer deferred resolver was not called")
	}
}

func TestDeferredResolutionSingleFlightsProviderRead(t *testing.T) {
	source, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	sourceOrigin, err := source.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	sourceRoot, err := source.ApplyFilesystem(sourceOrigin, "source", []FilesystemOperation{{PutInode: &InodeRecord{Number: 1, Type: "directory", Mode: 0755}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Seal(sourceOrigin, SnapshotSealedLocal); err != nil {
		t.Fatal(err)
	}
	target, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	targetOrigin, err := target.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	root, err := target.ImportDeferredFilesystemRoot(targetOrigin, "target", "peer", sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var loads atomic.Int64
	if err := target.AttachDeferredResolver("peer", targetOrigin, func(id ObjectID) (Object, error) {
		if loads.Add(1) == 1 {
			close(entered)
		}
		<-release
		object, ok := source.Object(id)
		if !ok {
			return Object{}, fmt.Errorf("source object %s is absent", id)
		}
		return object, nil
	}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := target.ResolveObject(root)
			errs <- err
		}()
	}
	<-entered
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := loads.Load(); got != 1 {
		t.Fatalf("provider loads=%d, want one", got)
	}
}

// A walk of a whole deferred filesystem expands it many objects to a journal
// record, to the graph the source has.
func TestDeferredFilesystemRecordsExpandManyToARecord(t *testing.T) {
	source, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	sourceOrigin, err := source.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	operations := []FilesystemOperation{
		{PutInode: &InodeRecord{Number: 1, Type: "directory", Mode: 0755, Nlink: 4}},
		{PutInode: &InodeRecord{Number: 2, Type: "directory", Mode: 0755, Nlink: 2}},
		{PutInode: &InodeRecord{Number: 3, Type: "directory", Mode: 0700, Nlink: 2}},
		{PutEntry: &DirectoryEntryRecord{Parent: 1, Name: "big", Inode: 2}},
		{PutEntry: &DirectoryEntryRecord{Parent: 1, Name: "small", Inode: 3}},
	}
	for i := uint64(10); i < 1010; i++ {
		parent := uint64(2)
		if i >= 710 {
			parent = 3
		}
		operations = append(operations,
			FilesystemOperation{PutInode: &InodeRecord{Number: i, Type: "regular", Mode: 0644, Nlink: 1, Size: int64(i)}},
			FilesystemOperation{PutEntry: &DirectoryEntryRecord{Parent: parent, Name: fmt.Sprintf("file-%04d", i), Inode: i}},
		)
	}
	// A second name for one file.
	operations = append(operations, FilesystemOperation{PutEntry: &DirectoryEntryRecord{Parent: 3, Name: "link", Inode: 10}})
	sourceRoot, err := source.ApplyFilesystem(sourceOrigin, "source", operations)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Seal(sourceOrigin, SnapshotSealedLocal); err != nil {
		t.Fatal(err)
	}
	wantInodes, wantEntries, err := source.FilesystemRecords(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}

	targetDir := t.TempDir()
	target, err := Open(targetDir)
	if err != nil {
		t.Fatal(err)
	}
	targetOrigin, err := target.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	targetRoot, err := target.ImportDeferredFilesystemRoot(targetOrigin, "target", "peer-A/tx-1", sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := target.AttachDeferredResolver("peer-A/tx-1", targetOrigin, func(id ObjectID) (Object, error) {
		object, ok := source.Object(id)
		if !ok {
			return Object{}, fmt.Errorf("source object %s is absent", id)
		}
		return object, nil
	}); err != nil {
		t.Fatal(err)
	}
	before := target.state.Sequence
	inodes, entries, err := target.FilesystemRecords(targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	records := target.state.Sequence - before
	expanded := uint64(target.Stats().Objects)
	if records == 0 || records*10 > expanded {
		t.Fatalf("expanding %d objects took %d journal records", expanded, records)
	}
	sameRecords := func(inodes []InodeRecord, entries []DirectoryEntryRecord) {
		t.Helper()
		if len(inodes) != len(wantInodes) || !reflect.DeepEqual(entries, wantEntries) {
			t.Fatalf("walked %d inodes and %d entries, want %d and %d", len(inodes), len(entries), len(wantInodes), len(wantEntries))
		}
		for index, inode := range inodes {
			// The target names the directories' trees by its own IDs.
			want := wantInodes[index]
			if want.Directory.IsZero() != inode.Directory.IsZero() {
				t.Fatalf("inode %d has directory %s, want %s", inode.Number, inode.Directory, want.Directory)
			}
			inode.Directory, want.Directory = ObjectID{}, ObjectID{}
			if !reflect.DeepEqual(inode, want) {
				t.Fatalf("inode %+v, want %+v", inode, want)
			}
		}
	}
	sameRecords(inodes, entries)
	if err := validateGraphState(&target.state); err != nil {
		t.Fatalf("the expanded graph is invalid: %v", err)
	}
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
	// Replayed, the graph needs its source no more.
	target, err = Open(targetDir)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	inodes, entries, err = target.FilesystemRecords(targetRoot)
	if err != nil {
		t.Fatalf("walking the replayed graph without its source: %v", err)
	}
	sameRecords(inodes, entries)
}

// A provider may expand a stub of the graph it answers for, even one of the
// batch it answers.
func TestDeferredBatchAllowsReentrantExpansion(t *testing.T) {
	source, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	sourceOrigin, err := source.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	operations := []FilesystemOperation{{PutInode: &InodeRecord{Number: 1, Type: "directory", Mode: 0755}}}
	for i := uint64(2); i < 300; i++ {
		operations = append(operations,
			FilesystemOperation{PutInode: &InodeRecord{Number: i, Type: "regular", Mode: 0644}},
			FilesystemOperation{PutEntry: &DirectoryEntryRecord{Parent: 1, Name: fmt.Sprintf("file-%03d", i), Inode: i}},
		)
	}
	sourceRoot, err := source.ApplyFilesystem(sourceOrigin, "source", operations)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Seal(sourceOrigin, SnapshotSealedLocal); err != nil {
		t.Fatal(err)
	}
	wantInodes, wantEntries, err := source.FilesystemRecords(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	target, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	targetOrigin, err := target.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	targetRoot, err := target.ImportDeferredFilesystemRoot(targetOrigin, "target", "peer-A", sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	var reentered, depth atomic.Int64
	if err := target.AttachDeferredResolver("peer-A", targetOrigin, func(id ObjectID) (Object, error) {
		// Expand another stub of this catalog first, as a multi-hop
		// provider may; not from within that expansion, whose own object is
		// being loaded up the stack.
		if depth.Add(1) == 1 {
			target.mu.RLock()
			var other ObjectID
			for _, object := range target.state.Objects {
				if object.Deferred && object.SourceObject != id {
					other = object.ID
					break
				}
			}
			target.mu.RUnlock()
			if !other.IsZero() {
				reentered.Add(1)
				if _, err := target.ResolveObject(other); err != nil {
					depth.Add(-1)
					return Object{}, err
				}
			}
		}
		depth.Add(-1)
		object, ok := source.Object(id)
		if !ok {
			return Object{}, fmt.Errorf("source object %s is absent", id)
		}
		return object, nil
	}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		inodes, entries, err := target.FilesystemRecords(targetRoot)
		if err == nil && (len(inodes) != len(wantInodes) || !reflect.DeepEqual(entries, wantEntries)) {
			err = fmt.Errorf("walked %d inodes and %d entries, want %d and %d", len(inodes), len(entries), len(wantInodes), len(wantEntries))
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a reentrant expansion deadlocked the batch")
	}
	if reentered.Load() == 0 {
		t.Fatal("the provider never expanded another stub")
	}
	if err := validateGraphState(&target.state); err != nil {
		t.Fatalf("the expanded graph is invalid: %v", err)
	}
}
