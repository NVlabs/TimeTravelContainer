// Copyright 2026 The gVisor Authors.
// Licensed under the Apache License, Version 2.0 (the "License");

package cmd

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	"gvisor.dev/gvisor/pkg/ttcvolume"
	"gvisor.dev/gvisor/runsc/boot"
	"gvisor.dev/gvisor/runsc/specutils"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
	"gvisor.dev/gvisor/runsc/ttc/cluster"
	ttcstorage "gvisor.dev/gvisor/runsc/ttc/storage"
)

type sharedSnapshotObjectReader struct {
	store          *ttcsnapshot.SharedPackStore
	mu             sync.Mutex
	allowed        map[ttcsnapshot.ObjectID]struct{}
	allowedOrigins map[ttcsnapshot.SnapshotID]struct{}
}

func newSharedSnapshotObjectReader(store *ttcsnapshot.SharedPackStore, root string) (*sharedSnapshotObjectReader, error) {
	id, err := ttcsnapshot.ParseObjectID(root)
	if err != nil {
		return nil, err
	}
	return &sharedSnapshotObjectReader{store: store, allowed: map[ttcsnapshot.ObjectID]struct{}{id: {}}}, nil
}

func (r *sharedSnapshotObjectReader) authorize(id ttcsnapshot.ObjectID) bool {
	r.mu.Lock()
	_, ok := r.allowed[id]
	if !ok {
		_, ok = r.allowedOrigins[id.Snapshot]
	}
	r.mu.Unlock()
	return ok
}

func (r *sharedSnapshotObjectReader) ReadObjectRange(object string, destination []byte, offset int64) (int, error) {
	id, err := ttcsnapshot.ParseObjectID(object)
	if err != nil {
		return 0, err
	}
	if !r.authorize(id) {
		return 0, os.ErrPermission
	}
	return r.store.ReadObjectRange(context.Background(), id, destination, offset)
}

func (r *sharedSnapshotObjectReader) RemoteObject(id ttcsnapshot.ObjectID) (ttcsnapshot.Object, error) {
	if !r.authorize(id) {
		return ttcsnapshot.Object{}, os.ErrPermission
	}
	object, err := r.store.Object(id)
	if err != nil {
		return ttcsnapshot.Object{}, err
	}
	r.mu.Lock()
	for _, edge := range object.Children {
		r.allowed[edge.Child] = struct{}{}
	}
	if object.Deferred && !object.SourceObject.IsZero() {
		r.allowed[object.SourceObject] = struct{}{}
	}
	r.mu.Unlock()
	return object, nil
}

type coldFilesystemRelays struct {
	token   ttcvolume.RemoteToken
	clients []*os.File
	done    []<-chan error
}

const sharedMemoryRelayCatalogVersion = 1

type sharedMemoryRelayObject struct {
	ID     string `json:"id"`
	Length uint64 `json:"length"`
}

type sharedMemoryRelayCatalog struct {
	Version int                       `json:"version"`
	Objects []sharedMemoryRelayObject `json:"objects"`
}

// RunSharedMemoryRelay serves immutable graph-memory chunks from shared packs
// over inherited fd 3. The bounded allowlist is read from inherited fd 4.
// Like the filesystem relay, this helper is detached from the control daemon:
// a rehydrated Sentry may continue faulting cold pages across ttcd restarts.
func RunSharedMemoryRelay(args []string, stderr io.Writer) int {
	if len(args) != 2 {
		fmt.Fprintln(stderr, "ttcd shared memory relay: invalid arguments")
		return 2
	}
	store, err := ttcsnapshot.OpenSharedPackStore(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "ttcd shared memory relay: opening packs: %v\n", err)
		return 1
	}
	var token ttcsnapshot.ObjectRelayToken
	if decoded, decodeErr := hex.DecodeString(args[1]); decodeErr != nil || len(decoded) != len(token) {
		fmt.Fprintln(stderr, "ttcd shared memory relay: invalid token")
		return 2
	} else {
		copy(token[:], decoded)
	}
	catalogFile := os.NewFile(4, "ttcd-shared-memory-catalog")
	if catalogFile == nil {
		fmt.Fprintln(stderr, "ttcd shared memory relay: inherited catalog is unavailable")
		return 2
	}
	defer catalogFile.Close()
	var catalog sharedMemoryRelayCatalog
	decoder := json.NewDecoder(io.LimitReader(catalogFile, 64<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&catalog); err != nil || decoder.Decode(&struct{}{}) != io.EOF || catalog.Version != sharedMemoryRelayCatalogVersion || len(catalog.Objects) == 0 {
		fmt.Fprintln(stderr, "ttcd shared memory relay: invalid catalog")
		return 2
	}
	allowed := make(map[ttcsnapshot.ObjectID]uint64, len(catalog.Objects))
	for _, encoded := range catalog.Objects {
		id, err := ttcsnapshot.ParseObjectID(encoded.ID)
		if err != nil || encoded.Length == 0 || encoded.Length > ttcsnapshot.MemoryChunkBytes {
			fmt.Fprintln(stderr, "ttcd shared memory relay: invalid catalog object")
			return 2
		}
		if _, duplicate := allowed[id]; duplicate {
			fmt.Fprintln(stderr, "ttcd shared memory relay: duplicate catalog object")
			return 2
		}
		object, err := store.Object(id)
		if err != nil || object.BulkLength != encoded.Length {
			fmt.Fprintln(stderr, "ttcd shared memory relay: catalog object is unavailable")
			return 1
		}
		allowed[id] = encoded.Length
	}
	connection := os.NewFile(3, "ttcd-shared-memory-relay")
	if connection == nil {
		fmt.Fprintln(stderr, "ttcd shared memory relay: inherited connection is unavailable")
		return 2
	}
	err = ttcsnapshot.ServeObjectRelay(connection, token, store, allowed)
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) {
		return 0
	}
	fmt.Fprintf(stderr, "ttcd shared memory relay: %v\n", err)
	return 1
}

func startSharedMemoryRelay(sharedRoot string, objects map[ttcsnapshot.ObjectID]ttcsnapshot.Object, token ttcsnapshot.ObjectRelayToken) (*os.File, <-chan error, error) {
	if len(objects) == 0 {
		return nil, nil, errors.New("shared memory relay has no immutable objects")
	}
	catalog := sharedMemoryRelayCatalog{Version: sharedMemoryRelayCatalogVersion, Objects: make([]sharedMemoryRelayObject, 0, len(objects))}
	for id, object := range objects {
		if id.IsZero() || object.ID != id || object.BulkLength == 0 || object.BulkLength > ttcsnapshot.MemoryChunkBytes {
			return nil, nil, fmt.Errorf("invalid shared memory relay object %s", id)
		}
		catalog.Objects = append(catalog.Objects, sharedMemoryRelayObject{ID: id.String(), Length: object.BulkLength})
	}
	sort.Slice(catalog.Objects, func(i, j int) bool { return catalog.Objects[i].ID < catalog.Objects[j].ID })
	encoded, err := json.Marshal(&catalog)
	if err != nil {
		return nil, nil, err
	}
	catalogFile, err := newConfigMemFD("ttc-shared-memory-catalog", encoded)
	if err != nil {
		return nil, nil, err
	}
	defer catalogFile.Close()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	server := os.NewFile(uintptr(fds[0]), "ttc-shared-memory-server")
	client := os.NewFile(uintptr(fds[1]), "ttc-shared-memory-client")
	executable, err := os.Executable()
	if err != nil {
		server.Close()
		client.Close()
		return nil, nil, err
	}
	command := exec.Command(executable, "_shared-memory-relay", filepath.Join(sharedRoot, "snapshot-packs"), hex.EncodeToString(token[:]))
	command.ExtraFiles = []*os.File{server, catalogFile}
	command.Stdin, command.Stdout, command.Stderr = nil, nil, os.Stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		server.Close()
		client.Close()
		return nil, nil, err
	}
	server.Close()
	done := make(chan error, 1)
	go func() {
		done <- command.Wait()
		close(done)
	}()
	return client, done, nil
}

