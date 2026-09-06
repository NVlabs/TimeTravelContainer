// Copyright 2026 The gVisor Authors.
// Licensed under the Apache License, Version 2.0 (the "License");

package ttcvolume

import (
	"bytes"
	"crypto/sha256"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRemoteSnapshotFetchesExtentsAndPrivatizesWrites(t *testing.T) {
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
