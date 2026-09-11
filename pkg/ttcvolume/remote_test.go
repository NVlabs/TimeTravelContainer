// Copyright 2026 The gVisor Authors.
// Licensed under the Apache License, Version 2.0 (the "License");

package ttcvolume

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRemoteSnapshotFetchesWholeFileAndPrivatizesWrites(t *testing.T) {
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
	if err := source.Fork(MainBranch, "remote-generation"); err != nil {
		t.Fatal(err)
	}
	snapshotDir := filepath.Join(t.TempDir(), "snapshot")
	_, objects, err := source.ExportRemoteSnapshot("remote-generation", snapshotDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(snapshotDir, "graph.json")); err != nil {
		t.Fatalf("graph-native remote metadata is missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(snapshotDir, "metadata.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("flattened remote metadata exists: %v", err)
	}
	targetManager := openTestManager(t)
	targetPath := filepath.Join(targetManager.root, "target-repository")
	target, _, err := targetManager.ImportPrivateRemoteSnapshot(targetPath, "target", snapshotDir, "target-generation")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	server, client := net.Pipe()
	var token RemoteToken
	copy(token[:], []byte("snapshot-token-1"))
	key := RemoteSourceKey(sha256.Sum256([]byte("rootfs-upper")))
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- ServeRemoteObjects(server, token, []RemoteObjectSource{{Key: key, Repository: source, Objects: objects}})
	}()
	if err := target.AttachRemoteObjectSource(client, token, key, "target-generation"); err != nil {
		t.Fatal(err)
	}
	delta := filepath.Join(t.TempDir(), "delta")
	inherited := make(map[string]struct{}, len(objects))
	for object := range objects {
		inherited[object] = struct{}{}
	}
	if _, err := target.ExportSnapshotDelta("target-generation", delta, inherited); err != nil {
		t.Fatalf("metadata-only delta: %v", err)
	}
	if entries, err := os.ReadDir(filepath.Join(delta, "objects")); err != nil || len(entries) != 0 {
		t.Fatalf("inherited objects were exported: entries=%v err=%v", entries, err)
	}
	got := make([]byte, 4096)
	offset := int64(remoteExtentBytes + 123)
	if n, err := target.ReadAt("target-generation", ino, got, offset); err != nil || n != len(got) {
		t.Fatalf("remote ReadAt: %d, %v", n, err)
	}
	if !bytes.Equal(got, payload[offset:offset+int64(len(got))]) {
		t.Fatal("remote range contents differ")
	}
	// One small read installs the entire immutable file. Local cache state is
	// absent/complete; it is never a sparse partially-populated object.
	remoteInode, err := target.Inode("target-generation", ino)
	if err != nil {
		t.Fatal(err)
	}
	if pending, err := target.remoteObjectPending(remoteInode.Object); err != nil || pending {
		t.Fatalf("remote object pending after first read: pending=%t err=%v", pending, err)
	}
	localBytes, err := os.ReadFile(filepath.Join(target.path, target.objectPath(remoteInode.Object)))
	if err != nil || !bytes.Equal(localBytes, payload) {
		t.Fatalf("complete local materialization: bytes=%d err=%v", len(localBytes), err)
	}
	if n, cloned, err := target.WriteAt("target-generation", ino, []byte("TARGET"), 17); err != nil || n != 6 || !cloned {
		t.Fatalf("remote target WriteAt: n=%d cloned=%t err=%v", n, cloned, err)
	}
	if got := readAll(t, source, "remote-generation", ino); !bytes.Equal([]byte(got), payload) {
		t.Fatal("target write changed source generation")
	}
	if err := target.MaterializeRemoteObjects(); err != nil {
		t.Fatalf("MaterializeRemoteObjects: %v", err)
	}
	select {
	case <-serverErr:
	case <-time.After(time.Second):
		t.Fatal("source stream remained open after target materialization")
	}
}

func TestRemoteSnapshotSharedObjectUsesNewestLiveSource(t *testing.T) {
	sourceManager := openTestManager(t)
	source, err := sourceManager.Create("source")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	ino, err := source.CreateRegular(MainBranch, RootInode, "data", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("immutable-data-"), 100000)
	if n, _, err := source.WriteAt(MainBranch, ino, payload, 0); err != nil || n != len(payload) {
		t.Fatalf("WriteAt: %d, %v", n, err)
	}
	if err := source.Fork(MainBranch, "remote-generation"); err != nil {
		t.Fatal(err)
	}
	snapshotDir := filepath.Join(t.TempDir(), "snapshot")
	_, objects, err := source.ExportRemoteSnapshot("remote-generation", snapshotDir)
	if err != nil {
		t.Fatal(err)
	}
	targetManager := openTestManager(t)
	targetPath := filepath.Join(targetManager.root, "target-repository")
	target, _, err := targetManager.ImportPrivateRemoteSnapshot(targetPath, "target", snapshotDir, "first")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()

	var token RemoteToken
	copy(token[:], []byte("snapshot-token-1"))
	key := RemoteSourceKey(sha256.Sum256([]byte("rootfs-upper")))
	server1, client1 := net.Pipe()
	go ServeRemoteObjects(server1, token, []RemoteObjectSource{{Key: key, Repository: source, Objects: objects}})
	if err := target.AttachRemoteObjectSource(client1, token, key, "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := target.ImportRemoteBranchSnapshot(snapshotDir, "second"); err != nil {
		t.Fatal(err)
	}
	server2, client2 := net.Pipe()
	server2Done := make(chan error, 1)
	go func() {
		server2Done <- ServeRemoteObjects(server2, token, []RemoteObjectSource{{Key: key, Repository: source, Objects: objects}})
	}()
	if err := target.AttachRemoteObjectSource(client2, token, key, "second"); err != nil {
		t.Fatal(err)
	}
	// Simulate loss of the older branch's source. Since both sources serve the
	// same content-addressed object, repository-wide materialization must use
	// the newer live connection and make both branches self-contained.
	_ = server1.Close()
	if err := target.MaterializeRemoteObjects(); err != nil {
		t.Fatalf("MaterializeRemoteObjects: %v", err)
	}
	select {
	case <-server2Done:
	case <-time.After(time.Second):
		t.Fatal("new source stream remained open after materialization")
	}
	for _, branch := range []string{"first", "second"} {
		got := make([]byte, len(payload))
		if n, err := target.ReadAt(branch, ino, got, 0); err != nil || n != len(got) {
			t.Fatalf("ReadAt(%s): %d, %v", branch, n, err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("%s contents differ", branch)
		}
	}
}