// RunSharedFilesystemRelay serves one immutable filesystem tree from shared
// packs over inherited fd 3. It runs in a tiny detached ttcd helper so a live
// Sentry's lazy reads do not depend on the control daemon's process lifetime.
func RunSharedFilesystemRelay(args []string, stderr io.Writer) int {
	if len(args) != 5 {
		fmt.Fprintln(stderr, "ttcd shared filesystem relay: invalid arguments")
		return 2
	}
	store, err := ttcsnapshot.OpenSharedPackStore(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "ttcd shared filesystem relay: opening packs: %v\n", err)
		return 1
	}
	reader, err := newSharedSnapshotObjectReader(store, args[1])
	if err != nil {
		fmt.Fprintf(stderr, "ttcd shared filesystem relay: opening root: %v\n", err)
		return 1
	}
	reader.allowedOrigins = make(map[ttcsnapshot.SnapshotID]struct{})
	for _, encoded := range strings.Split(args[4], ",") {
		origin, parseErr := strconv.ParseUint(encoded, 16, 64)
		if parseErr != nil || origin == 0 {
			fmt.Fprintln(stderr, "ttcd shared filesystem relay: invalid origin scope")
			return 2
		}
		reader.allowedOrigins[ttcsnapshot.SnapshotID(origin)] = struct{}{}
	}
	var token ttcvolume.RemoteToken
	var source ttcvolume.RemoteSourceKey
	if decoded, decodeErr := hex.DecodeString(args[2]); decodeErr != nil || len(decoded) != len(token) {
		fmt.Fprintln(stderr, "ttcd shared filesystem relay: invalid token")
		return 2
	} else {
		copy(token[:], decoded)
	}
	if decoded, decodeErr := hex.DecodeString(args[3]); decodeErr != nil || len(decoded) != len(source) {
		fmt.Fprintln(stderr, "ttcd shared filesystem relay: invalid source key")
		return 2
	} else {
		copy(source[:], decoded)
	}
	connection := os.NewFile(3, "ttcd-shared-filesystem-relay")
	if connection == nil {
		fmt.Fprintln(stderr, "ttcd shared filesystem relay: inherited connection is unavailable")
		return 2
	}
	err = ttcvolume.ServeRemoteObjects(connection, token, []ttcvolume.RemoteObjectSource{{
		Key: source, Reader: reader, Catalog: reader,
	}})
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) {
		return 0
	}
	fmt.Fprintf(stderr, "ttcd shared filesystem relay: %v\n", err)
	return 1
}

func startSharedFilesystemRelay(sharedRoot, root, sourceName string, origins []uint64, token ttcvolume.RemoteToken) (*os.File, <-chan error, error) {
	if len(origins) == 0 {
		return nil, nil, errors.New("shared filesystem relay has no immutable origin scope")
	}
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	server := os.NewFile(uintptr(fds[0]), "ttc-cold-graph-server")
	client := os.NewFile(uintptr(fds[1]), "ttc-cold-graph-client")
	key := ttcvolume.RemoteSourceKey(sha256.Sum256([]byte(sourceName)))
	executable, err := os.Executable()
	if err != nil {
		server.Close()
		client.Close()
		return nil, nil, err
	}
	encodedOrigins := make([]string, len(origins))
	for index, origin := range origins {
		if origin == 0 {
			server.Close()
			client.Close()
			return nil, nil, errors.New("shared filesystem relay contains zero origin")
		}
		encodedOrigins[index] = fmt.Sprintf("%016x", origin)
	}
	command := exec.Command(executable, "_shared-fs-relay", filepath.Join(sharedRoot, "snapshot-packs"), root, hex.EncodeToString(token[:]), hex.EncodeToString(key[:]), strings.Join(encodedOrigins, ","))
	command.ExtraFiles = []*os.File{server}
	command.Stdin, command.Stdout, command.Stderr = nil, nil, os.Stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		server.Close()
		client.Close()
		return nil, nil, err
	}
	server.Close()
	done := make(chan error, 1)
	go func() {
		done <- command.Wait()
		close(done)
	}()
	return client, done, nil
}

func (b *runtimeBackend) reportColdRelayError(kind string, err error) {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) {
		return
	}
	if b.stderr != nil {
		fmt.Fprintf(b.stderr, "ttc: warning: %s cold-storage relay failed: %v\n", kind, err)
	}
}

func (r *coldFilesystemRelays) close() {
	if r == nil {
		return
	}
	for _, file := range r.clients {
		_ = file.Close()
	}
}

type coldFilesystemSnapshot struct {
	Root    cluster.FilesystemRootDescriptor `json:"root"`
	Origins []uint64                         `json:"origins"`
}

type coldBranchManifest struct {
	Version       int       `json:"version"`
	Container     string    `json:"container"`
	Branch        string    `json:"branch"`
	PublicationID string    `json:"publication_id"`
	Kind          string    `json:"kind"`
	CreatedAt     time.Time `json:"created_at"`
	// MemorySnapshot names the graph-native immutable capture origin. Its
	// ControlRoot is a filesystem-tree object in that same origin containing
	// serialized process/kernel state; pages and auxiliary state therefore use
	// one authority, resolver, publication, and GC protocol.
	MemorySnapshot uint64                   `json:"memory_snapshot,omitempty"`
	ControlRoot    string                   `json:"control_root,omitempty"`
	Filesystems    []coldFilesystemSnapshot `json:"filesystems,omitempty"`
}

type branchPublisher struct {
	sharedRoot   string
	localRoot    string
	nodeID       string
	incarnation  string
	placements   cluster.PlacementCatalog
	transactions cluster.TransactionCatalog
	authority    *cluster.RaftCatalog
	graph        *ttcsnapshot.Store
	graphData    *ttcsnapshot.LocalDataStore
	graphShared  *ttcsnapshot.SharedPackStore
	sharedRefs   *sharedReferenceService
	peers        *distributedSourceRuntime
	event        func(DaemonEvent)
}

// FenceCancellation orders cancellation against the publication's one
// irreversible Raft commit before waiting for data-plane cleanup.
func (p *branchPublisher) FenceCancellation(intent ttcstorage.Intent) error {
	if p.authority == nil {
		return nil
	}
	return p.authority.CancelPublication(intent.Container, intent.Branch, intent.PublicationID)
}

