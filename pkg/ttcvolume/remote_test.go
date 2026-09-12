// Copyright 2026 The gVisor Authors.
// Licensed under the Apache License, Version 2.0 (the "License");

package ttcvolume

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"path/filepath"
	"slices"
	"testing"
	"time"

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
	if _, err := target.CreateRegular("imported", RootInode, "target-local", 0644, 0, 0); err != nil {
		t.Fatal(err)
	}
	tx, err := target.BeginForkWithOrigins("imported", "published", 102, 103)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	origins, err := target.PrepareBranchPublication("published")
	if err != nil {
		t.Fatal(err)
	}
	for _, branchID := range []string{"published", "imported"} {
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
