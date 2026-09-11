// Copyright 2026 The gVisor Authors.
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

package ttcsnapshot

import (
	"fmt"
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