func (p *branchPublisher) Publish(_ context.Context, intent ttcstorage.Intent) (ttcstorage.Result, error) {
	store := ttcbundle.Store{Root: p.sharedRoot, ActiveRoot: p.localRoot}
	info, err := store.LoadContainer(intent.Container)
	if err != nil {
		return ttcstorage.Result{}, err
	}
	branch, err := persistentBranchInfo(info, intent.Branch)
	if err != nil {
		return ttcstorage.Result{}, err
	}
	if branch.PublicationID != intent.PublicationID {
		return ttcstorage.Result{}, fmt.Errorf("publication identity changed to %q", branch.PublicationID)
	}
	path := coldManifestPath(p.sharedRoot, info.ID, branch.ID, intent.PublicationID)
	if existing, err := readColdBranchManifest(path); err == nil {
		if existing.Container != info.ID || existing.Branch != branch.ID || existing.PublicationID != intent.PublicationID || existing.Kind != intent.Kind {
			return ttcstorage.Result{}, errors.New("existing cold manifest has a conflicting identity")
		}
		for _, filesystem := range existing.Filesystems {
			if filesystem.Root.Root == "" || len(filesystem.Origins) == 0 {
				return ttcstorage.Result{}, errors.New("existing cold manifest has an incomplete filesystem snapshot")
			}
			for _, origin := range filesystem.Origins {
				if _, err := p.graphShared.Manifest(ttcsnapshot.SnapshotID(origin)); err != nil {
					return ttcstorage.Result{}, fmt.Errorf("validating published filesystem origin %d: %w", origin, err)
				}
			}
		}
		if existing.MemorySnapshot != 0 {
			if existing.MemorySnapshot != branch.SnapshotID || p.graphShared == nil {
				return ttcstorage.Result{}, errors.New("existing cold manifest has a conflicting graph memory snapshot")
			}
			published, err := p.graphShared.Manifest(ttcsnapshot.SnapshotID(existing.MemorySnapshot))
			if err != nil {
				return ttcstorage.Result{}, fmt.Errorf("validating published graph memory snapshot: %w", err)
			}
			control, err := ttcsnapshot.ParseObjectID(existing.ControlRoot)
			if err != nil {
				return ttcstorage.Result{}, fmt.Errorf("validating published graph control root: %w", err)
			}
			found := false
			for _, object := range published.Objects {
				if object.Object.ID == control {
					found = true
					break
				}
			}
			if !found {
				return ttcstorage.Result{}, fmt.Errorf("published graph control root %s is missing", control)
			}
		}
		return ttcstorage.Result{Manifest: path}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return ttcstorage.Result{}, err
	}
	// A branch rehydrated lazily from an older cold snapshot may still contain
	// deferred graph nodes. Reattach their immutable shared source before
	// sealing the next snapshot; resolver attachments are intentionally
	// process-local and must be reconstructible after a daemon restart.
	priorFilesystems := make(map[string]coldFilesystemSnapshot)
	if branch.StorageManifest != "" {
		prior, err := readColdBranchManifest(branch.StorageManifest)
		if err != nil {
			return ttcstorage.Result{}, fmt.Errorf("reading prior cold filesystem manifest: %w", err)
		}
		for _, filesystem := range prior.Filesystems {
			priorFilesystems[filesystem.Root.Source] = filesystem
		}
	}
	staging := filepath.Join(p.localRoot, "publication-staging", intent.ID)
	if err := os.RemoveAll(staging); err != nil {
		return ttcstorage.Result{}, err
	}
	if err := os.MkdirAll(staging, 0700); err != nil {
		return ttcstorage.Result{}, err
	}
	defer os.RemoveAll(staging)
	manifest := coldBranchManifest{Version: 5, Container: info.ID, Branch: branch.ID, PublicationID: intent.PublicationID, Kind: intent.Kind, CreatedAt: time.Now().UTC()}
	spec, err := store.LoadSpec(info.ID)
	if err != nil {
		return ttcstorage.Result{}, err
	}
	repositories, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		return ttcstorage.Result{}, err
	}
	seen := make(map[string]struct{})
	for _, config := range repositories {
		path := filepath.Clean(config.Path)
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		repository, err := ttcvolume.OpenRepository(path, ttcvolume.CopyMode(config.CopyMode))
		if err != nil {
			return ttcstorage.Result{}, fmt.Errorf("opening active repository %q: %w", config.Source, err)
		}
		physicalBranch := repositoryPhysicalBranch(config, intent.FilesystemBranch)
		var relayClient *os.File
		if prior, ok := priorFilesystems[config.Source]; ok {
			var token ttcvolume.RemoteToken
			if _, err := rand.Read(token[:]); err != nil {
				repository.Close()
				return ttcstorage.Result{}, err
			}
			client, _, err := startSharedFilesystemRelay(p.sharedRoot, prior.Root.Root, config.Source, prior.Origins, token)
			if err != nil {
				repository.Close()
				return ttcstorage.Result{}, fmt.Errorf("starting shared filesystem resolver for %q: %w", config.Source, err)
			}
			key := ttcvolume.RemoteSourceKey(sha256.Sum256([]byte(config.Source)))
			if err := repository.AttachRemoteObjectSource(client, token, key, physicalBranch); err != nil {
				client.Close()
				repository.Close()
				return ttcstorage.Result{}, fmt.Errorf("attaching shared filesystem resolver for %q: %w", config.Source, err)
			}
			relayClient = client
		}
		descriptor, snapshotErr := repository.DescribeRemoteSnapshot(physicalBranch)
		origins, originsErr := repository.PrepareBranchPublication(physicalBranch)
		snapshotErr = errors.Join(snapshotErr, originsErr)
		for _, origin := range origins {
			if snapshotErr == nil {
				snapshotErr = repository.PublishOriginHandoff(context.Background(), p.graphShared, origin)
			}
		}
		closeErr := repository.Close()
		if relayClient != nil {
			if err := relayClient.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
				closeErr = errors.Join(closeErr, err)
			}
		}
		if snapshotErr != nil || closeErr != nil {
			return ttcstorage.Result{}, errors.Join(snapshotErr, closeErr)
		}
		encodedOrigins := make([]uint64, len(origins))
		for i, origin := range origins {
			encodedOrigins[i] = uint64(origin)
		}
		restoreID := config.RestoreID
		if restoreID == "" {
			restoreID = descriptor.Snapshot.RepositoryID
		}
		manifest.Filesystems = append(manifest.Filesystems, coldFilesystemSnapshot{
			Root: cluster.FilesystemRootDescriptor{
				Source: config.Source, RepositoryID: descriptor.Snapshot.RepositoryID,
				RestoreID: restoreID,
				BranchID:  descriptor.Snapshot.BranchID, Root: descriptor.Root.String(),
				NextInode: descriptor.Snapshot.NextInode, LogicalBytes: descriptor.Snapshot.LogicalBytes,
			},
			Origins: encodedOrigins,
		})
	}
	if intent.Kind == "dehydrate" {
		if branch.SnapshotID != 0 && len(branch.MemoryRoots) != 0 {
			if p.graph == nil || p.graphData == nil || p.graphShared == nil {
				return ttcstorage.Result{}, errors.New("graph-native snapshot publication is unavailable")
			}
			snapshot := ttcsnapshot.SnapshotID(branch.SnapshotID)
			if err := p.graphShared.PublishHandoff(context.Background(), p.graph, snapshot, p.graphData); err != nil {
				return ttcstorage.Result{}, fmt.Errorf("publishing graph memory snapshot %d: %w", snapshot, err)
			}
			manifest.MemorySnapshot = branch.SnapshotID
			if err := validateGraphControlRoot(branch.SnapshotID, branch.ControlRoot, branch.ControlRootObject); err != nil {
				return ttcstorage.Result{}, fmt.Errorf("validating graph-native control root: %w", err)
			}
			manifest.ControlRoot = branch.ControlRootObject
		} else {
			return ttcstorage.Result{}, errors.New("new-format dehydration branch lacks graph-native snapshot identity")
		}
	}
	sort.Slice(manifest.Filesystems, func(i, j int) bool { return manifest.Filesystems[i].Root.Source < manifest.Filesystems[j].Root.Source })
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return ttcstorage.Result{}, err
	}
	if err := writeDurableJSON(path, &manifest); err != nil {
		return ttcstorage.Result{}, err
	}
	return ttcstorage.Result{Manifest: path}, nil
}

func validateColdManifestPath(sharedRoot, path string) error {
	base := filepath.Join(sharedRoot, "cold", "manifests")
	relative, err := filepath.Rel(base, path)
	if err != nil || relative == "." || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("cold manifest %q escapes TTC_ROOT", path)
	}
	return nil
}

func coldManifestPath(sharedRoot, container, branch, publicationID string) string {
	return filepath.Join(sharedRoot, "cold", "manifests", hex.EncodeToString([]byte(container)), hex.EncodeToString([]byte(branch)), hex.EncodeToString([]byte(publicationID))+".json")
}

func (p *branchPublisher) CaptureAvailable(intent ttcstorage.Intent) (bool, error) {
	store := ttcbundle.Store{Root: p.sharedRoot, ActiveRoot: p.localRoot}
	info, err := store.LoadContainer(intent.Container)
	if err != nil {
		return false, err
	}
	spec, err := store.LoadSpec(info.ID)
	if err != nil {
		return false, err
	}
	repositories, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		return false, err
	}
	seen := make(map[string]struct{})
	for _, config := range repositories {
		path := filepath.Clean(config.Path)
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		repository, err := ttcvolume.OpenRepository(path, ttcvolume.CopyMode(config.CopyMode))
		if err != nil {
			return false, err
		}
		_, branchErr := repository.Branch(repositoryPhysicalBranch(config, intent.FilesystemBranch))
		closeErr := repository.Close()
		if errors.Is(branchErr, os.ErrNotExist) {
			return false, closeErr
		}
		if branchErr != nil || closeErr != nil {
			return false, errors.Join(branchErr, closeErr)
		}
	}
	if intent.Kind == "dehydrate" {
		branch, branchErr := persistentBranchInfo(info, intent.Branch)
		if branchErr != nil {
			return false, branchErr
		}
		if branch.SnapshotID != 0 && len(branch.MemoryRoots) != 0 {
			if p.graph == nil || p.graphShared == nil || p.graphData == nil {
				return false, errors.New("graph-native snapshot publication is unavailable")
			}
			snapshot := ttcsnapshot.SnapshotID(branch.SnapshotID)
			record, ok := p.graph.Snapshot(snapshot)
			if !ok || (record.State != ttcsnapshot.SnapshotSealedVolatile && record.State != ttcsnapshot.SnapshotShared) {
				return false, nil
			}
			if record.State == ttcsnapshot.SnapshotShared {
				if _, err := p.graphShared.Manifest(snapshot); err != nil {
					if errors.Is(err, os.ErrNotExist) {
						return false, nil
					}
					return false, err
				}
			}
			if err := validateGraphControlRoot(branch.SnapshotID, branch.ControlRoot, branch.ControlRootObject); err != nil {
				return false, err
			}
		} else {
			return false, errors.New("new-format dehydration branch lacks graph-native snapshot identity")
		}
	}
	if p.authority != nil {
		lifecycle := "exited"
		manifestPath := coldManifestPath(p.sharedRoot, info.ID, intent.Branch, intent.PublicationID)
		relativeManifest, err := filepath.Rel(p.sharedRoot, manifestPath)
		if err != nil || filepath.IsAbs(relativeManifest) || strings.HasPrefix(relativeManifest, ".."+string(filepath.Separator)) {
			return false, fmt.Errorf("cold manifest %q is outside shared root", manifestPath)
		}
		captureRoots := cluster.DurableRoots{FilesystemRoot: relativeManifest}
		if intent.Kind == "dehydrate" {
			lifecycle = "dehydrated"
			branch, err := persistentBranchInfo(info, intent.Branch)
			if err != nil {
				return false, err
			}
			if branch.SnapshotID == 0 || len(branch.MemoryRoots) == 0 {
				return false, errors.New("new-format dehydration branch lacks graph-native snapshot identity")
			}
			captureRoots.MemorySnapshot = branch.SnapshotID
		}
		if _, err := p.authority.EnsurePublicationReady(info.ID, intent.Branch, intent.PublicationID, lifecycle, captureRoots); err != nil {
			return false, fmt.Errorf("authorizing publication capture in Raft: %w", err)
		}
	}
	return true, nil
}

