// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcvolume

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/ttcsnapshot"
)

type testSharedCatalog struct{ store *ttcsnapshot.SharedPackStore }

type testRemoteCatalog interface {
	RemoteObjectReader
	RemoteObjectCatalog
}

func (c testSharedCatalog) RemoteObject(id ttcsnapshot.ObjectID) (ttcsnapshot.Object, error) {
	return c.store.Object(id)
}

func (c testSharedCatalog) ReadObjectRange(object string, destination []byte, offset int64) (int, error) {
	id, err := ttcsnapshot.ParseObjectID(object)
	if err != nil {
		return 0, err
	}
	return c.store.ReadObjectRange(context.Background(), id, destination, offset)
}

func attachTestRemote(t *testing.T, source, target *Repository, sourceBranch, targetBranch string, reader testRemoteCatalog) <-chan error {
	t.Helper()
	server, client := net.Pipe()
	var token RemoteToken
	copy(token[:], []byte("graph-only-token"))
	key := RemoteSourceKey(sha256.Sum256([]byte("rootfs-upper")))
	serverErr := make(chan error, 1)
	go func() {
		objects, err := source.BranchObjects(sourceBranch)
		if err != nil {
			serverErr <- err
			return
		}
		entry := RemoteObjectSource{Key: key, Repository: source, Objects: objects}
		if reader != nil {
			entry = RemoteObjectSource{Key: key, Reader: reader, Catalog: reader}
		}
		serverErr <- ServeRemoteObjects(server, token, []RemoteObjectSource{entry})
	}()
	if err := target.AttachRemoteObjectSource(client, token, key, targetBranch); err != nil {
		t.Fatal(err)
	}
	return serverErr
}

func TestRemoteClientPreservesFirstTerminalProtocolError(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	var token RemoteToken
	copy(token[:], []byte("terminal-error"))
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer server.Close()
		request, err := readRemoteHeader(server)
		if err != nil {
			return
		}
		_ = writeRemoteFailure(server, request, "authoritative object is unavailable")
	}()
	remote := &remoteObjectClient{conn: client, token: token}
	_, first := remote.request(remotePull, "object", 0, 1)
	if first == nil || !strings.Contains(first.Error(), "authoritative object is unavailable") {
		t.Fatalf("first request error=%v", first)
	}
	<-done
	_, second := remote.request(remotePull, "object", 0, 1)
	if second == nil || second.Error() != first.Error() {
		t.Fatalf("second request error=%v, want preserved %q", second, first)
	}
}

func finishTestRemote(t *testing.T, target *Repository, serverErr <-chan error) {
	t.Helper()
	if err := target.MaterializeRemoteObjects(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-serverErr:
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("source stream remained open after target materialization")
	}
}

func TestRemoteDescriptorFetchesWholeFileAndPrivatizesWrites(t *testing.T) {
	sourceManager := openTestManager(t)
	source, err := sourceManager.Create("source")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	ino, err := source.CreateRegular(MainBranch, RootInode, "large", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 2*remoteExtentBytes+12345)
	for i := range payload {
		payload[i] = byte(i*31 + 7)
	}
	if n, _, err := source.WriteAt(MainBranch, ino, payload, 0); err != nil || n != len(payload) {
		t.Fatalf("WriteAt: %d, %v", n, err)
	}
	if err := source.Fork(MainBranch, "sealed"); err != nil {
		t.Fatal(err)
	}
	descriptor, err := source.DescribeRemoteSnapshot("sealed")
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.Snapshot.LogicalBytes != uint64(len(payload)) {
		t.Fatalf("logical bytes=%d, want %d", descriptor.Snapshot.LogicalBytes, len(payload))
	}
	targetManager := openTestManager(t)
	target, err := targetManager.ImportPrivateRemoteDescriptor(filepath.Join(targetManager.root, "target"), "target", descriptor, "imported")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	serverErr := attachTestRemote(t, source, target, "sealed", "imported", nil)
	var materialized []string
	unregister := target.RegisterMaterializationHook(func(object string) { materialized = append(materialized, object) })
	defer unregister()
	got := make([]byte, 4096)
	offset := int64(remoteExtentBytes + 123)
	if n, err := target.ReadAt("imported", ino, got, offset); err != nil || n != len(got) {
		t.Fatalf("remote ReadAt: %d, %v", n, err)
	}
	if !bytes.Equal(got, payload[offset:offset+int64(len(got))]) || len(materialized) != 1 {
		t.Fatalf("whole-file materialization mismatch; hooks=%v", materialized)
	}
	if n, cloned, err := target.WriteAt("imported", ino, []byte("TARGET"), 17); err != nil || n != 6 || !cloned {
		t.Fatalf("target WriteAt: n=%d cloned=%t err=%v", n, cloned, err)
	}
	if got := readAll(t, source, "sealed", ino); !bytes.Equal([]byte(got), payload) {
		t.Fatal("target write changed source")
	}
	finishTestRemote(t, target, serverErr)
}

