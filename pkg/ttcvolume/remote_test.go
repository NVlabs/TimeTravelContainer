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
	"strings"
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
	var materialized []string
	unregister := target.RegisterMaterializationHook(func(object string) {
		materialized = append(materialized, object)
	})
	defer unregister()
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
	if len(materialized) != 1 {
		t.Fatalf("materialization hooks=%v, want exactly one replacement", materialized)
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

func TestRemoteSnapshotReexportsTargetLocalFile(t *testing.T) {
	sourceManager := openTestManager(t)
	source, err := sourceManager.Create("source")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	seed, err := source.CreateRegular(MainBranch, RootInode, "seed", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := source.WriteAt(MainBranch, seed, []byte("seed"), 0); err != nil {
		t.Fatal(err)
	}
	if err := source.Fork(MainBranch, "first-hop"); err != nil {
		t.Fatal(err)
	}
	firstSnapshot := filepath.Join(t.TempDir(), "first")
	_, firstObjects, err := source.ExportRemoteSnapshot("first-hop", firstSnapshot)
	if err != nil {
		t.Fatal(err)
	}

	targetManager := openTestManager(t)
	target, _, err := targetManager.ImportPrivateRemoteSnapshot(filepath.Join(targetManager.root, "target"), "target", firstSnapshot, "first-hop")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	var token RemoteToken
	copy(token[:], []byte("snapshot-token-1"))
	key := RemoteSourceKey(sha256.Sum256([]byte("rootfs-upper")))
	server1, client1 := net.Pipe()
	go ServeRemoteObjects(server1, token, []RemoteObjectSource{{Key: key, Repository: source, Objects: firstObjects}})
	if err := target.AttachRemoteObjectSource(client1, token, key, "first-hop"); err != nil {
		t.Fatal(err)
	}
	identity, err := target.CreateRegular("first-hop", RootInode, "identity", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n, _, err := target.WriteAt("first-hop", identity, []byte("REMOTE-1"), 0); err != nil || n != 8 {
		t.Fatalf("writing target-local file: n=%d err=%v", n, err)
	}
	if got := readAll(t, target, "first-hop", identity); got != "REMOTE-1" {
		t.Fatalf("target-local identity=%q", got)
	}
	if err := target.Fork("first-hop", "second-hop"); err != nil {
		t.Fatal(err)
	}
	secondSnapshot := filepath.Join(t.TempDir(), "second")
	_, secondObjects, err := target.ExportRemoteSnapshot("second-hop", secondSnapshot)
	if err != nil {
		t.Fatal(err)
	}

	thirdManager := openTestManager(t)
	third, _, err := thirdManager.ImportPrivateRemoteSnapshot(filepath.Join(thirdManager.root, "third"), "third", secondSnapshot, "second-hop")
	if err != nil {
		t.Fatal(err)
	}
	defer third.Close()
	server2, client2 := net.Pipe()
	go ServeRemoteObjects(server2, token, []RemoteObjectSource{{Key: key, Repository: target, Objects: secondObjects}})
	if err := third.AttachRemoteObjectSource(client2, token, key, "second-hop"); err != nil {
		t.Fatal(err)
	}
	gotInode, err := third.Lookup("second-hop", RootInode, "identity")
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, third, "second-hop", gotInode); got != "REMOTE-1" {
		t.Fatalf("second-hop identity=%q, want REMOTE-1", got)
	}
}

func TestRemoteSnapshotNeverReadsUnattachedPlaceholderAsZeros(t *testing.T) {
	sourceManager := openTestManager(t)
	source, err := sourceManager.Create("source")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	ino, err := source.CreateRegular(MainBranch, RootInode, "value", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := source.WriteAt(MainBranch, ino, []byte("not-zero"), 0); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(t.TempDir(), "snapshot")
	if _, _, err := source.ExportRemoteSnapshot(MainBranch, snapshot); err != nil {
		t.Fatal(err)
	}
	targetManager := openTestManager(t)
	target, _, err := targetManager.ImportPrivateRemoteSnapshot(filepath.Join(targetManager.root, "target"), "target", snapshot, "remote")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	targetIno, err := target.Lookup("remote", RootInode, "value")
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 8)
	if _, err := target.ReadAt("remote", targetIno, data, 0); err == nil || !strings.Contains(err.Error(), "no attached authoritative source") {
		t.Fatalf("ReadAt on unattached placeholder got %q, err=%v", data, err)
	}
}