func (p *branchPublisher) Cleanup(intent ttcstorage.Intent) error {
	canceled := p.authority != nil
	if p.authority != nil {
		if err := p.authority.CancelPublication(intent.Container, intent.Branch, intent.PublicationID); err != nil {
			return fmt.Errorf("canceling publication authority: %w", err)
		}
		record, ok, err := p.authority.Publication(intent.Container, intent.Branch, intent.PublicationID)
		if err != nil {
			return fmt.Errorf("reading publication authority after cleanup fence: %w", err)
		}
		canceled = ok && record.State == cluster.PublicationCanceled
	}
	manifestPath := coldManifestPath(p.sharedRoot, intent.Container, intent.Branch, intent.PublicationID)
	var published coldBranchManifest
	haveManifest := false
	if manifest, err := readColdBranchManifest(manifestPath); err == nil {
		published, haveManifest = manifest, true
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("reading canceled publication manifest: %w", err)
	}
	store := ttcbundle.Store{Root: p.sharedRoot, ActiveRoot: p.localRoot}
	info, err := store.LoadContainer(intent.Container)
	if err != nil {
		return err
	}
	spec, err := store.LoadSpec(info.ID)
	if err != nil {
		return err
	}
	repositories, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		return err
	}
	seen := make(map[string]struct{})
	filesystemOrigins := make(map[string][]uint64)
	for _, filesystem := range published.Filesystems {
		filesystemOrigins[filesystem.Root.Source] = append(filesystemOrigins[filesystem.Root.Source], filesystem.Origins...)
	}
	emptyFilesystemOrigins := make(map[uint64]bool)
	for _, config := range repositories {
		path := filepath.Clean(config.Path)
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		repository, err := ttcvolume.OpenRepository(path, ttcvolume.CopyMode(config.CopyMode))
		if err != nil {
			return err
		}
		for _, encodedOrigin := range filesystemOrigins[config.Source] {
			if !canceled {
				break
			}
			origin := ttcsnapshot.SnapshotID(encodedOrigin)
			provider, volatile, providerErr := p.authority.SnapshotProvider(encodedOrigin)
			if providerErr != nil {
				repository.Close()
				return providerErr
			}
			if volatile && provider.NodeID == p.nodeID {
				if err := repository.CancelOriginHandoff(origin); err != nil {
					repository.Close()
					return fmt.Errorf("canceling filesystem origin %d handoff: %w", origin, err)
				}
			}
		}
		removeErr := repository.RemoveBranch(repositoryPhysicalBranch(config, intent.FilesystemBranch))
		if errors.Is(removeErr, os.ErrNotExist) {
			removeErr = nil
		}
		gcErr := repository.GarbageCollect()
		for _, encodedOrigin := range filesystemOrigins[config.Source] {
			empty, emptyErr := repository.OriginEmpty(ttcsnapshot.SnapshotID(encodedOrigin))
			if emptyErr != nil {
				closeErr := repository.Close()
				return errors.Join(emptyErr, closeErr)
			}
			emptyFilesystemOrigins[encodedOrigin] = empty
		}
		closeErr := repository.Close()
		if removeErr != nil || gcErr != nil || closeErr != nil {
			return errors.Join(removeErr, gcErr, closeErr)
		}
	}
	for encodedOrigin, empty := range emptyFilesystemOrigins {
		if !canceled {
			break
		}
		provider, volatile, err := p.authority.SnapshotProvider(encodedOrigin)
		if err != nil {
			return err
		}
		if !volatile || provider.NodeID != p.nodeID {
			continue
		}
		if p.graphShared != nil {
			busy, err := p.graphShared.DiscardHandoff(ttcsnapshot.SnapshotID(encodedOrigin))
			if err != nil || busy {
				return errors.Join(err, func() error {
					if busy {
						return fmt.Errorf("filesystem origin %d handoff pack is busy", encodedOrigin)
					}
					return nil
				}())
			}
		}
		if empty {
			if err := p.authority.AbandonSnapshotOrigin(p.nodeID, p.incarnation, encodedOrigin); err != nil {
				return fmt.Errorf("abandoning empty filesystem origin %d: %w", encodedOrigin, err)
			}
		}
	}
	if canceled && haveManifest && published.MemorySnapshot != 0 && p.graph != nil && p.graphShared != nil {
		snapshot := ttcsnapshot.SnapshotID(published.MemorySnapshot)
		provider, volatile, err := p.authority.SnapshotProvider(published.MemorySnapshot)
		if err != nil {
			return err
		}
		if volatile && provider.NodeID == p.nodeID {
			if err := p.graph.UnfreezeSnapshotLedger(snapshot); err != nil {
				return fmt.Errorf("canceling memory origin %d handoff: %w", snapshot, err)
			}
			busy, err := p.graphShared.DiscardHandoff(snapshot)
			if err != nil || busy {
				return errors.Join(err, func() error {
					if busy {
						return fmt.Errorf("memory origin %d handoff pack is busy", snapshot)
					}
					return nil
				}())
			}
		}
	}
	if canceled && haveManifest {
		if err := os.Remove(manifestPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		// The publication manifest is authoritative; its branch/family
		// directories are only namespace scaffolding. Remove empty scaffolding
		// after cancellation so repeated branch creation does not leave one
		// directory per historical name. Non-empty directories contain another
		// live publication or branch and are retained.
		for _, directory := range []string{filepath.Dir(manifestPath), filepath.Dir(filepath.Dir(manifestPath))} {
			if err := os.Remove(directory); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) {
				return err
			}
		}
	}
	return nil
}

// releasePublishedProviderGraphRoots drops the temporary roots installed for
// remote consumers once Raft routes the origin to its committed shared pack.
// Branch-local roots remain untouched, and targets now resolve the immutable
// data from shared storage, so the provider copy is cache rather than
// authority after this boundary.
func releasePublishedProviderGraphRoots(graph *ttcsnapshot.Store, data *ttcsnapshot.LocalDataStore, snapshot ttcsnapshot.SnapshotID, exactRoots ...[]string) error {
	if graph == nil || snapshot == 0 {
		return nil
	}
	prefix := fmt.Sprintf("remote/%d/", snapshot)
	exact := make(map[string]struct{})
	if len(exactRoots) != 0 {
		for _, name := range exactRoots[0] {
			exact[name] = struct{}{}
		}
	}
	bindings := make(map[string]ttcsnapshot.ObjectID)
	for name, object := range graph.Inspect().Roots {
		_, explicitlySelected := exact[name]
		if !explicitlySelected && !strings.HasPrefix(name, prefix) && !(strings.HasPrefix(name, "bridge/edge/") && object.Snapshot == snapshot) {
			continue
		}
		if object.Snapshot != snapshot {
			return fmt.Errorf("provider root %q selects foreign object %s", name, object)
		}
		bindings[name] = object
	}
	if err := graph.ReleaseRetainedRoots(bindings); err != nil {
		return err
	}
	for {
		collected, err := graph.Collect(4096)
		if err != nil {
			return err
		}
		if collected == 0 {
			break
		}
	}
	if data != nil {
		if _, _, err := data.ReclaimUnreferenced(graph); err != nil {
			return err
		}
	}
	return nil
}

