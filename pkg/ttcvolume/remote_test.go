// Copyright 2026 The gVisor Authors.
// Licensed under the Apache License, Version 2.0 (the "License");

package ttcvolume

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	snapshot, objects, err := source.ExportRemoteSnapshot("remote-generation", snapshotDir)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.LogicalBytes != uint64(len(payload)) {
		t.Fatalf("remote logical bytes=%d, want %d", snapshot.LogicalBytes, len(payload))
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

func TestPackedSnapshotFetchesPortableObjectIdentity(t *testing.T) {
	sourceManager := openTestManager(t)
	source, err := sourceManager.Create("source-packed")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	ino, err := source.CreateRegular(MainBranch, RootInode, "artifact", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("packed-object-identity\n"), 10000)
	if n, _, err := source.WriteAt(MainBranch, ino, payload, 0); err != nil || n != len(payload) {
		t.Fatalf("WriteAt: %d, %v", n, err)
	}
	snapshotDir := filepath.Join(t.TempDir(), "portable")
	if _, err := source.ExportSnapshot(MainBranch, snapshotDir); err != nil {
		t.Fatal(err)
	}
	sourceObjects, err := source.BranchObjects(MainBranch)
	if err != nil {
		t.Fatal(err)
	}
	// Shared-pack metadata materialization deliberately omits object bytes.
	if err := os.RemoveAll(filepath.Join(snapshotDir, "objects")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(snapshotDir, "objects"), 0700); err != nil {
		t.Fatal(err)
	}

	targetManager := openTestManager(t)
	targetPath := filepath.Join(targetManager.root, "packed-target")
	target, _, err := targetManager.ImportPrivatePackedSnapshot(targetPath, "packed-target", snapshotDir, "restored")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	allowed, err := target.BranchRemoteSourceObjects("restored")
	if err != nil {
		t.Fatal(err)
	}
	if len(allowed) != len(sourceObjects) {
		t.Fatalf("source allowlist=%v, want %v", allowed, sourceObjects)
	}
	for object, size := range sourceObjects {
		if allowed[object] != size {
			t.Fatalf("source allowlist[%s]=%d, want %d", object, allowed[object], size)
		}
	}
	server, client := net.Pipe()
	var token RemoteToken
	copy(token[:], []byte("packed-token-001"))
	key := RemoteSourceKey(sha256.Sum256([]byte("rootfs-upper")))
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- ServeRemoteObjects(server, token, []RemoteObjectSource{{Key: key, Repository: source, Objects: sourceObjects}})
	}()
	if err := target.AttachRemoteObjectSource(client, token, key, "restored"); err != nil {
		t.Fatal(err)
	}
	got := readAll(t, target, "restored", ino)
	if !bytes.Equal([]byte(got), payload) {
		t.Fatal("lazy portable object contents differ")
	}
	if err := target.MaterializeRemoteObjects(); err != nil {
		t.Fatal(err)
	}
	if err := <-serverErr; err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
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
	var drains sync.WaitGroup
	drainErrors := make(chan error, 2)
	for i := 0; i < 2; i++ {
		drains.Add(1)
		go func() {
			defer drains.Done()
			drainErrors <- target.MaterializeRemoteObjects()
		}()
	}
	drains.Wait()
	close(drainErrors)
	for err := range drainErrors {
		if err != nil {
			t.Fatalf("concurrent MaterializeRemoteObjects: %v", err)
		}
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

func TestRemoteSnapshotReexportFromIndependentManagerHandle(t *testing.T) {
	sourceManager := openTestManager(t)
	source, err := sourceManager.Create("source")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	ino, err := source.CreateRegular(MainBranch, RootInode, "payload", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := source.WriteAt(MainBranch, ino, []byte("remote-data"), 0); err != nil {
		t.Fatal(err)
	}
	if err := source.Fork(MainBranch, "first-hop"); err != nil {
		t.Fatal(err)
	}
	firstSnapshot := filepath.Join(t.TempDir(), "first")
	firstInfo, _, err := source.ExportRemoteSnapshot("first-hop", firstSnapshot)
	if err != nil {
		t.Fatal(err)
	}

	targetManager := openTestManager(t)
	targetPath := filepath.Join(targetManager.root, "target")
	target, _, err := targetManager.ImportPrivateRemoteSnapshot(targetPath, "target", firstSnapshot, "first-hop")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()

	// A daemon-side repository handle does not own the Sentry's transient
	// provider connection. Re-export must remain O(1) and preserve the
	// deferred root instead of trying to resolve it through this handle.
	managerHandle, err := OpenRepository(targetPath, CopyModeFull)
	if err != nil {
		t.Fatal(err)
	}
	defer managerHandle.Close()
	secondSnapshot := filepath.Join(t.TempDir(), "second")
	secondInfo, _, err := managerHandle.ExportRemoteSnapshot("first-hop", secondSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if secondInfo.Root == "" || secondInfo.LogicalBytes != firstInfo.LogicalBytes {
		t.Fatalf("second-hop snapshot=%+v, want logical bytes %d", secondInfo, firstInfo.LogicalBytes)
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
	if err := source.Fork(MainBranch, "remote-generation"); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(t.TempDir(), "snapshot")
	if _, _, err := source.ExportRemoteSnapshot("remote-generation", snapshot); err != nil {
		t.Fatal(err)
	}
	targetManager := openTestManager(t)
	target, _, err := targetManager.ImportPrivateRemoteSnapshot(filepath.Join(targetManager.root, "target"), "target", snapshot, "remote")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if _, err := target.Lookup("remote", RootInode, "value"); err == nil || !strings.Contains(err.Error(), "no attached authoritative source") {
		t.Fatalf("Lookup on unattached deferred metadata err=%v", err)
	}
}

func TestRemoteSnapshotImportAndLookupStayGraphBounded(t *testing.T) {
	sourceManager := openTestManager(t)
	source, err := sourceManager.Create("source")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	for i := 0; i < 512; i++ {
		if _, err := source.CreateRegular(MainBranch, RootInode, fmt.Sprintf("entry-%04d", i), 0644, 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := source.Fork(MainBranch, "remote-generation"); err != nil {
		t.Fatal(err)
	}
	sourceObjects := source.graph.Stats().Objects
	if sourceObjects < 100 {
		t.Fatalf("source graph objects=%d, want a nontrivial namespace", sourceObjects)
	}
	snapshot := filepath.Join(t.TempDir(), "snapshot")
	if _, _, err := source.ExportRemoteSnapshot("remote-generation", snapshot); err != nil {
		t.Fatal(err)
	}
	graphDescriptor, err := os.ReadFile(filepath.Join(snapshot, "graph.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(graphDescriptor) > 512 {
		t.Fatalf("remote root descriptor=%d bytes, want O(1) metadata", len(graphDescriptor))
	}

	targetManager := openTestManager(t)
	target, _, err := targetManager.ImportPrivateRemoteSnapshot(filepath.Join(targetManager.root, "target"), "target", snapshot, "remote")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	initialObjects := target.graph.Stats().Objects
	if initialObjects >= 16 || initialObjects >= sourceObjects {
		t.Fatalf("initial target graph objects=%d, source=%d; import traversed the namespace", initialObjects, sourceObjects)
	}

	var token RemoteToken
	copy(token[:], []byte("snapshot-token-1"))
	key := RemoteSourceKey(sha256.Sum256([]byte("rootfs-upper")))
	server, client := net.Pipe()
	go ServeRemoteObjects(server, token, []RemoteObjectSource{{Key: key, Repository: source}})
	if err := target.AttachRemoteObjectSource(client, token, key, "remote"); err != nil {
		t.Fatal(err)
	}
	if _, err := target.Lookup("remote", RootInode, "entry-0256"); err != nil {
		t.Fatal(err)
	}
	afterLookup := target.graph.Stats().Objects
	// Expanding each node installs its bounded direct-child stubs, so the local
	// object count includes at most the traversed paths plus one fanout per
	// level. It must not scale with the complete namespace.
	if loaded := afterLookup - initialObjects; loaded == 0 || loaded > 128 {
		t.Fatalf("single lookup installed %d graph objects, want bounded B-tree paths and child stubs", loaded)
	}
}

func TestRemoteSnapshotReexportReturningToExistingRepositories(t *testing.T) {
	managerA := openTestManager(t)
	repositoryA, err := managerA.Create("family")
	if err != nil {
		t.Fatal(err)
	}
	defer repositoryA.Close()
	inode, err := repositoryA.CreateRegular(MainBranch, RootInode, "payload", 0644, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := repositoryA.WriteAt(MainBranch, inode, []byte("original"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := repositoryA.CreateRegular(MainBranch, RootInode, "empty", 0644, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := repositoryA.Fork(MainBranch, "ab"); err != nil {
		t.Fatal(err)
	}

	var token RemoteToken
	copy(token[:], []byte("returning-hops"))
	key := RemoteSourceKey(sha256.Sum256([]byte("rootfs-upper")))
	exportAB := filepath.Join(t.TempDir(), "ab")
	_, objectsAB, err := repositoryA.ExportRemoteSnapshot("ab", exportAB)
	if err != nil {
		t.Fatal(err)
	}
	managerB := openTestManager(t)
	repositoryB, _, err := managerB.ImportPrivateRemoteSnapshot(filepath.Join(managerB.root, "family"), "family", exportAB, "ab")
	if err != nil {
		t.Fatal(err)
	}
	defer repositoryB.Close()
	serverAB, clientAB := net.Pipe()
	go ServeRemoteObjects(serverAB, token, []RemoteObjectSource{{Key: key, Repository: repositoryA, Objects: objectsAB}})
	if err := repositoryB.AttachRemoteObjectSource(clientAB, token, key, "ab"); err != nil {
		t.Fatal(err)
	}

	if err := repositoryB.Fork("ab", "ba"); err != nil {
		t.Fatal(err)
	}
	exportBA := filepath.Join(t.TempDir(), "ba")
	_, objectsBA, err := repositoryB.ExportRemoteSnapshot("ba", exportBA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repositoryA.ImportRemoteBranchSnapshot(exportBA, "ba"); err != nil {
		t.Fatal(err)
	}
	serverBA, clientBA := net.Pipe()
	go ServeRemoteObjects(serverBA, token, []RemoteObjectSource{{Key: key, Repository: repositoryB, Objects: objectsBA}})
	if err := repositoryA.AttachRemoteObjectSource(clientBA, token, key, "ba"); err != nil {
		t.Fatal(err)
	}

	if err := repositoryA.Fork("ba", "aba"); err != nil {
		t.Fatal(err)
	}
	exportABA := filepath.Join(t.TempDir(), "aba")
	_, objectsABA, err := repositoryA.ExportRemoteSnapshot("aba", exportABA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repositoryB.ImportRemoteBranchSnapshot(exportABA, "aba"); err != nil {
		t.Fatal(err)
	}
	serverABA, clientABA := net.Pipe()
	go ServeRemoteObjects(serverABA, token, []RemoteObjectSource{{Key: key, Repository: repositoryA, Objects: objectsABA}})
	if err := repositoryB.AttachRemoteObjectSource(clientABA, token, key, "aba"); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- repositoryB.MaterializeRemoteObjects() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("materializing A-to-B-to-A-to-B repository dependency deadlocked")
	}
}