func TestSyncDataMaterializesRemoteWholeFile(t *testing.T) {
	sourceManager := openTestManager(t)
	source, err := sourceManager.Create("source")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	ino, err := source.CreateRegular(MainBranch, RootInode, "database-wal", 0600, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("sqlite-wal"), 8192)
	if n, _, err := source.WriteAt(MainBranch, ino, payload, 0); err != nil || n != len(payload) {
		t.Fatalf("WriteAt: %d, %v", n, err)
	}
	if err := source.Fork(MainBranch, "sealed"); err != nil {
		t.Fatal(err)
	}
	descriptor, err := source.DescribeRemoteSnapshot("sealed")
	if err != nil {
		t.Fatal(err)
	}
	targetManager := openTestManager(t)
	target, err := targetManager.ImportPrivateRemoteDescriptor(filepath.Join(targetManager.root, "target"), "target", descriptor, "imported")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	serverErr := attachTestRemote(t, source, target, "sealed", "imported", nil)

	// No read, write, or mmap fault precedes this synchronization. This models
	// SQLite recovery immediately fsyncing an inherited WAL after migration.
	if err := target.SyncData("imported", ino, false); err != nil {
		t.Fatalf("SyncData on untouched remote file: %v", err)
	}
	got := make([]byte, len(payload))
	if n, err := target.ReadAt("imported", ino, got, 0); err != nil || n != len(got) {
		t.Fatalf("ReadAt after SyncData: %d, %v", n, err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("SyncData installed incorrect remote contents")
	}
	finishTestRemote(t, target, serverErr)
}

func TestMaterializationHookMayEnterRepositoryMutation(t *testing.T) {
	sourceManager := openTestManager(t)
	source, err := sourceManager.Create("source")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	ino, err := source.CreateRegular(MainBranch, RootInode, "artifact", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := source.WriteAt(MainBranch, ino, []byte("remote data"), 0); err != nil {
		t.Fatal(err)
	}
	if err := source.Fork(MainBranch, "sealed"); err != nil {
		t.Fatal(err)
	}
	descriptor, err := source.DescribeRemoteSnapshot("sealed")
	if err != nil {
		t.Fatal(err)
	}
	targetManager := openTestManager(t)
	target, err := targetManager.ImportPrivateRemoteDescriptor(filepath.Join(targetManager.root, "target"), "target", descriptor, "imported")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	serverErr := attachTestRemote(t, source, target, "sealed", "imported", nil)
	unregister := target.RegisterMaterializationHook(func(string) {
		// Runtime hooks acquire their own file locks before repository mutation.
		// Entering withLock here is a compact regression model for that ordering.
		if err := target.withLock(func() error { return nil }); err != nil {
			t.Errorf("hook repository mutation: %v", err)
		}
	})
	defer unregister()
	done := make(chan error, 1)
	go func() { done <- target.MaterializeRemoteObjects() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("remote materialization deadlocked with its runtime invalidation hook")
	}
	select {
	case err := <-serverErr:
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("source stream remained open after target materialization")
	}
}

func TestRemoteRestorePinDefersRepositoryMaterialization(t *testing.T) {
	sourceManager := openTestManager(t)
	source, err := sourceManager.Create("source")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	ino, err := source.CreateRegular(MainBranch, RootInode, "artifact", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := source.WriteAt(MainBranch, ino, []byte("remote data"), 0); err != nil {
		t.Fatal(err)
	}
	if err := source.Fork(MainBranch, "sealed"); err != nil {
		t.Fatal(err)
	}
	descriptor, err := source.DescribeRemoteSnapshot("sealed")
	if err != nil {
		t.Fatal(err)
	}
	targetManager := openTestManager(t)
	target, err := targetManager.ImportPrivateRemoteDescriptor(filepath.Join(targetManager.root, "target"), "target", descriptor, "imported")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	release := target.PinRemoteRestore()
	serverErr := attachTestRemote(t, source, target, "sealed", "imported", nil)
	done := make(chan error, 1)
	go func() { done <- target.MaterializeRemoteObjects() }()
	select {
	case err := <-done:
		t.Fatalf("materialization passed an active restore pin: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	// Lazy catalog expansion must remain available for the whole restore, not
	// merely for the AttachRemoteObjectSource call.
	if _, err := target.Lookup("imported", RootInode, "artifact"); err != nil {
		t.Fatalf("lookup while restore is pinned: %v", err)
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("materialization remained blocked after restore pin release")
	}
	select {
	case err := <-serverErr:
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("source stream remained open after materialization")
	}
}

func TestMaterializeRemoteObjectsRacesLazyCatalogExpansion(t *testing.T) {
	const files = 128
	sourceManager := openTestManager(t)
	source, err := sourceManager.Create("source")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	for i := 0; i < files; i++ {
		name := fmt.Sprintf("artifact-%03d", i)
		ino, err := source.CreateRegular(MainBranch, RootInode, name, 0644, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := source.WriteAt(MainBranch, ino, []byte(name), 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := source.Fork(MainBranch, "sealed"); err != nil {
		t.Fatal(err)
	}
	descriptor, err := source.DescribeRemoteSnapshot("sealed")
	if err != nil {
		t.Fatal(err)
	}
	targetManager := openTestManager(t)
	target, err := targetManager.ImportPrivateRemoteDescriptor(filepath.Join(targetManager.root, "target"), "target", descriptor, "imported")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	serverErr := attachTestRemote(t, source, target, "sealed", "imported", nil)

	start := make(chan struct{})
	errs := make(chan error, files+1)
	var wg sync.WaitGroup
	for i := 0; i < files; i++ {
		name := fmt.Sprintf("artifact-%03d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := target.Lookup("imported", RootInode, name)
			if err != nil {
				errs <- fmt.Errorf("lookup %s: %w", name, err)
			}
		}()
	}
	materialized := make(chan error, 1)
	go func() {
		<-start
		materialized <- target.MaterializeRemoteObjects()
	}()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if err := <-materialized; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < files; i++ {
		name := fmt.Sprintf("artifact-%03d", i)
		ino, err := target.Lookup("imported", RootInode, name)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(name))
		if n, err := target.ReadAt("imported", ino, got, 0); err != nil || n != len(got) || string(got) != name {
			t.Fatalf("ReadAt(%s)=(%q, %d, %v)", name, got, n, err)
		}
	}
	select {
	case err := <-serverErr:
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("source stream remained open after racing materialization")
	}
}

func TestRemoteMarkerRetirementSerializesWithMetadataSync(t *testing.T) {
	sourceManager := openTestManager(t)
	source, err := sourceManager.Create("source")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	ino, err := source.CreateRegular(MainBranch, RootInode, "database-wal", 0600, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := source.WriteAt(MainBranch, ino, []byte("wal contents"), 0); err != nil {
		t.Fatal(err)
	}
	if err := source.Fork(MainBranch, "sealed"); err != nil {
		t.Fatal(err)
	}
	descriptor, err := source.DescribeRemoteSnapshot("sealed")
	if err != nil {
		t.Fatal(err)
	}
	targetManager := openTestManager(t)
	target, err := targetManager.ImportPrivateRemoteDescriptor(filepath.Join(targetManager.root, "target"), "target", descriptor, "imported")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	serverErr := attachTestRemote(t, source, target, "sealed", "imported", nil)

	meta, err := target.Inode("imported", ino)
	if err != nil {
		t.Fatal(err)
	}
	target.remoteMu.Lock()
	remote := target.remoteByObject[meta.Object]
	target.remoteMu.Unlock()
	if remote == nil {
		t.Fatal("imported object has no remote provider")
	}

	locked := make(chan struct{})
	release := make(chan struct{})
	lockDone := make(chan struct{})
	go func() {
		target.remoteMarkerSyncMu.Lock()
		close(locked)
		<-release
		target.remoteMarkerSyncMu.Unlock()
		close(lockDone)
	}()
	<-locked

	materialized := make(chan error, 1)
	go func() { materialized <- remote.ensure(meta.Object, meta.Size, 0, meta.Size) }()
	select {
	case err := <-materialized:
		t.Fatalf("materialization retired its dirty marker outside the repository mutation fence: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	<-lockDone
	select {
	case err := <-materialized:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("materialization did not resume after the metadata fence")
	}
	if err := target.SyncMetadata(); err != nil {
		t.Fatalf("SyncMetadata after marker retirement: %v", err)
	}
	finishTestRemote(t, target, serverErr)
}

func TestRemoteMarkerRetirementDoesNotWaitForRepositoryMutation(t *testing.T) {
	sourceManager := openTestManager(t)
	source, err := sourceManager.Create("source")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	ino, err := source.CreateRegular(MainBranch, RootInode, "artifact", 0600, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := source.WriteAt(MainBranch, ino, []byte("contents"), 0); err != nil {
		t.Fatal(err)
	}
	if err := source.Fork(MainBranch, "sealed"); err != nil {
		t.Fatal(err)
	}
	descriptor, err := source.DescribeRemoteSnapshot("sealed")
	if err != nil {
		t.Fatal(err)
	}
	targetManager := openTestManager(t)
	target, err := targetManager.ImportPrivateRemoteDescriptor(filepath.Join(targetManager.root, "target"), "target", descriptor, "imported")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	serverErr := attachTestRemote(t, source, target, "sealed", "imported", nil)

	// Runtime filesystem operations may retain file or mapping locks while a
	// different goroutine owns operationMu. Completing a remote read must not
	// join that lock cycle merely to retire its pending marker.
	target.operationMu.Lock()
	readDone := make(chan error, 1)
	go func() { readDone <- target.EnsureDataRange("imported", ino, 0, 1) }()
	select {
	case err := <-readDone:
		if err != nil {
			target.operationMu.Unlock()
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		target.operationMu.Unlock()
		t.Fatal("remote marker retirement waited for the repository mutation lock")
	}
	target.operationMu.Unlock()
	finishTestRemote(t, target, serverErr)
}

func TestRemoteFetchDoesNotHoldBranchViewLock(t *testing.T) {
	sourceManager := openTestManager(t)
	source, err := sourceManager.Create("source")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	ino, err := source.CreateRegular(MainBranch, RootInode, "artifact", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := source.WriteAt(MainBranch, ino, []byte("remote data"), 0); err != nil {
		t.Fatal(err)
	}
	if err := source.Fork(MainBranch, "sealed"); err != nil {
		t.Fatal(err)
	}
	descriptor, err := source.DescribeRemoteSnapshot("sealed")
	if err != nil {
		t.Fatal(err)
	}
	targetManager := openTestManager(t)
	target, err := targetManager.ImportPrivateRemoteDescriptor(filepath.Join(targetManager.root, "target"), "target", descriptor, "imported")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	serverErr := attachTestRemote(t, source, target, "sealed", "imported", nil)
	imported, err := target.Inode("imported", ino)
	if err != nil {
		t.Fatal(err)
	}

	// Hold the provider lock so EnsureDataRange reaches the bulk-transfer
	// boundary but cannot complete. Repository mutation must remain possible in
	// this state; runtime invalidation hooks may need exactly that ordering.
	target.remoteMu.Lock()
	remote := target.remoteByObject[imported.Object]
	if remote == nil {
		target.remoteMu.Unlock()
		t.Fatal("imported object has no remote provider")
	}
	remote.mu.Lock()
	target.remoteMu.Unlock()

	readDone := make(chan error, 1)
	go func() { readDone <- target.EnsureDataRange("imported", ino, 0, 1) }()
	// Give the reader several scheduling turns to reach remote.mu. The assertion
	// below is bounded and cleanup always releases the artificial barrier.
	for i := 0; i < 20; i++ {
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
	mutationDone := make(chan error, 1)
	go func() { mutationDone <- target.withLock(func() error { return nil }) }()
	select {
	case err := <-mutationDone:
		if err != nil {
			remote.mu.Unlock()
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		remote.mu.Unlock()
		<-readDone
		t.Fatal("remote fetch retained the branch view lock while waiting for its provider")
	}
	remote.mu.Unlock()
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
	finishTestRemote(t, target, serverErr)
}

func TestRemoteDescriptorReadsCatalogAndFilesFromSharedPacks(t *testing.T) {
	sourceManager := openTestManager(t)
	source, err := sourceManager.Create("source")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	ino, err := source.CreateRegular(MainBranch, RootInode, "artifact", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("shared graph range\n"), 140000)
	if n, _, err := source.WriteAt(MainBranch, ino, payload, 0); err != nil || n != len(payload) {
		t.Fatalf("WriteAt: %d, %v", n, err)
	}
	if err := source.Fork(MainBranch, "sealed"); err != nil {
		t.Fatal(err)
	}
	descriptor, err := source.DescribeRemoteSnapshot("sealed")
	if err != nil {
		t.Fatal(err)
	}
	shared, err := ttcsnapshot.OpenSharedPackStore(filepath.Join(t.TempDir(), "shared"))
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range source.graph.Inspect().Snapshots {
		if snapshot.State != ttcsnapshot.SnapshotWorking {
			if err := shared.Publish(context.Background(), source.graph, snapshot.ID, source.data); err != nil {
				t.Fatal(err)
			}
		}
	}
	targetManager := openTestManager(t)
	target, err := targetManager.ImportPrivateRemoteDescriptor(filepath.Join(targetManager.root, "target"), "target", descriptor, "imported")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	catalog := testSharedCatalog{store: shared}
	serverErr := attachTestRemote(t, source, target, "sealed", "imported", catalog)
	if got := readAll(t, target, "imported", ino); !bytes.Equal([]byte(got), payload) {
		t.Fatal("shared graph pack returned different contents")
	}
	finishTestRemote(t, target, serverErr)
}

// A lazily imported branch reads and becomes independent of a source the
// process reads itself, as it does of one served over a stream.
func TestLocalObjectSourceServesSharedPacks(t *testing.T) {
	sourceManager := openTestManager(t)
	source, err := sourceManager.Create("source-local")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	ino, err := source.CreateRegular(MainBranch, RootInode, "artifact", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("read in process\n"), 90000)
	if n, _, err := source.WriteAt(MainBranch, ino, payload, 0); err != nil || n != len(payload) {
		t.Fatalf("WriteAt: %d, %v", n, err)
	}
	if err := source.Fork(MainBranch, "sealed"); err != nil {
		t.Fatal(err)
	}
	descriptor, err := source.DescribeRemoteSnapshot("sealed")
	if err != nil {
		t.Fatal(err)
	}
	shared, err := ttcsnapshot.OpenSharedPackStore(filepath.Join(t.TempDir(), "shared"))
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range source.graph.Inspect().Snapshots {
		if snapshot.State != ttcsnapshot.SnapshotWorking {
			if err := shared.Publish(context.Background(), source.graph, snapshot.ID, source.data); err != nil {
				t.Fatal(err)
			}
		}
	}
	targetManager := openTestManager(t)
	target, err := targetManager.ImportPrivateRemoteDescriptorWithOrigins(filepath.Join(targetManager.root, "target-local"), "target-local", descriptor, "imported", 100, 101)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	catalog := testSharedCatalog{store: shared}
	key := RemoteSourceKey(sha256.Sum256([]byte("rootfs-upper")))
	if err := target.AttachLocalObjectSource(RemoteObjectSource{Key: key, Reader: catalog, Catalog: catalog}, "imported"); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, target, "imported", ino); !bytes.Equal([]byte(got), payload) {
		t.Fatal("the local source returned different contents")
	}
	if err := target.MaterializeRemoteObjects(); err != nil {
		t.Fatal(err)
	}
	if pending, err := target.BranchRemoteSourceObjects("imported"); err != nil || len(pending) != 0 {
		t.Fatalf("objects left pending: %v, %v", pending, err)
	}
	target.remoteMu.Lock()
	attached := len(target.remotes)
	target.remoteMu.Unlock()
	if attached != 0 {
		t.Fatalf("%d sources still attached after materialization", attached)
	}
	if got := readAll(t, target, "imported", ino); !bytes.Equal([]byte(got), payload) {
		t.Fatal("the materialized file differs")
	}
}

func TestPrepareBranchPublicationSealsLazyCatalog(t *testing.T) {
	sourceManager := openTestManager(t)
	source, err := sourceManager.Create("source")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	ino, err := source.CreateRegular(MainBranch, RootInode, "artifact", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := source.WriteAt(MainBranch, ino, []byte("published through lazy catalog"), 0); err != nil {
		t.Fatal(err)
	}
	if err := source.Fork(MainBranch, "sealed"); err != nil {
		t.Fatal(err)
	}
	descriptor, err := source.DescribeRemoteSnapshot("sealed")
	if err != nil {
		t.Fatal(err)
	}
	shared, err := ttcsnapshot.OpenSharedPackStore(filepath.Join(t.TempDir(), "shared"))
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range source.graph.Inspect().Snapshots {
		if snapshot.State != ttcsnapshot.SnapshotWorking {
			if err := shared.Publish(context.Background(), source.graph, snapshot.ID, source.data); err != nil {
				t.Fatal(err)
			}
		}
	}
	targetManager := openTestManager(t)
	target, err := targetManager.ImportPrivateRemoteDescriptorWithOrigins(filepath.Join(targetManager.root, "target"), "target", descriptor, "imported", 100, 101)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	catalog := testSharedCatalog{store: shared}
	serverErr := attachTestRemote(t, source, target, "sealed", "imported", catalog)
	if _, err := target.Lookup("imported", RootInode, "artifact"); err != nil {
		t.Fatal(err)
	}
	if err := target.Unlink("imported", RootInode, "artifact"); err != nil {
		t.Fatal(err)
	}
	if _, err := target.CreateRegular("imported", RootInode, "target-local", 0644, 0, 0); err != nil {
		t.Fatal(err)
	}
	changes, err := target.BranchDiff("imported")
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 || changes[0].Path != "/artifact" || changes[0].Kind != "delete" || changes[1].Path != "/target-local" || changes[1].Kind != "add" {
		t.Fatalf("changes before publication=%+v, want inherited deletion and local addition", changes)
	}
	imported, err := target.Branch("imported")
	if err != nil {
		t.Fatal(err)
	}
	if err := target.withLock(func() error {
		return target.graph.Seal(ttcsnapshot.SnapshotID(imported.WorkingSnapshot), ttcsnapshot.SnapshotSealedVolatile)
	}); err != nil {
		t.Fatal(err)
	}
	origins, err := target.PrepareBranchPublication("imported")
	if err != nil {
		t.Fatal(err)
	}
	for _, branchID := range []string{"imported"} {
		branch, err := target.Branch(branchID)
		if err != nil {
			t.Fatal(err)
		}
		if branch.RemoteCatalog != "" || branch.RemoteCatalogSnapshot != 0 || branch.RemoteLogicalBytes != 0 {
			t.Fatalf("branch %s retained remote catalog state: %+v", branchID, branch)
		}
	}
	if want := []ttcsnapshot.SnapshotID{101, 100}; !slices.Equal(origins, want) {
		t.Fatalf("publication origins=%v, want %v", origins, want)
	}
	for _, origin := range origins {
		record, ok := target.graph.Snapshot(origin)
		if !ok || record.State != ttcsnapshot.SnapshotSealedVolatile {
			t.Fatalf("origin %d=(%+v,%t), want sealed-volatile", origin, record, ok)
		}
	}
	if err := <-serverErr; err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	changes, err = target.BranchDiff("imported")
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 || changes[0].Path != "/artifact" || changes[0].Kind != "delete" || changes[1].Path != "/target-local" || changes[1].Kind != "add" {
		t.Fatalf("changes=%+v, want inherited deletion and local addition after provider detach", changes)
	}
}

func TestPrepareRetirementPublicationSealsWorkingRemoteBranch(t *testing.T) {
	sourceManager := openTestManager(t)
	source, err := sourceManager.Create("source")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	ino, err := source.CreateRegular(MainBranch, RootInode, "artifact", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("retirement materialization\n"), 10000)
	if written, _, err := source.WriteAt(MainBranch, ino, payload, 0); err != nil || written != len(payload) {
		t.Fatalf("WriteAt: %d, %v", written, err)
	}
	if err := source.Fork(MainBranch, "sealed"); err != nil {
		t.Fatal(err)
	}
	descriptor, err := source.DescribeRemoteSnapshot("sealed")
	if err != nil {
		t.Fatal(err)
	}

	targetManager := openTestManager(t)
	target, err := targetManager.ImportPrivateRemoteDescriptorWithOrigins(filepath.Join(targetManager.root, "target"), "target", descriptor, "imported", 100, 101)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	serverErr := attachTestRemote(t, source, target, "sealed", "imported", nil)
	if _, err := target.CreateRegular("imported", RootInode, "target-local", 0644, 0, 0); err != nil {
		t.Fatal(err)
	}

	origins, err := target.PrepareRetirementPublication()
	if err != nil {
		t.Fatal(err)
	}
	if want := []ttcsnapshot.SnapshotID{101, 100}; !slices.Equal(origins, want) {
		t.Fatalf("retirement origins=%v, want %v", origins, want)
	}
	for _, origin := range origins {
		record, ok := target.graph.Snapshot(origin)
		if !ok || record.State != ttcsnapshot.SnapshotSealedVolatile {
			t.Fatalf("origin %d=(%+v,%t), want sealed-volatile", origin, record, ok)
		}
	}
	branch, err := target.Branch("imported")
	if err != nil {
		t.Fatal(err)
	}
	if branch.RemoteCatalog != "" || branch.RemoteCatalogSnapshot != 0 || branch.RemoteLogicalBytes != 0 {
		t.Fatalf("retired branch retained remote catalog state: %+v", branch)
	}
	if got := readAll(t, target, "imported", ino); !bytes.Equal([]byte(got), payload) {
		t.Fatal("retirement materialization changed file contents")
	}
	finishTestRemote(t, target, serverErr)
}

func TestIndependentRemoteImportsOfSameRootHaveDistinctCatalogs(t *testing.T) {
	sourceManager := openTestManager(t)
	source, err := sourceManager.Create("source")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	ino, err := source.CreateRegular(MainBranch, RootInode, "artifact", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("independent imported catalog\n"), 10000)
	if n, _, err := source.WriteAt(MainBranch, ino, payload, 0); err != nil || n != len(payload) {
		t.Fatalf("WriteAt: %d, %v", n, err)
	}
	if err := source.Fork(MainBranch, "sealed"); err != nil {
		t.Fatal(err)
	}
	descriptor, err := source.DescribeRemoteSnapshot("sealed")
	if err != nil {
		t.Fatal(err)
	}

	targetManager := openTestManager(t)
	target, err := targetManager.ImportPrivateRemoteDescriptorWithOrigins(filepath.Join(targetManager.root, "target"), "target", descriptor, "first", 100, 101)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if err := target.ImportRemoteBranchDescriptorWithOrigins(descriptor, "second", 102, 103); err != nil {
		t.Fatal(err)
	}
	first, err := target.Branch("first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := target.Branch("second")
	if err != nil {
		t.Fatal(err)
	}
	if first.RemoteCatalog == second.RemoteCatalog {
		t.Fatalf("independent imports share catalog %q", first.RemoteCatalog)
	}
	if first.RemoteCatalogSnapshot != 101 || second.RemoteCatalogSnapshot != 103 {
		t.Fatalf("catalog origins first=%d second=%d, want 101 and 103", first.RemoteCatalogSnapshot, second.RemoteCatalogSnapshot)
	}
	for _, origin := range []ttcsnapshot.SnapshotID{101, 103} {
		snapshot, ok := target.graph.Snapshot(origin)
		if !ok || snapshot.State != ttcsnapshot.SnapshotWorking {
			t.Fatalf("catalog origin %d=(%+v,%t), want working", origin, snapshot, ok)
		}
	}

	firstServer := attachTestRemote(t, source, target, "sealed", "first", nil)
	secondServer := attachTestRemote(t, source, target, "sealed", "second", nil)
	if got := readAll(t, target, "first", ino); !bytes.Equal([]byte(got), payload) {
		t.Fatal("first import returned different contents")
	}
	if got := readAll(t, target, "second", ino); !bytes.Equal([]byte(got), payload) {
		t.Fatal("second import returned different contents")
	}
	finishTestRemote(t, target, firstServer)
	finishTestRemote(t, target, secondServer)
}

func TestDeferredMetadataObservationDoesNotRequireBulkTransport(t *testing.T) {
	sourceManager := openTestManager(t)
	source, err := sourceManager.Create("source-observation")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	ino, err := source.CreateRegular(MainBranch, RootInode, "artifact", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := source.WriteAt(MainBranch, ino, []byte("immutable payload"), 0); err != nil {
		t.Fatal(err)
	}
	if err := source.Fork(MainBranch, "sealed"); err != nil {
		t.Fatal(err)
	}
	descriptor, err := source.DescribeRemoteSnapshot("sealed")
	if err != nil {
		t.Fatal(err)
	}

	targetManager := openTestManager(t)
	target, err := targetManager.ImportPrivateRemoteDescriptorWithOrigins(filepath.Join(targetManager.root, "target-observation"), "target-observation", descriptor, "imported", 100, 101)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if err := target.AttachImmutableMetadataSource("imported", source.graph.ResolveObject); err != nil {
		t.Fatal(err)
	}
	usage, err := target.BranchSetUsage([]string{"imported"})
	if err != nil {
		t.Fatal(err)
	}
	if len(usage.Branches) != 1 || usage.Branches[0].Inodes != 2 || usage.Branches[0].TotalBytes != uint64(hostarch.PageSize) {
		t.Fatalf("usage=%+v, want two inodes and one page", usage)
	}
	branch, err := target.Branch("imported")
	if err != nil {
		t.Fatal(err)
	}
	root, err := ttcsnapshot.ParseObjectID(branch.Root)
	if err != nil {
		t.Fatal(err)
	}
	inodes, _, err := target.graph.FilesystemRecords(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, inode := range inodes {
		if inode.Type != "regular" {
			continue
		}
		if file, err := target.data.Open(inode.File, int64(inode.Size)); err == nil {
			file.Close()
			t.Fatalf("metadata observation materialized regular-file object %s", inode.File)
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	if _, err := target.CreateRegular("imported", RootInode, "local", 0644, 0, 0); err != nil {
		t.Fatal(err)
	}
	changes, err := target.BranchDiff("imported")
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Path != "/local" || changes[0].Kind != "add" {
		t.Fatalf("changes=%+v, want local addition", changes)
	}
}

// An open of a deferred file that looked for its bytes before a fetch of them
// ended registers the file only after the fetch: it must not mark the file
// pending again, a marker no source would retire, nor fail once a drain has
// detached the source.
func TestDeferredFileRegisteredAfterItsFetch(t *testing.T) {
	sourceManager := openTestManager(t)
	source, err := sourceManager.Create("source-fetched")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	ino, err := source.CreateRegular(MainBranch, RootInode, "artifact", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("fetched before its open registered it\n"), 1000)
	if n, _, err := source.WriteAt(MainBranch, ino, payload, 0); err != nil || n != len(payload) {
		t.Fatalf("WriteAt: %d, %v", n, err)
	}
	if err := source.Fork(MainBranch, "sealed"); err != nil {
		t.Fatal(err)
	}
	descriptor, err := source.DescribeRemoteSnapshot("sealed")
	if err != nil {
		t.Fatal(err)
	}
	targetManager := openTestManager(t)
	target, err := targetManager.ImportPrivateRemoteDescriptorWithOrigins(filepath.Join(targetManager.root, "target-fetched"), "target-fetched", descriptor, "imported", 100, 101)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	serverErr := attachTestRemote(t, source, target, "sealed", "imported", nil)
	inode, err := target.Inode("imported", ino)
	if err != nil {
		t.Fatal(err)
	}
	id, err := ttcsnapshot.ParseObjectID(inode.Object)
	if err != nil {
		t.Fatal(err)
	}
	catalog, sourceID, deferred := target.graph.DeferredSource(id)
	if !deferred {
		t.Fatalf("imported file object %s is not deferred", id)
	}
	// A reader fetches the file while the source stays attached; then the
	// open that looked before registers it.
	if got := readAll(t, target, "imported", ino); !bytes.Equal([]byte(got), payload) {
		t.Fatal("the fetched file differs")
	}
	if err := target.registerDeferredFileObject(id, inode.Size, catalog, sourceID); err != nil {
		t.Fatal(err)
	}
	if pending, err := target.remoteObjectPending(inode.Object); err != nil || pending {
		t.Fatalf("a fetched file is pending again: %t, %v", pending, err)
	}
	// The drain detaches the source: the file stays readable, and an open
	// that looked before registers it without the source.
	finishTestRemote(t, target, serverErr)
	if err := target.registerDeferredFileObject(id, inode.Size, catalog, sourceID); err != nil {
		t.Fatalf("registering a file fetched before its source detached: %v", err)
	}
	if got := readAll(t, target, "imported", ino); !bytes.Equal([]byte(got), payload) {
		t.Fatal("the fetched file differs after its source detached")
	}
}

// An empty deferred file has no bytes to fetch: preparing it installs it at
// once, neither registered with its source nor marked pending.
func TestEmptyDeferredFileNeedsNoSource(t *testing.T) {
	sourceManager := openTestManager(t)
	source, err := sourceManager.Create("source-empty")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	emptyIno, err := source.CreateRegular(MainBranch, RootInode, "empty", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	fullIno, err := source.CreateRegular(MainBranch, RootInode, "full", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("fetched from its source\n")
	if n, _, err := source.WriteAt(MainBranch, fullIno, payload, 0); err != nil || n != len(payload) {
		t.Fatalf("WriteAt: %d, %v", n, err)
	}
	if err := source.Fork(MainBranch, "sealed"); err != nil {
		t.Fatal(err)
	}
	descriptor, err := source.DescribeRemoteSnapshot("sealed")
	if err != nil {
		t.Fatal(err)
	}
	targetManager := openTestManager(t)
	target, err := targetManager.ImportPrivateRemoteDescriptorWithOrigins(filepath.Join(targetManager.root, "target-empty"), "target-empty", descriptor, "imported", 100, 101)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	serverErr := attachTestRemote(t, source, target, "sealed", "imported", nil)
	inode, err := target.Inode("imported", emptyIno)
	if err != nil {
		t.Fatal(err)
	}
	id, err := ttcsnapshot.ParseObjectID(inode.Object)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, deferred := target.graph.DeferredSource(id); !deferred {
		t.Fatalf("imported empty file object %s is not deferred", id)
	}
	if err := target.prepareDeferredInode(&inode); err != nil {
		t.Fatal(err)
	}
	target.remoteMu.Lock()
	registered := target.remoteByObject[inode.Object] != nil
	target.remoteMu.Unlock()
	if registered {
		t.Fatal("an empty file was registered with its source")
	}
	if pending, err := target.remoteObjectPending(inode.Object); err != nil || pending {
		t.Fatalf("an empty file is pending: %t, %v", pending, err)
	}
	if local, err := target.deferredObjectLocal(id, 0); err != nil || !local {
		t.Fatalf("an empty file is not local: %t, %v", local, err)
	}
	finishTestRemote(t, target, serverErr)
	if got := readAll(t, target, "imported", emptyIno); got != "" {
		t.Fatalf("empty file reads %q", got)
	}
	if got := readAll(t, target, "imported", fullIno); got != string(payload) {
		t.Fatalf("full file reads %q", got)
	}
	if pending, err := target.BranchRemoteSourceObjects("imported"); err != nil || len(pending) != 0 {
		t.Fatalf("objects left pending: %v, %v", pending, err)
	}
}

func TestRemoteDescriptorPreservesDiffBase(t *testing.T) {
	sourceManager := openTestManager(t)
	source, err := sourceManager.Create("source-diff")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	ino, err := source.CreateRegular(MainBranch, RootInode, "artifact", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := source.WriteAt(MainBranch, ino, []byte("base"), 0); err != nil {
		t.Fatal(err)
	}
	if err := source.Fork(MainBranch, "migrating"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := source.WriteAt("migrating", ino, []byte("changed"), 0); err != nil {
		t.Fatal(err)
	}
	migrating, err := source.Branch("migrating")
	if err != nil {
		t.Fatal(err)
	}
	if err := source.withLock(func() error {
		return source.graph.Seal(ttcsnapshot.SnapshotID(migrating.WorkingSnapshot), ttcsnapshot.SnapshotSealedVolatile)
	}); err != nil {
		t.Fatal(err)
	}
	descriptor, err := source.DescribeRemoteSnapshot("migrating")
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.BaseRoot.IsZero() || descriptor.BaseRoot == descriptor.Root {
		t.Fatalf("descriptor roots current=%s base=%s", descriptor.Root, descriptor.BaseRoot)
	}
	targetManager := openTestManager(t)
	target, err := targetManager.ImportPrivateRemoteDescriptorWithOrigins(filepath.Join(targetManager.root, "target-diff"), "target-diff", descriptor, "imported", 100, 101)
	if err != nil {
		t.Fatal(err)
	}
	serverErr := attachTestRemote(t, source, target, "migrating", "imported", nil)
	if err := target.ResolveBranchMetadata("imported"); err != nil {
		t.Fatal(err)
	}
	branch, err := target.Branch("imported")
	if err != nil {
		t.Fatal(err)
	}
	// Model the manager-side observer: it can reopen persistent metadata, but
	// does not own the Sentry's process-local lazy-catalog resolver.
	target.graph.DetachDeferredResolver(branch.RemoteCatalog)
	changes, err := target.BranchDiff("imported")
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Path != "/artifact" || changes[0].Kind != "modify" {
		t.Fatalf("changes=%+v, want migrated artifact modification", changes)
	}
	root, err := ttcsnapshot.ParseObjectID(branch.Root)
	if err != nil {
		t.Fatal(err)
	}
	inodes, _, err := target.graph.FilesystemRecords(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, inode := range inodes {
		if inode.Type != "regular" {
			continue
		}
		if file, err := target.data.Open(inode.File, int64(inode.Size)); err == nil {
			file.Close()
			t.Fatalf("metadata resolution materialized regular-file object %s", inode.File)
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-serverErr; err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
}

func TestMaterializeRemoteObjectsAfterImportedBranchRemoval(t *testing.T) {
	sourceManager := openTestManager(t)
	source, err := sourceManager.Create("source")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	ino, err := source.CreateRegular(MainBranch, RootInode, "artifact", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("inherited lazy data\n"), 100000)
	if n, _, err := source.WriteAt(MainBranch, ino, payload, 0); err != nil || n != len(payload) {
		t.Fatalf("WriteAt: %d, %v", n, err)
	}
	if err := source.Fork(MainBranch, "sealed"); err != nil {
		t.Fatal(err)
	}
	descriptor, err := source.DescribeRemoteSnapshot("sealed")
	if err != nil {
		t.Fatal(err)
	}
	targetManager := openTestManager(t)
	target, err := targetManager.ImportPrivateRemoteDescriptorWithOrigins(filepath.Join(targetManager.root, "target"), "target", descriptor, "imported", 100, 101)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	serverErr := attachTestRemote(t, source, target, "sealed", "imported", nil)
	tx, err := target.BeginForkWithOrigins("imported", "survivor", 102, 103)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := target.RemoveBranch("imported"); err != nil {
		t.Fatal(err)
	}
	finishTestRemote(t, target, serverErr)
	if got := readAll(t, target, "survivor", ino); !bytes.Equal([]byte(got), payload) {
		t.Fatal("surviving child lost data after its imported parent was removed")
	}
}