func (p *branchPublisher) Complete(intent ttcstorage.Intent, result ttcstorage.Result) error {
	store := ttcbundle.Store{Root: p.sharedRoot, ActiveRoot: p.localRoot}
	info, err := store.LoadContainer(intent.Container)
	if err != nil {
		return err
	}
	lifecycle := ""
	if intent.Kind == "dehydrate" {
		lifecycle = "dehydrated"
	}
	if p.authority != nil {
		branch, branchErr := persistentBranchInfo(info, intent.Branch)
		if branchErr != nil {
			return branchErr
		}
		manifest, err := readColdBranchManifest(result.Manifest)
		if err != nil {
			return fmt.Errorf("reading committed cold manifest: %w", err)
		}
		var handoffOrigins []uint64
		if intent.Kind == "dehydrate" && branch.SnapshotID != 0 && len(branch.MemoryRoots) != 0 {
			snapshot := ttcsnapshot.SnapshotID(branch.SnapshotID)
			_, volatile, err := p.authority.SnapshotProvider(branch.SnapshotID)
			if err != nil {
				return err
			}
			if volatile {
				if p.graph == nil {
					return errors.New("snapshot graph is unavailable for shared reference handoff")
				}
				if _, _, err := p.graph.FreezeSnapshotLedger(snapshot); err != nil {
					return fmt.Errorf("freezing snapshot origin %d reference handoff: %w", branch.SnapshotID, err)
				}
				changes, _, frozen, err := p.graph.SnapshotLedgerHandoffChanges(snapshot, 0)
				if err != nil {
					return fmt.Errorf("reading snapshot origin %d reference catch-up: %w", branch.SnapshotID, err)
				}
				if !frozen {
					return fmt.Errorf("snapshot origin %d reference handoff is not frozen", branch.SnapshotID)
				}
				if err := p.publishSharedReferenceLedger(snapshot, changes); err != nil {
					return fmt.Errorf("transferring snapshot origin %d reference authority: %w", branch.SnapshotID, err)
				}
				handoffOrigins = append(handoffOrigins, branch.SnapshotID)
			}
		}
		spec, err := store.LoadSpec(info.ID)
		if err != nil {
			return err
		}
		configs, err := specutils.TTCVolumeRepositories(spec)
		if err != nil {
			return err
		}
		configBySource := make(map[string]specutils.TTCVolumeRepository, len(configs))
		for _, config := range configs {
			configBySource[config.Source] = config
		}
		for _, filesystem := range manifest.Filesystems {
			config, ok := configBySource[filesystem.Root.Source]
			if !ok {
				return fmt.Errorf("published filesystem source %q is absent from the container spec", filesystem.Root.Source)
			}
			repository, err := ttcvolume.OpenRepository(config.Path, ttcvolume.CopyMode(config.CopyMode))
			if err != nil {
				return err
			}
			for _, encodedOrigin := range filesystem.Origins {
				origin := ttcsnapshot.SnapshotID(encodedOrigin)
				if _, err := p.graphShared.Manifest(origin); err != nil {
					repository.Close()
					return fmt.Errorf("validating filesystem snapshot origin %d: %w", origin, err)
				}
				_, volatile, err := p.authority.SnapshotProvider(encodedOrigin)
				if err != nil {
					repository.Close()
					return err
				}
				if volatile {
					changes, err := repository.FreezeOriginHandoff(origin)
					if err != nil {
						repository.Close()
						return fmt.Errorf("freezing filesystem snapshot origin %d: %w", origin, err)
					}
					if err := p.publishSharedReferenceLedger(origin, changes); err != nil {
						repository.Close()
						return fmt.Errorf("transferring filesystem snapshot origin %d reference authority: %w", origin, err)
					}
					handoffOrigins = append(handoffOrigins, encodedOrigin)
				}
			}
			if err := repository.Close(); err != nil {
				return err
			}
		}
		relativeManifest, err := filepath.Rel(p.sharedRoot, result.Manifest)
		if err != nil || filepath.IsAbs(relativeManifest) || strings.HasPrefix(relativeManifest, ".."+string(filepath.Separator)) {
			return fmt.Errorf("cold manifest %q is outside shared root", result.Manifest)
		}
		roots := cluster.DurableRoots{FilesystemRoot: relativeManifest}
		if intent.Kind == "dehydrate" {
			if branch.SnapshotID == 0 || len(branch.MemoryRoots) == 0 {
				return errors.New("new-format dehydration branch lacks graph-native snapshot identity")
			}
			// Snapshot identity, not a runtime checksum, is the protocol root.
			roots.MemorySnapshot = branch.SnapshotID
		}
		// This single Raft entry is the irreversible publication boundary. It
		// installs branch roots, releases placement, and changes every prepared
		// origin from provider-owned to shared together. Cancellation before it
		// reverses every handoff; cancellation after it is a no-op.
		if _, err := p.authority.CommitPublication(info.ID, intent.Branch, intent.PublicationID, roots, p.nodeID, p.incarnation, handoffOrigins); err != nil {
			return fmt.Errorf("committing publication roots in Raft: %w", err)
		}
		if intent.Kind == "dehydrate" && branch.SnapshotID != 0 && p.graph != nil {
			snapshot := ttcsnapshot.SnapshotID(branch.SnapshotID)
			if err := p.graph.MarkShared(snapshot); err != nil {
				return fmt.Errorf("marking snapshot origin %d shared: %w", branch.SnapshotID, err)
			}
			if err := p.graph.UnfreezeSnapshotLedger(snapshot); err != nil {
				return fmt.Errorf("consuming snapshot origin %d reference handoff: %w", branch.SnapshotID, err)
			}
			if err := releasePublishedProviderGraphRoots(p.graph, p.graphData, snapshot, branchGraphRootNames(branch)); err != nil {
				return fmt.Errorf("releasing volatile provider roots for snapshot origin %d: %w", branch.SnapshotID, err)
			}
		}
		for _, filesystem := range manifest.Filesystems {
			config := configBySource[filesystem.Root.Source]
			repository, err := ttcvolume.OpenRepository(config.Path, ttcvolume.CopyMode(config.CopyMode))
			if err != nil {
				return err
			}
			for _, encodedOrigin := range filesystem.Origins {
				if err := repository.CompleteOriginHandoff(ttcsnapshot.SnapshotID(encodedOrigin)); err != nil {
					repository.Close()
					return fmt.Errorf("completing filesystem snapshot origin %d handoff: %w", encodedOrigin, err)
				}
			}
			if err := repository.Close(); err != nil {
				return err
			}
		}
	} else if p.nodeID != "" {
		if p.placements == nil {
			return errors.New("placement authority is unavailable")
		}
		if err := p.releasePublishedPlacement(p.placements, info.ID, intent.Branch); err != nil {
			return err
		}
	}
	// Publish branch-visible durability only after the placement is unplaced.
	// Thus wait --condition=durable cannot return during the narrow interval in
	// which a cold branch is still routed to a dead runtime incarnation.
	if err := store.CompleteBranchPublication(intent.Container, intent.Branch, intent.PublicationID, result.Manifest, lifecycle, true); err != nil {
		return err
	}
	if intent.Kind != "dehydrate" {
		p.recordStorageEvent(intent, "shared", "", result.Manifest)
		return nil
	}
	info, err = store.LoadContainer(intent.Container)
	if err != nil {
		return err
	}
	branch, err := persistentBranchInfo(info, intent.Branch)
	if err != nil {
		return err
	}
	transactionID := transactionIDForBranch(info, branch)
	if transactionID == "" {
		p.recordStorageEvent(intent, "shared", "", result.Manifest)
		return nil
	}
	transactions := p.transactions
	if transactions == nil {
		return errors.New("transaction authority is unavailable")
	}
	tx, err := transactions.Load(transactionID)
	if err != nil {
		return err
	}
	switch tx.State {
	case cluster.TransactionPublished, cluster.TransactionDraining:
		_, err = transactions.Advance(transactionID, tx.Revision, cluster.TransactionIndependent, "", time.Now())
		if err == nil {
			p.recordStorageEvent(intent, "shared", "", result.Manifest)
		}
		return err
	case cluster.TransactionIndependent:
		p.recordStorageEvent(intent, "shared", "", result.Manifest)
		return nil
	default:
		return fmt.Errorf("distributed fork transaction %q is %s", transactionID, tx.State)
	}
}

func (p *branchPublisher) publishSharedReferenceLedger(snapshot ttcsnapshot.SnapshotID, changes []ttcsnapshot.SnapshotReferenceMutation) error {
	if p == nil || p.authority == nil || p.graphShared == nil || snapshot == 0 {
		return errors.New("shared reference publication is unavailable")
	}
	lease, err := p.authority.SharedGCLease()
	if err != nil {
		return err
	}
	if lease.Generation == 0 || !time.Now().Before(lease.ExpiresAt) {
		return errors.New("shared reference coordinator has no active lease")
	}
	var importOrigin func(context.Context, uint64) error
	var retainBridge func(context.Context, uint64, string, string, string) error
	var applyHandoff func(context.Context, uint64, uint64, []cluster.SharedReferenceMutation) error
	var flushOrigin func(context.Context, uint64) error
	if lease.NodeID == p.nodeID && lease.Incarnation == p.incarnation {
		importOrigin = func(ctx context.Context, snapshot uint64) error {
			return p.sharedRefs.ImportSharedSnapshot(ctx, p.nodeID, snapshot)
		}
		retainBridge = func(_ context.Context, _ uint64, operation, name, object string) error {
			parsed, err := ttcsnapshot.ParseObjectID(object)
			if err != nil {
				return err
			}
			return p.sharedRefs.mutateReference(operation, name, parsed, true)
		}
		applyHandoff = func(ctx context.Context, snapshot, from uint64, mutations []cluster.SharedReferenceMutation) error {
			return p.sharedRefs.ApplySharedSnapshotHandoff(ctx, p.nodeID, snapshot, from, mutations)
		}
		flushOrigin = func(ctx context.Context, snapshot uint64) error {
			return p.sharedRefs.FlushSharedSnapshot(ctx, p.nodeID, snapshot)
		}
	} else {
		if p.peers == nil {
			return errors.New("cluster peer routing is unavailable")
		}
		peer, err := p.peers.peer(lease.NodeID, false)
		if err != nil {
			return err
		}
		importOrigin = peer.ImportSharedSnapshot
		retainBridge = func(ctx context.Context, snapshot uint64, operation, name, object string) error {
			return peer.ApplySharedSnapshotReference(ctx, snapshot, operation, name, object, true)
		}
		applyHandoff = peer.ApplySharedSnapshotHandoff
		flushOrigin = peer.FlushSharedSnapshot
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := importOrigin(ctx, uint64(snapshot)); err != nil {
		return err
	}
	// The pack baseline may contain roots that disappear while bulk data is
	// being written (for example when a stopped branch releases its runtime
	// roots). Pin those roots under publication-owned names before replaying the
	// handoff delta. Otherwise the last provider-root release can put an object
	// into the irreversible reclaiming state immediately before Raft publishes
	// the equivalent cold branch root. Authority reconciliation retains the
	// final cold roots before releasing these bridge roots after the atomic
	// provider-to-shared transition.
	manifest, err := p.graphShared.Manifest(snapshot)
	if err != nil {
		return fmt.Errorf("reading publication bridge roots: %w", err)
	}
	for _, bridge := range publicationBridgeReferences(snapshot, manifest.References) {
		if err := retainBridge(ctx, uint64(snapshot), bridge.Operation, bridge.Name, bridge.Object.String()); err != nil {
			return fmt.Errorf("retaining publication bridge %q: %w", bridge.Name, err)
		}
	}
	// A mutation includes stable string identities and is substantially larger
	// than an ObjectID. Keep batches below both the 8 MiB ledger record bound
	// and the 1 MiB peer control-body bound even for maximum-length names.
	const batchSize = 256
	from := uint64(0)
	for len(changes) != 0 {
		count := min(len(changes), batchSize)
		wire := make([]cluster.SharedReferenceMutation, count)
		for index, mutation := range changes[:count] {
			wire[index] = cluster.SharedReferenceMutation{
				Sequence: mutation.Sequence, Operation: mutation.Operation, Name: mutation.Name,
				Object: mutation.Object.String(), Retain: mutation.Retain,
			}
		}
		if err := applyHandoff(ctx, uint64(snapshot), from, wire); err != nil {
			return err
		}
		from = wire[len(wire)-1].Sequence
		changes = changes[count:]
	}
	return flushOrigin(ctx, uint64(snapshot))
}

type publicationBridgeReference struct {
	Operation string
	Name      string
	Object    ttcsnapshot.ObjectID
}

func publicationBridgeReferences(snapshot ttcsnapshot.SnapshotID, references map[string]ttcsnapshot.ObjectID) []publicationBridgeReference {
	bridges := make([]publicationBridgeReference, 0)
	for name, object := range references {
		if !strings.HasPrefix(name, "root/") && !strings.HasPrefix(name, "pin/") {
			continue
		}
		digest := sha256.Sum256([]byte(name + "\x00" + object.String()))
		bridgeName := fmt.Sprintf("root/publication-bridge/%016x/%x", uint64(snapshot), digest)
		bridges = append(bridges, publicationBridgeReference{
			// Operation crosses the cluster control protocol and therefore uses
			// its bounded opaque-ID alphabet. Name is ledger-local and may retain
			// the hierarchical spelling.
			Operation: fmt.Sprintf("publication-bridge-retain-%016x-%x", uint64(snapshot), digest),
			Name:      bridgeName,
			Object:    object,
		})
	}
	sort.Slice(bridges, func(i, j int) bool { return bridges[i].Name < bridges[j].Name })
	return bridges
}

func fileSHA256Digest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), errors.Join(copyErr, file.Close())
}

// releasePublishedPlacement releases runtime ownership after a process-free
// branch has become durable. A daemon that recovered an activated publication
// intent may have a new incarnation. Node admission already proves that the
// previous incarnation of the same node ID has expired, so adopt it before
// releasing the placement. Both transitions are identity-fenced and a
// crash between them is safe to retry from the next incarnation.
func (p *branchPublisher) releasePublishedPlacement(placements cluster.PlacementCatalog, family, branch string) error {
	record, err := placements.Lookup(family, branch)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// Families created before the placement catalog are still valid.
		return nil
	case err != nil:
		return err
	case record.State == cluster.PlacementUnplaced:
		return nil
	case record.OwnerNode != p.nodeID:
		return fmt.Errorf("published branch placement belongs to %s/%s, not local incarnation %s/%s", record.OwnerNode, record.OwnerIncarnation, p.nodeID, p.incarnation)
	}
	if record.OwnerIncarnation != p.incarnation {
		record, err = placements.AdoptOwnerIncarnation(family, branch, p.nodeID, record.OwnerIncarnation, p.incarnation, record.Generation, time.Now())
		if err != nil {
			return fmt.Errorf("adopting recovered publication placement: %w", err)
		}
	}
	if _, err := placements.Unplace(record.FamilyID, record.BranchID, p.nodeID, p.incarnation, record.Generation, time.Now()); err != nil {
		return fmt.Errorf("releasing durable branch placement: %w", err)
	}
	return nil
}

func (p *branchPublisher) Failed(intent ttcstorage.Intent, cause error, retryAt time.Time) error {
	store := ttcbundle.Store{Root: p.sharedRoot, ActiveRoot: p.localRoot}
	err := store.UpdateBranchStorage(intent.Container, intent.Branch, intent.PublicationID, "error", cause.Error(), retryAt)
	if err == nil {
		p.recordStorageEvent(intent, "error", cause.Error(), "")
	}
	return err
}

func (p *branchPublisher) recordStorageEvent(intent ttcstorage.Intent, state, storageError, manifest string) {
	if p.event == nil {
		return
	}
	attributes := map[string]string{"state": state, "publication_id": intent.PublicationID, "kind": intent.Kind}
	if storageError != "" {
		attributes["error"] = storageError
	}
	if manifest != "" {
		attributes["manifest"] = manifest
	}
	p.event(DaemonEvent{Type: "storage", Action: state, Subject: intent.Container + "@" + intent.Branch, Attributes: attributes})
}

func readColdBranchManifest(path string) (coldBranchManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return coldBranchManifest{}, err
	}
	var manifest coldBranchManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return coldBranchManifest{}, err
	}
	if manifest.Version != 5 {
		return coldBranchManifest{}, fmt.Errorf("cold manifest format %d is unsupported; TTC requires snapshot-graph format 5", manifest.Version)
	}
	if manifest.Container == "" || manifest.Branch == "" || !transactionIDPatternForDaemon(manifest.PublicationID) || (manifest.Kind != "stop" && manifest.Kind != "dehydrate") {
		return coldBranchManifest{}, errors.New("cold manifest has invalid identity or lifecycle metadata")
	}
	seenSources := make(map[string]struct{}, len(manifest.Filesystems))
	seenOrigins := make(map[uint64]struct{})
	for _, filesystem := range manifest.Filesystems {
		root := filesystem.Root
		if root.Source == "" || root.RepositoryID == "" || root.RestoreID == "" || root.BranchID == "" || root.Root == "" || root.NextInode < 2 {
			return coldBranchManifest{}, errors.New("cold manifest contains an incomplete filesystem root")
		}
		if _, duplicate := seenSources[root.Source]; duplicate {
			return coldBranchManifest{}, fmt.Errorf("cold manifest repeats filesystem source %q", root.Source)
		}
		seenSources[root.Source] = struct{}{}
		if _, err := ttcvolume.NewRemoteSnapshotDescriptor(root.RepositoryID, root.BranchID, root.Root, root.NextInode, root.LogicalBytes); err != nil {
			return coldBranchManifest{}, fmt.Errorf("validating filesystem source %q: %w", root.Source, err)
		}
		for _, origin := range filesystem.Origins {
			if origin == 0 {
				return coldBranchManifest{}, errors.New("cold manifest contains snapshot origin zero")
			}
			if _, duplicate := seenOrigins[origin]; duplicate {
				return coldBranchManifest{}, fmt.Errorf("cold manifest repeats snapshot origin %d", origin)
			}
			seenOrigins[origin] = struct{}{}
		}
	}
	if manifest.Kind == "dehydrate" {
		if manifest.MemorySnapshot == 0 {
			return coldBranchManifest{}, errors.New("dehydration manifest omits graph-native memory snapshot")
		}
		controlRoot, err := ttcsnapshot.ParseObjectID(manifest.ControlRoot)
		if err != nil || controlRoot.Kind != ttcsnapshot.ObjectKindMetadata || uint64(controlRoot.Snapshot) != manifest.MemorySnapshot {
			return coldBranchManifest{}, errors.New("dehydration manifest omits graph-native control state")
		}
	}
	return manifest, nil
}

func coldFilesystemRoots(manifest coldBranchManifest) []cluster.FilesystemRootDescriptor {
	roots := make([]cluster.FilesystemRootDescriptor, len(manifest.Filesystems))
	for index := range manifest.Filesystems {
		roots[index] = manifest.Filesystems[index].Root
	}
	return roots
}

func localGraphCaptureMatches(path string, branch ttcbundle.BranchInfo) bool {
	if branch.SnapshotID == 0 || len(branch.MemoryRoots) == 0 {
		return false
	}
	control, err := os.ReadFile(filepath.Join(path, "control.json"))
	if err != nil {
		return false
	}
	var capture boot.BranchDehydrateInfo
	if err := json.Unmarshal(control, &capture); err != nil {
		return false
	}
	if capture.SnapshotID != branch.SnapshotID || len(capture.MemoryRoots) != len(branch.MemoryRoots) || len(branch.MemoryRootObjects) != len(branch.MemoryRoots) {
		return false
	}
	wanted := make(map[string]string, len(branch.MemoryRoots))
	for index, name := range branch.MemoryRoots {
		wanted[name] = branch.MemoryRootObjects[index]
	}
	for _, root := range capture.MemoryRoots {
		if object, ok := wanted[root.Name]; !ok || object != root.Root {
			return false
		}
		delete(wanted, root.Name)
	}
	return len(wanted) == 0
}

func materializeSharedGraphFilesystem(ctx context.Context, shared *ttcsnapshot.SharedPackStore, snapshot ttcsnapshot.SnapshotID, encodedRoot, destination string) error {
	if shared == nil {
		return errors.New("shared graph snapshot repository is unavailable")
	}
	root, err := ttcsnapshot.ParseObjectID(encodedRoot)
	if err != nil || root.Snapshot != snapshot || root.Kind != ttcsnapshot.ObjectKindMetadata {
		return fmt.Errorf("invalid graph filesystem root %q for snapshot %d", encodedRoot, snapshot)
	}
	manifest, err := shared.Manifest(snapshot)
	if err != nil {
		return err
	}
	objects := make([]ttcsnapshot.Object, len(manifest.Objects))
	for index := range manifest.Objects {
		objects[index] = manifest.Objects[index].Object
	}
	catalog, err := ttcsnapshot.NewImmutableCatalog(objects)
	if err != nil {
		return err
	}
	return ttcsnapshot.MaterializeFilesystemFrom(ctx, catalog, func(ctx context.Context, id ttcsnapshot.ObjectID, expected int64) (io.ReadCloser, error) {
		reader, length, err := shared.Fetch(ctx, id)
		if err != nil {
			return nil, err
		}
		if length != uint64(expected) {
			_ = reader.Close()
			return nil, fmt.Errorf("graph object %s has length %d, want %d", id, length, expected)
		}
		return reader, nil
	}, root, destination)
}

func (b *runtimeBackend) ensureLocalColdBranch(info ttcbundle.ContainerInfo, branch ttcbundle.BranchInfo) (retErr error) {
	localImage := existingMemoryBranchDir(b.activeDataRoot(), info.ID, "dehydrated", branch)
	if _, err := os.Stat(localImage); err == nil {
		if branch.SnapshotID == 0 {
			return nil
		}
		if localGraphCaptureMatches(localImage, branch) {
			return nil
		}
		// A branch may return to a node after another node captured a newer
		// snapshot. Branch-name-only local control paths are caches, not
		// authority; discard an old or malformed tree before materializing the
		// manifest selected by the persistent branch publication identity.
		if err := os.RemoveAll(localImage); err != nil {
			return fmt.Errorf("discarding stale local dehydration control state: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// A prior materialization failure records storage_state=error while
	// preserving the committed manifest. Rehydration is retryable from that
	// manifest; lifecycle state must not make durable data unreachable.
	if branch.StorageManifest == "" {
		return fmt.Errorf("branch %s@%s has no local capture or committed shared manifest", info.Name, branch.ID)
	}
	if err := validateColdManifestPath(b.dataRoot, branch.StorageManifest); err != nil {
		return err
	}
	store := b.containerStore()
	if err := store.UpdateBranchStorage(info.ID, branch.ID, branch.PublicationID, "materializing", "", time.Time{}); err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			_ = store.UpdateBranchStorage(info.ID, branch.ID, branch.PublicationID, "error", retErr.Error(), time.Time{})
		}
	}()
	manifest, err := readColdBranchManifest(branch.StorageManifest)
	if err != nil {
		return err
	}
	if manifest.Container != info.ID || manifest.Branch != branch.ID || manifest.PublicationID != branch.PublicationID {
		return errors.New("cold manifest identity does not match branch publication")
	}
	if manifest.MemorySnapshot != branch.SnapshotID || manifest.ControlRoot != branch.ControlRootObject {
		return errors.New("cold manifest process-state roots do not match branch metadata")
	}
	if err := os.MkdirAll(filepath.Join(b.activeDataRoot(), "containers", info.ID), 0700); err != nil {
		return err
	}
	if len(manifest.Filesystems) == 0 {
		return errors.New("cold manifest omits filesystem graph roots")
	}
	if _, err := b.materializeDistributedFilesystemRoots(info, branch.FilesystemBranch, coldFilesystemRoots(manifest), true); err != nil {
		return err
	}
	if manifest.ControlRoot == "" {
		return errors.New("cold manifest omits dehydration control state")
	}
	if _, err := os.Stat(localImage); errors.Is(err, os.ErrNotExist) {
		if err := materializeSharedGraphFilesystem(context.Background(), b.snapshotShared, ttcsnapshot.SnapshotID(manifest.MemorySnapshot), manifest.ControlRoot, localImage); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if manifest.MemorySnapshot != 0 {
		if manifest.MemorySnapshot != branch.SnapshotID || len(branch.MemoryRoots) == 0 {
			return errors.New("cold manifest graph snapshot disagrees with branch memory roots")
		}
		if b.snapshotShared == nil {
			return errors.New("shared graph snapshot repository is unavailable")
		}
		if _, err := b.snapshotShared.Manifest(ttcsnapshot.SnapshotID(manifest.MemorySnapshot)); err != nil {
			return fmt.Errorf("opening shared graph memory snapshot: %w", err)
		}
		return store.UpdateBranchStorage(info.ID, branch.ID, branch.PublicationID, "materializing", "", time.Time{})
	}
	return errors.New("new-format cold manifest omits graph-native memory snapshot")
}

// ensureLocalColdFilesystem materializes the filesystem portion of a durable
// stopped branch without requiring dehydration memory records. A stopped
// branch has deliberately discarded execution state, but its next fresh start
// must still reconstruct the committed writable repositories on whichever
// node claims its unplaced Raft record.
func (b *runtimeBackend) ensureLocalColdFilesystem(info ttcbundle.ContainerInfo, branch ttcbundle.BranchInfo) (retErr error) {
	if branch.StorageManifest == "" {
		return nil
	}
	if err := validateColdManifestPath(b.dataRoot, branch.StorageManifest); err != nil {
		return err
	}
	store := b.containerStore()
	if err := store.UpdateBranchStorage(info.ID, branch.ID, branch.PublicationID, "materializing", "", time.Time{}); err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			_ = store.UpdateBranchStorage(info.ID, branch.ID, branch.PublicationID, "error", retErr.Error(), time.Time{})
		}
	}()
	manifest, err := readColdBranchManifest(branch.StorageManifest)
	if err != nil {
		return err
	}
	if manifest.Container != info.ID || manifest.Branch != branch.ID || manifest.PublicationID != branch.PublicationID {
		return errors.New("cold manifest identity does not match branch publication")
	}
	if len(manifest.Filesystems) == 0 {
		return errors.New("cold manifest omits filesystem graph roots")
	}
	_, err = b.materializeDistributedFilesystemRoots(info, branch.FilesystemBranch, coldFilesystemRoots(manifest), true)
	return err
}

func (b *runtimeBackend) filesystemRestoreIDs(container string) (map[string]string, error) {
	spec, err := b.containerStore().LoadSpec(container)
	if err != nil {
		return nil, err
	}
	repositories, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]string)
	for _, repository := range repositories {
		if repository.RestoreID == "" {
			continue
		}
		opened, err := ttcvolume.OpenRepository(repository.Path, ttcvolume.CopyMode(repository.CopyMode))
		if err != nil {
			return nil, fmt.Errorf("opening active repository %q: %w", repository.Source, err)
		}
		current := opened.Info().ID
		if err := opened.Close(); err != nil {
			return nil, fmt.Errorf("closing active repository %q: %w", repository.Source, err)
		}
		ids[current] = repository.RestoreID
	}
	return ids, nil
}

// prepareColdFilesystemRelays exposes shared packs through the same fenced
// 1 MiB range protocol used by cross-node forks. The donated client ends are
// owned by the Sentry after a successful restore.
func (b *runtimeBackend) prepareColdFilesystemRelays(info ttcbundle.ContainerInfo, branch ttcbundle.BranchInfo) (*coldFilesystemRelays, error) {
	if branch.StorageManifest == "" {
		return nil, nil
	}
	manifest, err := readColdBranchManifest(branch.StorageManifest)
	if err != nil {
		return nil, err
	}
	store := b.containerStore()
	spec, err := store.LoadSpec(info.ID)
	if err != nil {
		return nil, err
	}
	repositories, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		return nil, err
	}
	sort.Slice(repositories, func(i, j int) bool { return repositories[i].Source < repositories[j].Source })
	relays := &coldFilesystemRelays{}
	if _, err := rand.Read(relays.token[:]); err != nil {
		return nil, err
	}
	fail := func(err error) (*coldFilesystemRelays, error) {
		relays.close()
		return nil, err
	}
	if b.snapshotShared == nil {
		return fail(errors.New("shared snapshot graph store is unavailable"))
	}
	bySource := make(map[string]coldFilesystemSnapshot, len(manifest.Filesystems))
	for _, filesystem := range manifest.Filesystems {
		bySource[filesystem.Root.Source] = filesystem
	}
	seenPaths := make(map[string]struct{})
	for _, config := range repositories {
		path := filepath.Clean(config.Path)
		if _, duplicate := seenPaths[path]; duplicate {
			continue
		}
		seenPaths[path] = struct{}{}
		filesystem, ok := bySource[config.Source]
		if !ok {
			return fail(fmt.Errorf("cold manifest omits repository %q", config.Source))
		}
		client, done, err := startSharedFilesystemRelay(b.dataRoot, filesystem.Root.Root, config.Source, filesystem.Origins, relays.token)
		if err != nil {
			return fail(err)
		}
		relays.clients = append(relays.clients, client)
		relays.done = append(relays.done, done)
	}
	if manifest.MemorySnapshot != 0 {
		// Graph-native memory is resolved independently through the snapshot
		// origin resolver.
		return relays, nil
	}
	return fail(errors.New("new-format cold manifest omits graph-native memory snapshot"))
}

// prepareColdDirectFilesystemSources opens the process-free equivalent of
// prepareColdFilesystemRelays. Each manager-side repository resolves its lazy
// local placeholders from the committed shared pack chain, then serves its
// graph metadata and immutable files directly to a remote child. The returned
// cleanup owns every repository and upstream pack stream.
func (b *runtimeBackend) prepareColdDirectFilesystemSources(info ttcbundle.ContainerInfo, branch ttcbundle.BranchInfo, filesystemBranch string) ([]ttcvolume.RemoteObjectSource, func(), error) {
	if branch.StorageManifest == "" {
		return nil, nil, errors.New("dehydrated branch has no durable filesystem manifest")
	}
	manifest, err := readColdBranchManifest(branch.StorageManifest)
	if err != nil {
		return nil, nil, err
	}
	spec, err := b.containerStore().LoadSpec(info.ID)
	if err != nil {
		return nil, nil, err
	}
	configs, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(configs, func(i, j int) bool { return configs[i].Source < configs[j].Source })
	var repositories []*ttcvolume.Repository
	var clients []io.Closer
	cleanup := func() {
		for _, repository := range repositories {
			_ = repository.Close()
		}
		for _, client := range clients {
			_ = client.Close()
		}
	}
	fail := func(err error) ([]ttcvolume.RemoteObjectSource, func(), error) {
		cleanup()
		return nil, nil, err
	}
	sources := make([]ttcvolume.RemoteObjectSource, 0, len(configs))
	if b.snapshotShared == nil {
		return fail(errors.New("shared snapshot graph store is unavailable"))
	}
	bySource := make(map[string]coldFilesystemSnapshot, len(manifest.Filesystems))
	for _, filesystem := range manifest.Filesystems {
		bySource[filesystem.Root.Source] = filesystem
	}
	seenPaths := make(map[string]struct{})
	for _, config := range configs {
		path := filepath.Clean(config.Path)
		if _, duplicate := seenPaths[path]; duplicate {
			continue
		}
		seenPaths[path] = struct{}{}
		filesystem, ok := bySource[config.Source]
		if !ok {
			return fail(fmt.Errorf("cold manifest omits repository %q", config.Source))
		}
		repository, err := ttcvolume.OpenRepository(config.Path, ttcvolume.CopyMode(config.CopyMode))
		if err != nil {
			return fail(err)
		}
		physicalBranch := repositoryPhysicalBranch(config, filesystemBranch)
		var token ttcvolume.RemoteToken
		if _, err := rand.Read(token[:]); err != nil {
			repository.Close()
			return fail(err)
		}
		key := ttcvolume.RemoteSourceKey(sha256.Sum256([]byte(config.Source)))
		client, _, err := startSharedFilesystemRelay(b.dataRoot, filesystem.Root.Root, config.Source, filesystem.Origins, token)
		if err != nil {
			repository.Close()
			return fail(err)
		}
		if err := repository.AttachRemoteObjectSource(client, token, key, physicalBranch); err != nil {
			client.Close()
			repository.Close()
			return fail(err)
		}
		repositories = append(repositories, repository)
		clients = append(clients, client)
		sources = append(sources, ttcvolume.RemoteObjectSource{Key: key, Repository: repository})
	}
	if len(sources) == 0 {
		cleanup()
		return nil, nil, errors.New("dehydrated branch has no managed filesystem repositories")
	}
	return sources, cleanup, nil
}

// collectColdStorage traces every authoritative and pending manifest before
// reclaiming immutable packs. A grace period protects a publisher between
// writing its pack and atomically committing its manifest.
func collectColdStorage(sharedRoot, localRoot string, publications *ttcstorage.Queue, transactions cluster.TransactionCatalog) error {
	return collectColdStorageOlderThan(sharedRoot, localRoot, publications, transactions, time.Now().Add(-10*time.Minute))
}

// collectColdStorageOlderThan is the common tracer with an explicit physical
// publication grace. Periodic callers retain the ordinary ten-minute window
// that protects pack-before-manifest publication. The last clean cluster node
// may pass a future boundary after every node's publication queue has drained,
// because no unfenced publisher remains that could still name an unreferenced
// object.
func collectColdStorageOlderThan(sharedRoot, localRoot string, publications *ttcstorage.Queue, transactions cluster.TransactionCatalog, olderThan time.Time) error {
	store := ttcbundle.Store{Root: sharedRoot, ActiveRoot: localRoot}
	containers, err := store.ListContainers()
	if err != nil {
		return err
	}
	queue := make([]string, 0)
	for _, info := range containers {
		for _, branch := range info.Branches {
			if branch.StorageManifest != "" {
				queue = append(queue, branch.StorageManifest)
			}
		}
	}
	if publications != nil {
		pending, err := publications.Pending()
		if err != nil {
			return err
		}
		for _, intent := range pending {
			path := coldManifestPath(sharedRoot, intent.Container, intent.Branch, intent.PublicationID)
			if _, err := os.Stat(path); err == nil {
				queue = append(queue, path)
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	manifests := make(map[string]struct{})
	transactionsByID := make(map[string]cluster.Transaction)
	if transactions != nil {
		records, err := transactions.List()
		if err != nil {
			return err
		}
		for _, tx := range records {
			transactionsByID[tx.Identity.TransactionID] = tx
		}
	}
	for len(queue) != 0 {
		path := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if _, ok := manifests[path]; ok {
			continue
		}
		if err := validateColdManifestPath(sharedRoot, path); err != nil {
			return err
		}
		if _, err := readColdBranchManifest(path); err != nil {
			return fmt.Errorf("tracing cold manifest %q: %w", path, err)
		}
		manifests[path] = struct{}{}
	}
	// Graph handoff manifests are transaction-scoped restore descriptors, not
	// durable branch roots. Keep them through every nonterminal transaction
	// state (including the interval before Handoff binds its descriptor), then
	// reclaim them as soon as the transaction is terminal or retired. This is
	// also the crash-recovery path for a source that died after publishing the
	// target but before its completion monitor removed the manifest.
	memoryManifestRoot := filepath.Join(sharedRoot, "cluster", "memory-manifests")
	memoryManifests, err := os.ReadDir(memoryManifestRoot)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, entry := range memoryManifests {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".manifest" {
			continue
		}
		transactionID := strings.TrimSuffix(entry.Name(), ".manifest")
		if !strings.HasPrefix(transactionID, "fork-") || !transactionIDPatternForDaemon(transactionID) {
			// Unknown files are never interpreted as TTC state or allowed to stop
			// collection of recognized objects.
			continue
		}
		if tx, exists := transactionsByID[transactionID]; exists && !tx.Terminal() {
			continue
		}
		if err := os.Remove(filepath.Join(memoryManifestRoot, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	root := filepath.Join(sharedRoot, "cold", "manifests")
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if errors.Is(walkErr, os.ErrNotExist) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			return nil
		}
		if _, ok := manifests[path]; ok {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.ModTime().After(olderThan) {
			return nil
		}
		return os.Remove(path)
	}); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := pruneEmptyColdManifestDirectories(root); err != nil {
		return err
	}
	return nil
}

func pruneEmptyColdManifestDirectories(root string) error {
	var directories []string
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if errors.Is(walkErr, os.ErrNotExist) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && path != root {
			directories = append(directories, path)
		}
		return nil
	}); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	sort.Slice(directories, func(i, j int) bool {
		return strings.Count(directories[i], string(filepath.Separator)) > strings.Count(directories[j], string(filepath.Separator))
	})
	for _, directory := range directories {
		if err := os.Remove(directory); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) {
			return err
		}
	}
	return nil
}

// collectTerminalColdStorage is the best-effort terminal hook for asynchronous
// distributed transfers. Periodic maintenance remains the crash-recovery
// fallback, but the ordinary path must leave no unreachable packs merely
// because the source daemon shuts down before another timer tick.
func (b *runtimeBackend) collectTerminalColdStorage() {
	transactions, err := b.transactionCatalog()
	if err == nil {
		err = collectColdStorage(b.dataRoot, b.activeDataRoot(), b.publicationQueue, transactions)
	}
	if err != nil && b.stderr != nil {
		fmt.Fprintf(b.stderr, "ttcd: warning: collecting terminal distributed storage: %v\n", err)
	}
}
