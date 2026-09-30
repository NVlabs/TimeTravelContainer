// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	"gvisor.dev/gvisor/pkg/ttcvolume"
	"gvisor.dev/gvisor/runsc/boot"
	"gvisor.dev/gvisor/runsc/specutils"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
	"gvisor.dev/gvisor/runsc/ttc/cluster"
	ttcstorage "gvisor.dev/gvisor/runsc/ttc/storage"
)

type sharedSnapshotObjectReader struct {
	store *ttcsnapshot.SharedPackStore
	// view serves the reader's metadata.
	view           *ttcsnapshot.SharedPackView
	mu             sync.Mutex
	allowed        map[ttcsnapshot.ObjectID]struct{}
	allowedOrigins map[ttcsnapshot.SnapshotID]struct{}
	// unexpanded holds the allowed objects whose children are not allowed
	// yet.
	unexpanded []ttcsnapshot.ObjectID
}

func newSharedSnapshotObjectReader(store *ttcsnapshot.SharedPackStore, root string) (*sharedSnapshotObjectReader, error) {
	id, err := ttcsnapshot.ParseObjectID(root)
	if err != nil {
		return nil, err
	}
	return &sharedSnapshotObjectReader{store: store, view: store.View(), allowed: map[ttcsnapshot.ObjectID]struct{}{id: {}}, unexpanded: []ttcsnapshot.ObjectID{id}}, nil
}

// authorize reports whether id is in the tree the reader serves: the root,
// an object of an origin the tree's publication made, or an object below
// another it allows.
func (r *sharedSnapshotObjectReader) authorize(id ttcsnapshot.ObjectID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.allowed[id]; ok {
		return true
	}
	if _, ok := r.allowedOrigins[id.Snapshot]; ok {
		return true
	}
	// A repository that expanded part of the tree through an earlier relay,
	// as a branch rehydrated on a node before does, resumes below objects
	// this relay has yet to serve. Look for id below the allowed objects,
	// once each.
	for len(r.unexpanded) != 0 {
		next := r.unexpanded[len(r.unexpanded)-1]
		r.unexpanded = r.unexpanded[:len(r.unexpanded)-1]
		object, err := r.view.Object(next)
		if err != nil {
			continue
		}
		r.allowLocked(object)
		if _, ok := r.allowed[id]; ok {
			return true
		}
	}
	return false
}

// allowLocked allows the objects below object.
//
// +checklocks:r.mu
func (r *sharedSnapshotObjectReader) allowLocked(object ttcsnapshot.Object) {
	allow := func(id ttcsnapshot.ObjectID) {
		if _, ok := r.allowed[id]; !ok {
			r.allowed[id] = struct{}{}
			r.unexpanded = append(r.unexpanded, id)
		}
	}
	for _, edge := range object.Children {
		allow(edge.Child)
	}
	if object.Deferred && !object.SourceObject.IsZero() {
		allow(object.SourceObject)
	}
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
	object, err := r.view.Object(id)
	if err != nil {
		return ttcsnapshot.Object{}, err
	}
	r.mu.Lock()
	r.allowLocked(object)
	r.mu.Unlock()
	return object, nil
}

// newSharedFilesystemSource returns the source a shared filesystem relay
// serves, for the process to read itself: the tree below root in store, and
// the origins its publication made.
func newSharedFilesystemSource(store *ttcsnapshot.SharedPackStore, root, sourceName string, origins []uint64) (ttcvolume.RemoteObjectSource, error) {
	if store == nil {
		return ttcvolume.RemoteObjectSource{}, errors.New("shared snapshot packs are unavailable")
	}
	reader, err := newSharedSnapshotObjectReader(store, root)
	if err != nil {
		return ttcvolume.RemoteObjectSource{}, err
	}
	reader.allowedOrigins = make(map[ttcsnapshot.SnapshotID]struct{}, len(origins))
	for _, origin := range origins {
		if origin == 0 {
			return ttcvolume.RemoteObjectSource{}, errors.New("shared filesystem source contains zero origin")
		}
		reader.allowedOrigins[ttcsnapshot.SnapshotID(origin)] = struct{}{}
	}
	key := ttcvolume.RemoteSourceKey(sha256.Sum256([]byte(sourceName)))
	return ttcvolume.RemoteObjectSource{Key: key, Reader: reader, Catalog: reader}, nil
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
	// Spare marks a chunk the node's store holds, asked for only if the
	// store loses it: the packs need not hold it yet when the helper starts.
	Spare bool `json:"spare,omitempty"`
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
	requested := make([]ttcsnapshot.ObjectID, 0, len(catalog.Objects))
	expected := make(map[ttcsnapshot.ObjectID]uint64, len(catalog.Objects))
	for _, encoded := range catalog.Objects {
		id, err := ttcsnapshot.ParseObjectID(encoded.ID)
		if err != nil || encoded.Length == 0 || encoded.Length > ttcsnapshot.MemoryChunkBytes {
			fmt.Fprintln(stderr, "ttcd shared memory relay: invalid catalog object")
			return 2
		}
		if _, duplicate := expected[id]; duplicate {
			fmt.Fprintln(stderr, "ttcd shared memory relay: duplicate catalog object")
			return 2
		}
		if !encoded.Spare {
			requested = append(requested, id)
		}
		expected[id] = encoded.Length
	}
	objects, err := store.Objects(requested)
	if err != nil {
		fmt.Fprintln(stderr, "ttcd shared memory relay: catalog object is unavailable")
		return 1
	}
	for id, object := range objects {
		if object.BulkLength != expected[id] {
			fmt.Fprintln(stderr, "ttcd shared memory relay: catalog object is unavailable")
			return 1
		}
	}
	connection := os.NewFile(3, "ttcd-shared-memory-relay")
	if connection == nil {
		fmt.Fprintln(stderr, "ttcd shared memory relay: inherited connection is unavailable")
		return 2
	}
	err = ttcsnapshot.ServeObjectRelay(connection, token, store, expected)
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) {
		return 0
	}
	fmt.Fprintf(stderr, "ttcd shared memory relay: %v\n", err)
	return 1
}

func startSharedMemoryRelay(sharedRoot string, objects map[ttcsnapshot.ObjectID]ttcsnapshot.Object, spare map[ttcsnapshot.ObjectID]bool, token ttcsnapshot.ObjectRelayToken) (*os.File, <-chan error, error) {
	if len(objects) == 0 {
		return nil, nil, errors.New("shared memory relay has no immutable objects")
	}
	catalog := sharedMemoryRelayCatalog{Version: sharedMemoryRelayCatalogVersion, Objects: make([]sharedMemoryRelayObject, 0, len(objects))}
	for id, object := range objects {
		if id.IsZero() || object.ID != id || object.BulkLength == 0 || object.BulkLength > ttcsnapshot.MemoryChunkBytes {
			return nil, nil, fmt.Errorf("invalid shared memory relay object %s", id)
		}
		catalog.Objects = append(catalog.Objects, sharedMemoryRelayObject{ID: id.String(), Length: object.BulkLength, Spare: spare[id]})
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
	fds, err := cloexecStreamSocketpair()
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
	if reader.allowedOrigins, err = parseSharedRelayOriginScope(args[4]); err != nil {
		fmt.Fprintf(stderr, "ttcd shared filesystem relay: %v\n", err)
		return 2
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

// parseSharedRelayOriginScope decodes a shared filesystem relay's origin
// scope, the origins whose objects it serves without finding them below the
// root: those the tree's publication handed off. A publication that changed
// nothing of the tree handed off none, and its relay finds every object
// below the root.
func parseSharedRelayOriginScope(encoded string) (map[ttcsnapshot.SnapshotID]struct{}, error) {
	origins := make(map[ttcsnapshot.SnapshotID]struct{})
	if encoded == "" {
		return origins, nil
	}
	for _, field := range strings.Split(encoded, ",") {
		origin, err := strconv.ParseUint(field, 16, 64)
		if err != nil || origin == 0 {
			return nil, errors.New("invalid origin scope")
		}
		origins[ttcsnapshot.SnapshotID(origin)] = struct{}{}
	}
	return origins, nil
}

func startSharedFilesystemRelay(sharedRoot, root, sourceName string, origins []uint64, token ttcvolume.RemoteToken) (*os.File, <-chan error, error) {
	fds, err := cloexecStreamSocketpair()
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
	MemorySnapshot uint64                            `json:"memory_snapshot,omitempty"`
	ControlRoot    string                            `json:"control_root,omitempty"`
	Filesystems    []coldFilesystemSnapshot          `json:"filesystems,omitempty"`
	InternalMounts []cluster.InternalMountDescriptor `json:"internal_mounts,omitempty"`
	// ExecConsoleLogs indexes immutable retained-output byte ranges in the
	// publication's adjacent .console.pack file. Console output is runtime
	// state, not guest filesystem state; publishing it here keeps named exec
	// sessions reattachable when a cold branch is claimed on another node.
	ExecConsoleLogs []coldExecConsoleLog `json:"exec_console_logs,omitempty"`
	ConsolePackSize uint64               `json:"console_pack_size,omitempty"`
}

type coldExecConsoleLog struct {
	ExecID string `json:"exec_id"`
	Offset uint64 `json:"offset"`
	Length uint64 `json:"length"`
}

type branchPublisher struct {
	sharedRoot   string
	localRoot    string
	nodeID       string
	incarnation  string
	placements   cluster.PlacementCatalog
	transactions cluster.TransactionCatalog
	authority    *cluster.AuthorityCatalog
	graph        *ttcsnapshot.Store
	graphData    *ttcsnapshot.LocalDataStore
	graphShared  *ttcsnapshot.SharedPackStore
	sharedRefs   *sharedReferenceService
	peers        *distributedSourceRuntime
	event        func(DaemonEvent)
}

func publicationAuthorityIdentity(info ttcbundle.ContainerInfo, intent ttcstorage.Intent) (string, string) {
	if intent.AuthorityContainer != "" && intent.AuthorityBranch != "" {
		return intent.AuthorityContainer, intent.AuthorityBranch
	}
	return logicalFamilyID(info), intent.Branch
}

// FenceCancellation orders cancellation against the publication's one
// irreversible authority command before waiting for data-plane cleanup.
func (p *branchPublisher) FenceCancellation(intent ttcstorage.Intent) error {
	if p.authority == nil {
		return nil
	}
	store := ttcbundle.Store{Root: p.sharedRoot, ActiveRoot: p.localRoot}
	info, err := store.LoadContainer(intent.Container)
	if err != nil {
		return err
	}
	family, branch := publicationAuthorityIdentity(info, intent)
	return p.authority.CancelPublication(family, branch, intent.PublicationID)
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
		return ttcstorage.Result{}, fmt.Errorf("%w: branch now names %q", ttcstorage.ErrPublicationSuperseded, branch.PublicationID)
	}
	path := coldManifestPath(p.sharedRoot, info.ID, branch.ID, intent.PublicationID)
	if existing, err := readColdBranchManifest(path); err == nil {
		if existing.Container != info.ID || existing.Branch != branch.ID || existing.PublicationID != intent.PublicationID || existing.Kind != intent.Kind {
			return ttcstorage.Result{}, errors.New("existing cold manifest has a conflicting identity")
		}
		for _, filesystem := range existing.Filesystems {
			// A tree that changed nothing of the image it was restored from
			// publishes no origin of its own.
			if filesystem.Root.Root == "" {
				return ttcstorage.Result{}, errors.New("existing cold manifest has an incomplete filesystem snapshot")
			}
			for _, origin := range filesystem.Origins {
				if err := p.graphShared.CheckManifest(ttcsnapshot.SnapshotID(origin)); err != nil {
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
	internalMounts, err := store.PortableInternalMounts(info.ID)
	if err != nil {
		return ttcstorage.Result{}, fmt.Errorf("capturing portable internal mounts: %w", err)
	}
	manifest.InternalMounts = make([]cluster.InternalMountDescriptor, len(internalMounts))
	for i, mount := range internalMounts {
		manifest.InternalMounts[i] = cluster.InternalMountDescriptor{
			Destination: mount.Destination,
			Contents:    append([]byte(nil), mount.Contents...),
			Mode:        uint32(mount.Mode.Perm()),
		}
	}
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
		if prior, ok := priorFilesystems[config.Source]; ok {
			// The publisher reads the shared packs itself: what a runtime reaches
			// through a relay process, it reads in process, as the relay would.
			source, err := newSharedFilesystemSource(p.graphShared, prior.Root.Root, config.Source, prior.Origins)
			if err == nil {
				err = repository.AttachLocalObjectSource(source, physicalBranch)
			}
			if err != nil {
				repository.Close()
				return ttcstorage.Result{}, fmt.Errorf("attaching shared filesystem resolver for %q: %w", config.Source, err)
			}
		}
		// What the branch reaches of a shared image unchanged, the publication
		// names by the image's own identities, and does not copy.
		descriptor, snapshotErr := repository.DescribePublishedSnapshot(physicalBranch)
		origins, originsErr := repository.PrepareBranchPublication(physicalBranch)
		snapshotErr = errors.Join(snapshotErr, originsErr)
		for _, origin := range origins {
			if snapshotErr == nil {
				snapshotErr = repository.PublishOriginHandoffByReference(context.Background(), p.graphShared, origin)
			}
		}
		closeErr := repository.Close()
		if snapshotErr != nil || closeErr != nil {
			return ttcstorage.Result{}, errors.Join(snapshotErr, closeErr)
		}
		encodedOrigins := make([]uint64, len(origins))
		for i, origin := range origins {
			encodedOrigins[i] = uint64(origin)
		}
		manifest.Filesystems = append(manifest.Filesystems, coldFilesystemSnapshot{
			Root: cluster.FilesystemRootDescriptor{
				Source: config.Source, RepositoryID: descriptor.Snapshot.RepositoryID,
				// The captured Sentry has already canonicalized this filesystem
				// to its node-local repository. Persist that exact identity;
				// config.RestoreID may still describe an earlier import hop.
				RestoreID: descriptor.Snapshot.RepositoryID,
				BranchID:  descriptor.Snapshot.BranchID, Root: descriptor.Root.String(), BaseRoot: descriptor.BaseRootString(),
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
	activeRoot := p.localRoot
	if activeRoot == "" {
		activeRoot = p.sharedRoot
	}
	manifest.ExecConsoleLogs, manifest.ConsolePackSize, err = captureColdExecConsolePack(activeRoot, info, branch.ID, path)
	if err != nil {
		return ttcstorage.Result{}, fmt.Errorf("capturing retained exec output: %w", err)
	}
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

func coldConsolePackPath(manifestPath string) string {
	return strings.TrimSuffix(manifestPath, filepath.Ext(manifestPath)) + ".console.pack"
}

func validColdExecID(value string) bool {
	if value == "" || len(value) > 4096 {
		return false
	}
	for index, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || (index > 0 && strings.ContainsRune("_.-", r)) {
			continue
		}
		return false
	}
	return true
}

func coldExecConsoleDir(activeRoot, container, branch string) string {
	return filepath.Join(activeRoot, "containers", container, "console", "exec", branch)
}

// captureColdExecConsolePack serializes the retained output streams while
// their followers are stopped. Generations are concatenated oldest-first so
// the pack preserves exactly the byte stream observed by `ttc logs`.
func captureColdExecConsolePack(activeRoot string, info ttcbundle.ContainerInfo, branch, manifestPath string) ([]coldExecConsoleLog, uint64, error) {
	directory := coldExecConsoleDir(activeRoot, info.ID, branch)
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(coldConsolePackPath(manifestPath))
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("listing retained exec output: %w", err)
	}
	maxFiles := info.ConsoleLogMaxFiles
	if maxFiles < 1 {
		maxFiles = defaultConsoleLogFiles
	}
	var execIDs []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".log") || strings.HasSuffix(name, ".error.log") {
			continue
		}
		execID := strings.TrimSuffix(name, ".log")
		if validColdExecID(execID) {
			execIDs = append(execIDs, execID)
		}
	}
	sort.Strings(execIDs)
	packPath := coldConsolePackPath(manifestPath)
	if err := os.MkdirAll(filepath.Dir(packPath), 0700); err != nil {
		return nil, 0, err
	}
	temporary, err := os.CreateTemp(filepath.Dir(packPath), "."+filepath.Base(packPath)+".tmp-")
	if err != nil {
		return nil, 0, err
	}
	temporaryPath := temporary.Name()
	keep := false
	defer func() {
		_ = temporary.Close()
		if !keep {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0600); err != nil {
		return nil, 0, err
	}
	var descriptors []coldExecConsoleLog
	var offset uint64
	for _, execID := range execIDs {
		generations, err := listConsoleLogGenerations(filepath.Join(directory, execID+".log"), maxFiles)
		if err != nil {
			return nil, 0, fmt.Errorf("listing retained output for exec session %q: %w", execID, err)
		}
		start := offset
		for _, generation := range generations {
			if generation.info.Size() < 0 || uint64(generation.info.Size()) > ^uint64(0)-offset {
				return nil, 0, fmt.Errorf("retained output for exec session %q is too large", execID)
			}
			input, err := os.Open(generation.path)
			if err != nil {
				return nil, 0, err
			}
			written, copyErr := io.Copy(temporary, input)
			closeErr := input.Close()
			if copyErr != nil || closeErr != nil {
				return nil, 0, errors.Join(copyErr, closeErr)
			}
			if written != generation.info.Size() {
				return nil, 0, fmt.Errorf("retained output for exec session %q changed during capture", execID)
			}
			offset += uint64(written)
		}
		if offset != start {
			descriptors = append(descriptors, coldExecConsoleLog{ExecID: execID, Offset: start, Length: offset - start})
		}
	}
	if offset == 0 {
		if err := temporary.Close(); err != nil {
			return nil, 0, err
		}
		_ = os.Remove(temporaryPath)
		_ = os.Remove(packPath)
		keep = true
		return nil, 0, nil
	}
	if err := temporary.Sync(); err != nil {
		return nil, 0, err
	}
	if err := temporary.Close(); err != nil {
		return nil, 0, err
	}
	if err := os.Rename(temporaryPath, packPath); err != nil {
		return nil, 0, err
	}
	directoryFile, err := os.Open(filepath.Dir(packPath))
	if err != nil {
		return nil, 0, err
	}
	syncErr := directoryFile.Sync()
	closeErr := directoryFile.Close()
	if syncErr != nil || closeErr != nil {
		return nil, 0, errors.Join(syncErr, closeErr)
	}
	keep = true
	return descriptors, offset, nil
}

// installColdExecConsolePack restores retained output before console followers
// are restarted. Existing node-local bytes can be an older prefix or segment
// from a previous residence, so use the same idempotent merge as migration.
func installColdExecConsolePack(activeRoot string, info ttcbundle.ContainerInfo, branch string, manifest coldBranchManifest, manifestPath string) error {
	if len(manifest.ExecConsoleLogs) == 0 {
		return nil
	}
	pack, err := os.Open(coldConsolePackPath(manifestPath))
	if err != nil {
		return fmt.Errorf("opening retained exec output pack: %w", err)
	}
	defer pack.Close()
	directory := coldExecConsoleDir(activeRoot, info.ID, branch)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	for _, descriptor := range manifest.ExecConsoleLogs {
		destination := filepath.Join(directory, descriptor.ExecID+".log")
		temporary := destination + ".cold-partial"
		_ = os.Remove(temporary)
		output, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		reader := io.NewSectionReader(pack, int64(descriptor.Offset), int64(descriptor.Length))
		written, copyErr := io.Copy(output, reader)
		syncErr := output.Sync()
		closeErr := output.Close()
		if copyErr != nil || syncErr != nil || closeErr != nil || uint64(written) != descriptor.Length {
			_ = os.Remove(temporary)
			return errors.Join(copyErr, syncErr, closeErr, func() error {
				if uint64(written) != descriptor.Length {
					return io.ErrUnexpectedEOF
				}
				return nil
			}())
		}
		if err := reconcileRemoteConsoleLog(destination, temporary); err != nil {
			_ = os.Remove(temporary)
			return fmt.Errorf("installing retained output for exec session %q: %w", descriptor.ExecID, err)
		}
	}
	return nil
}

func (p *branchPublisher) CaptureAvailable(intent ttcstorage.Intent) (bool, error) {
	store := ttcbundle.Store{Root: p.sharedRoot, ActiveRoot: p.localRoot}
	info, err := store.LoadContainer(intent.Container)
	if err != nil {
		return false, err
	}
	branch, err := persistentBranchInfo(info, intent.Branch)
	if err != nil {
		return false, err
	}
	// An enqueued capture is deliberately invisible while its hidden roots are
	// being built. Do not cancel the predecessor or authorize this transaction
	// until BeginBranchPublicationIf has installed the complete capture.
	if branch.PublicationID != intent.PublicationID {
		return false, nil
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
				if err := p.graphShared.CheckManifest(snapshot); err != nil {
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
		family, authorityBranch := publicationAuthorityIdentity(info, intent)
		if intent.SupersedesPublicationID != "" && intent.SupersedesPublicationID != intent.PublicationID {
			if err := p.authority.CancelPublication(family, authorityBranch, intent.SupersedesPublicationID); err != nil {
				return false, fmt.Errorf("fencing superseded publication in the authority: %w", err)
			}
		}
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
		if _, err := p.authority.EnsurePublicationReady(family, authorityBranch, intent.PublicationID, lifecycle, captureRoots); err != nil {
			return false, fmt.Errorf("authorizing publication capture in the authority: %w", err)
		}
	}
	return true, nil
}

func (p *branchPublisher) Cleanup(intent ttcstorage.Intent) error {
	store := ttcbundle.Store{Root: p.sharedRoot, ActiveRoot: p.localRoot}
	info, err := store.LoadContainer(intent.Container)
	if err != nil {
		return err
	}
	family, authorityBranch := publicationAuthorityIdentity(info, intent)
	canceled := p.authority != nil
	if p.authority != nil {
		if err := p.authority.CancelPublication(family, authorityBranch, intent.PublicationID); err != nil {
			return fmt.Errorf("canceling publication authority: %w", err)
		}
		record, ok, err := p.authority.Publication(family, authorityBranch, intent.PublicationID)
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
			// Cancellation is also the recovery path for a capture whose local
			// active tier was lost before this daemon learned about the pending
			// intent. In that case there is no branch or local reference ledger
			// left to mutate. Treat every origin named by the immutable manifest
			// as locally empty so the authority/pack cleanup below can retire it.
			// Do not absorb malformed or unreadable repositories: only a path
			// whose absence is independently confirmed has this meaning.
			if errors.Is(err, os.ErrNotExist) {
				if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
					for _, encodedOrigin := range filesystemOrigins[config.Source] {
						emptyFilesystemOrigins[encodedOrigin] = true
					}
					continue
				} else if statErr != nil {
					return fmt.Errorf("stating canceled publication repository %q: %w", config.Source, statErr)
				}
			}
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
		if err := os.Remove(coldConsolePackPath(manifestPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
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
// remote consumers once the authority routes the origin to its committed shared pack.
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
	for name, object := range graph.InspectWithoutObjects().Roots {
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

// publishGraphDependencies makes every older origin reached directly by an
// immutable origin durable before the parent can become a durable root. The
// graph is a DAG, so recursively publishing children first makes lookup
// independent of ancestry depth without putting page/file identities in
// the authority. Remote owners perform their own recursive publication through the
// authenticated provider endpoint.
func (p *branchPublisher) publishGraphDependencies(ctx context.Context, snapshot ttcsnapshot.SnapshotID) error {
	if p == nil || p.graph == nil || p.authority == nil || p.graphShared == nil || snapshot == 0 {
		return errors.New("distributed graph dependency publication is unavailable")
	}
	objects, err := p.graph.SnapshotObjects(snapshot)
	if err != nil {
		// A concurrent idempotent publisher may have completed and reclaimed the
		// provider metadata after the initial authority read.
		if _, volatile, authorityErr := p.authority.SnapshotProvider(uint64(snapshot)); authorityErr == nil && !volatile {
			manifestErr := p.graphShared.CheckManifest(snapshot)
			return manifestErr
		}
		return err
	}
	dependencies := make(map[ttcsnapshot.SnapshotID]struct{})
	for _, object := range objects {
		seen := make(map[ttcsnapshot.ObjectID]struct{})
		for object.Deferred {
			if object.SourceObject.IsZero() {
				return fmt.Errorf("deferred object %s has no source identity", object.ID)
			}
			if _, duplicate := seen[object.SourceObject]; duplicate {
				return fmt.Errorf("deferred object %s has a source cycle through %s", object.ID, object.SourceObject)
			}
			seen[object.SourceObject] = struct{}{}
			if local, ok := p.graph.Object(object.SourceObject); ok {
				object = local
				continue
			}
			shared, sharedErr := p.graphShared.Object(object.SourceObject)
			if sharedErr != nil {
				return fmt.Errorf("resolving deferred source %s for object %s: %w", object.SourceObject, object.ID, sharedErr)
			}
			object = shared
		}
		for _, edge := range object.Children {
			if edge.Child.Snapshot != snapshot {
				dependencies[edge.Child.Snapshot] = struct{}{}
			}
		}
	}
	ordered := make([]ttcsnapshot.SnapshotID, 0, len(dependencies))
	for dependency := range dependencies {
		ordered = append(ordered, dependency)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	for _, dependency := range ordered {
		if err := p.ensureGraphOriginShared(ctx, dependency); err != nil {
			return fmt.Errorf("publishing backing snapshot origin %d for origin %d: %w", dependency, snapshot, err)
		}
	}
	return nil
}

func (p *branchPublisher) ensureGraphOriginShared(ctx context.Context, snapshot ttcsnapshot.SnapshotID) error {
	provider, volatile, err := p.authority.SnapshotProvider(uint64(snapshot))
	if err != nil {
		return err
	}
	if !volatile {
		if err := p.graphShared.CheckManifest(snapshot); err != nil {
			return fmt.Errorf("snapshot origin %d has shared authority without a manifest: %w", snapshot, err)
		}
		return nil
	}
	if provider.NodeID == p.nodeID {
		return p.publishVolatileGraphOrigin(ctx, snapshot)
	}
	if p.peers == nil {
		return fmt.Errorf("snapshot origin %d belongs to remote node %q but peer routing is unavailable", snapshot, provider.NodeID)
	}
	peer, err := p.peers.peer(provider.NodeID, false)
	if err != nil {
		return err
	}
	if err := peer.PublishSnapshot(ctx, uint64(snapshot)); err != nil {
		return fmt.Errorf("requesting publication from node %q: %w", provider.NodeID, err)
	}
	if _, volatile, err := p.authority.SnapshotProvider(uint64(snapshot)); err != nil {
		return err
	} else if volatile {
		return fmt.Errorf("node %q returned before snapshot origin %d became shared", provider.NodeID, snapshot)
	}
	if err := p.graphShared.CheckManifest(snapshot); err != nil {
		return fmt.Errorf("opening remotely published snapshot origin %d: %w", snapshot, err)
	}
	return nil
}

func (p *branchPublisher) publishVolatileGraphOrigin(ctx context.Context, snapshot ttcsnapshot.SnapshotID) (retErr error) {
	if p == nil || snapshot == 0 || p.graph == nil || p.graphData == nil || p.graphShared == nil ||
		p.authority == nil || p.sharedRefs == nil || p.peers == nil {
		return errors.New("distributed graph publication is unavailable")
	}
	provider, volatile, err := p.authority.SnapshotProvider(uint64(snapshot))
	if err != nil {
		return err
	}
	if !volatile {
		if err := p.graphShared.CheckManifest(snapshot); err != nil {
			return fmt.Errorf("snapshot origin %d has neither volatile nor shared authority: %w", snapshot, err)
		}
		return nil
	}
	if provider.NodeID != p.nodeID {
		return fmt.Errorf("snapshot origin %d belongs to node %q, not %q", snapshot, provider.NodeID, p.nodeID)
	}
	// Do not hold an origin lock while recursively publishing dependencies: a
	// bounded lock stripe may legitimately be shared by parent and child.
	if err := p.publishGraphDependencies(ctx, snapshot); err != nil {
		return err
	}
	unlock := p.peers.lockSnapshotOrigin(snapshot)
	defer unlock()
	provider, volatile, err = p.authority.SnapshotProvider(uint64(snapshot))
	if err != nil {
		return err
	}
	if !volatile {
		if err := p.graphShared.CheckManifest(snapshot); err != nil {
			return fmt.Errorf("snapshot origin %d has shared authority without a manifest: %w", snapshot, err)
		}
		return nil
	}
	if provider.NodeID != p.nodeID {
		return fmt.Errorf("snapshot origin %d provider changed to node %q", snapshot, provider.NodeID)
	}
	record, ok := p.graph.Snapshot(snapshot)
	if !ok || record.State != ttcsnapshot.SnapshotSealedVolatile {
		return fmt.Errorf("snapshot origin %d is not locally sealed for publication", snapshot)
	}
	if err := p.graphShared.PublishHandoff(ctx, p.graph, snapshot, p.graphData); err != nil {
		return fmt.Errorf("publishing snapshot origin %d: %w", snapshot, err)
	}

	finishHandoff := p.peers.beginFinalSnapshotSwitch()
	defer finishHandoff()
	ledgerFrozen := false
	defer func() {
		if retErr != nil && ledgerFrozen {
			p.rollForwardFrozenOrigin(snapshot)
		}
	}()
	if err := p.graph.FreezeSnapshotLedgerHandoff(snapshot); err != nil {
		return err
	}
	ledgerFrozen = true
	changes, _, frozen, err := p.graph.SnapshotLedgerHandoffChanges(snapshot, 0)
	if err != nil {
		return err
	}
	if !frozen {
		return fmt.Errorf("snapshot origin %d reference handoff did not freeze", snapshot)
	}
	bridgeIntent := sharedPublicationBridgeIntent{Kind: sharedPublicationBridgeOrigin}
	if err := retryGraphReferenceMutation(func(context.Context) error {
		return p.publishSharedReferenceLedger(snapshot, changes, bridgeIntent)
	}); err != nil {
		return fmt.Errorf("transferring snapshot origin %d reference authority: %w", snapshot, err)
	}
	references, _ := p.graph.OriginReferences(snapshot)
	if err := p.keepProviderReferences(snapshot, "", references); err != nil {
		return err
	}
	if err := p.authority.PublishSnapshotOrigin(p.nodeID, p.incarnation, uint64(snapshot)); err != nil {
		return fmt.Errorf("publishing snapshot origin %d authority: %w", snapshot, err)
	}
	if err := p.graph.MarkShared(snapshot); err != nil {
		return err
	}
	if err := p.graph.UnfreezeSnapshotLedger(snapshot); err != nil {
		return err
	}
	ledgerFrozen = false
	if err := retryGraphReferenceMutation(func(context.Context) error {
		return p.releaseSharedPublicationBridge(snapshot)
	}); err != nil {
		return fmt.Errorf("releasing snapshot origin %d publication bridge: %w", snapshot, err)
	}
	return releasePublishedProviderGraphRoots(p.graph, p.graphData, snapshot)
}

// BeginCompletion takes, for Complete, the snapshot handoff gate: the final
// provider-to-shared ledger switch freezes reference changes, which live and
// dehydration captures, creating new DAG edges to these immutable origins,
// must not make meanwhile. Slow pack construction happened before and does not
// hold the gate. The queue takes it before its own lock: a capture keeps the
// gate while it waits for the lifecycle lock, whose holder may wait for the
// queue, as a removal canceling the branch's publications does; and that
// removal waits, holding the lifecycle lock, for the attempt waiting for the
// gate, which so gives up once abandon reports the publication canceled.
func (p *branchPublisher) BeginCompletion(abandon func() bool) (func(), bool) {
	if p.authority == nil {
		return func() {}, true
	}
	return p.peers.beginFinalSnapshotSwitchUnless(abandon)
}

// rollForwardFrozenOrigin completes, in the background, the handoff of
// snapshot, whose reference ledger a failed or interrupted handoff left
// frozen, retrying until it completes. The handoff cannot be canceled: its
// pack's manifest, which no retry replaces, holds the baseline that its
// recorded mutations follow, and a new handoff would freeze another. At most
// one roll-forward runs for an origin.
func (p *branchPublisher) rollForwardFrozenOrigin(snapshot ttcsnapshot.SnapshotID) {
	if p == nil || p.peers == nil || p.graph == nil || snapshot == 0 {
		return
	}
	if _, running := p.peers.rollingForward.LoadOrStore(snapshot, struct{}{}); running {
		return
	}
	go func() {
		defer p.peers.rollingForward.Delete(snapshot)
		for delay := time.Second; p.graph.SnapshotLedgerFrozen(snapshot); delay = min(2*delay, 30*time.Second) {
			err := p.resumeFrozenGraphOrigin(context.Background(), snapshot)
			if err == nil || errors.Is(err, errFrozenOriginOwned) {
				return
			}
			fmt.Fprintf(os.Stderr, "ttcd: completing the frozen handoff of snapshot origin %d: %v\n", snapshot, err)
			time.Sleep(delay)
		}
	}()
}

// errFrozenOriginOwned reports a frozen origin whose handoff belongs to a
// branch publication, which the publication queue resumes.
var errFrozenOriginOwned = errors.New("snapshot origin handoff belongs to a branch publication")

// resumeFrozenGraphOrigin rolls forward the handoff of snapshot, an origin of
// this node's that a crash left frozen at its final barrier. The shared
// coordinator may already hold the handoff's baseline, so the handoff cannot be
// canceled; and until it completes, the freeze refuses every new reference to
// the origin, as a fork or a graph handoff's capture makes. Every step is
// idempotent, and one that fails leaves the freeze for the next attempt.
func (p *branchPublisher) resumeFrozenGraphOrigin(ctx context.Context, snapshot ttcsnapshot.SnapshotID) error {
	if p == nil || snapshot == 0 || p.graph == nil || p.graphShared == nil || p.authority == nil ||
		p.sharedRefs == nil || p.peers == nil {
		return errors.New("distributed graph publication is unavailable")
	}
	unlock := p.peers.lockSnapshotOrigin(snapshot)
	defer unlock()
	if !p.graph.SnapshotLedgerFrozen(snapshot) {
		return nil
	}
	// Only the exact retry of the operation that recorded a bridge may reuse
	// it. Without one recorded yet, the handoff proceeds as the origin's own.
	bridgeIntent, found, err := loadSharedPublicationBridgeIntent(p.sharedRoot, snapshot)
	if err != nil {
		return err
	}
	if !found {
		bridgeIntent = sharedPublicationBridgeIntent{Kind: sharedPublicationBridgeOrigin}
	} else if bridgeIntent.Kind == sharedPublicationBridgePublication {
		return errFrozenOriginOwned
	}
	provider, volatile, err := p.authority.SnapshotProvider(uint64(snapshot))
	if errors.Is(err, os.ErrNotExist) {
		// The origin was abandoned or retired: no handoff of it completes, and
		// no shared ledger counts its references any more.
		return p.graph.UnfreezeSnapshotLedger(snapshot)
	}
	if err != nil {
		return err
	}
	if volatile && provider.NodeID != p.nodeID {
		return fmt.Errorf("snapshot origin %d is frozen here but provided by node %q", snapshot, provider.NodeID)
	}
	if err := p.graphShared.CheckManifest(snapshot); err != nil {
		return fmt.Errorf("reading frozen snapshot origin %d manifest: %w", snapshot, err)
	}
	finishHandoff := p.peers.beginFinalSnapshotSwitch()
	defer finishHandoff()
	if volatile {
		changes, _, frozen, err := p.graph.SnapshotLedgerHandoffChanges(snapshot, 0)
		if err != nil {
			return err
		}
		if !frozen {
			return fmt.Errorf("snapshot origin %d reference handoff did not freeze", snapshot)
		}
		if err := retryGraphReferenceMutation(func(context.Context) error {
			return p.publishSharedReferenceLedger(snapshot, changes, bridgeIntent)
		}); err != nil {
			return fmt.Errorf("transferring snapshot origin %d reference authority: %w", snapshot, err)
		}
		references, _ := p.graph.OriginReferences(snapshot)
		if err := p.keepProviderReferences(snapshot, "", references); err != nil {
			return err
		}
		if err := p.authority.PublishSnapshotOrigin(p.nodeID, p.incarnation, uint64(snapshot)); err != nil {
			return fmt.Errorf("publishing snapshot origin %d authority: %w", snapshot, err)
		}
	}
	if record, ok := p.graph.Snapshot(snapshot); ok && record.State == ttcsnapshot.SnapshotSealedVolatile {
		if err := p.graph.MarkShared(snapshot); err != nil {
			return err
		}
	}
	if err := p.graph.UnfreezeSnapshotLedger(snapshot); err != nil {
		return err
	}
	// A graph handoff's bridge lasts until its transaction ends, when the
	// shared coordinator releases it. The origin's own ends here.
	if bridgeIntent.Kind == sharedPublicationBridgeOrigin {
		if err := retryGraphReferenceMutation(func(context.Context) error {
			return p.releaseSharedPublicationBridge(snapshot)
		}); err != nil {
			return fmt.Errorf("releasing snapshot origin %d publication bridge: %w", snapshot, err)
		}
	}
	return releasePublishedProviderGraphRoots(p.graph, p.graphData, snapshot)
}

func (p *branchPublisher) Complete(intent ttcstorage.Intent, result ttcstorage.Result) error {
	store := ttcbundle.Store{Root: p.sharedRoot, ActiveRoot: p.localRoot}
	info, err := store.LoadContainer(intent.Container)
	if err != nil {
		return err
	}
	lifecycle := "exited"
	if intent.Kind == "dehydrate" {
		lifecycle = "dehydrated"
	}
	if p.authority != nil {
		family, authorityBranch := publicationAuthorityIdentity(info, intent)
		bridgeIntent := sharedPublicationBridgeIntent{
			Kind: sharedPublicationBridgePublication, Family: family, Branch: authorityBranch, PublicationID: intent.PublicationID,
		}
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
			if err := p.publishGraphDependencies(context.Background(), snapshot); err != nil {
				return fmt.Errorf("publishing durable memory dependencies for snapshot origin %d: %w", branch.SnapshotID, err)
			}
			_, volatile, err := p.authority.SnapshotProvider(branch.SnapshotID)
			if err != nil {
				return err
			}
			if volatile {
				if p.graph == nil {
					return errors.New("snapshot graph is unavailable for shared reference handoff")
				}
				if err := p.graph.FreezeSnapshotLedgerHandoff(snapshot); err != nil {
					return fmt.Errorf("freezing snapshot origin %d reference handoff: %w", branch.SnapshotID, err)
				}
				changes, _, frozen, err := p.graph.SnapshotLedgerHandoffChanges(snapshot, 0)
				if err != nil {
					return fmt.Errorf("reading snapshot origin %d reference catch-up: %w", branch.SnapshotID, err)
				}
				if !frozen {
					return fmt.Errorf("snapshot origin %d reference handoff is not frozen", branch.SnapshotID)
				}
				if err := p.publishSharedReferenceLedger(snapshot, changes, bridgeIntent); err != nil {
					return fmt.Errorf("transferring snapshot origin %d reference authority: %w", branch.SnapshotID, err)
				}
				references, _ := p.graph.OriginReferences(snapshot)
				if err := p.keepProviderReferences(snapshot, "", references); err != nil {
					return err
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
				if err := p.graphShared.CheckManifest(origin); err != nil {
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
					if err := p.publishSharedReferenceLedger(origin, changes, bridgeIntent); err != nil {
						repository.Close()
						return fmt.Errorf("transferring filesystem snapshot origin %d reference authority: %w", origin, err)
					}
					references, _ := repository.OriginReferences(origin)
					if err := p.keepProviderReferences(origin, repository.Path(), references); err != nil {
						repository.Close()
						return err
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
		// This single authority command is the irreversible publication boundary. It
		// installs branch roots, releases placement, and changes every prepared
		// origin from provider-owned to shared together. Cancellation before it
		// reverses every handoff; cancellation after it is a no-op.
		if _, err := p.authority.CommitPublication(family, authorityBranch, intent.PublicationID, roots, p.nodeID, p.incarnation, handoffOrigins); err != nil {
			return fmt.Errorf("committing publication roots in the authority: %w", err)
		}
		for _, encodedOrigin := range handoffOrigins {
			if err := p.releaseSharedPublicationBridge(ttcsnapshot.SnapshotID(encodedOrigin)); err != nil {
				return fmt.Errorf("releasing publication bridge for snapshot origin %d: %w", encodedOrigin, err)
			}
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
				// CommitPublication already moved these origins to shared
				// authority. If the provider's disposable active tier was lost
				// after that irreversible boundary, its local handoff ledger is
				// gone as well and completion is already satisfied locally.
				if errors.Is(err, os.ErrNotExist) {
					if _, statErr := os.Stat(config.Path); errors.Is(statErr, os.ErrNotExist) {
						continue
					} else if statErr != nil {
						return fmt.Errorf("stating published filesystem source %q: %w", filesystem.Root.Source, statErr)
					}
				}
				return err
			}
			for _, encodedOrigin := range filesystem.Origins {
				if err := repository.CompleteOriginHandoff(ttcsnapshot.SnapshotID(encodedOrigin)); err != nil {
					repository.Close()
					return fmt.Errorf("completing filesystem snapshot origin %d handoff: %w", encodedOrigin, err)
				}
			}
			// The publication keeps what the branch reaches of an image its
			// repository expands: a catalog claim no longer need.
			if err := repository.MarkBranchPublished(repositoryPhysicalBranch(config, intent.FilesystemBranch)); err != nil {
				repository.Close()
				return fmt.Errorf("recording the publication of filesystem source %q: %w", filesystem.Root.Source, err)
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

func (p *branchPublisher) publishSharedReferenceLedger(snapshot ttcsnapshot.SnapshotID, changes []ttcsnapshot.SnapshotReferenceMutation, intent sharedPublicationBridgeIntent) error {
	if p == nil || p.authority == nil || p.graphShared == nil || snapshot == 0 {
		return errors.New("shared reference publication is unavailable")
	}
	intent.Version = sharedPublicationBridgeIntentVersion
	intent.Snapshot = uint64(snapshot)
	if intent.CreatedAt.IsZero() {
		intent.CreatedAt = time.Now().UTC()
	}
	state, err := p.authority.AuthorityStateOf(cluster.AuthorityQuery{
		Transactions: cluster.All[string](), Publications: cluster.All[string](),
	})
	if err != nil {
		return fmt.Errorf("reading publication bridge authority: %w", err)
	}
	if err := persistSharedPublicationBridgeIntentWithAuthority(p.sharedRoot, intent, state); err != nil {
		return fmt.Errorf("persisting publication bridge intent: %w", err)
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
	// into the irreversible reclaiming state immediately before the authority publishes
	// the equivalent cold branch root. Authority reconciliation retains the
	// final cold roots before releasing these bridge roots after the atomic
	// provider-to-shared transition.
	bridges, err := p.publicationBridges(snapshot)
	if err != nil {
		return fmt.Errorf("reading publication bridge roots: %w", err)
	}
	for _, bridge := range bridges {
		if err := retainBridge(ctx, uint64(snapshot), bridge.RetainOperation, bridge.Name, bridge.Object.String()); err != nil {
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

// publicationBridges returns the bridges of the roots and pins of
// snapshot's manifest, which it reads in place: copying it, of hundreds of
// thousands of objects, for its few references took 50-70 ms each time.
func (p *branchPublisher) publicationBridges(snapshot ttcsnapshot.SnapshotID) ([]publicationBridgeReference, error) {
	var bridges []publicationBridgeReference
	err := p.graphShared.ViewManifest(snapshot, func(manifest *ttcsnapshot.SnapshotManifest) error {
		bridges = publicationBridgeReferences(snapshot, manifest.References)
		return nil
	})
	return bridges, err
}

type publicationBridgeReference struct {
	RetainOperation  string
	ReleaseOperation string
	Name             string
	Object           ttcsnapshot.ObjectID
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
			RetainOperation:  fmt.Sprintf("publication-bridge-retain-%016x-%x", uint64(snapshot), digest),
			ReleaseOperation: fmt.Sprintf("publication-bridge-release-%016x-%x", uint64(snapshot), digest),
			Name:             bridgeName,
			Object:           object,
		})
	}
	sort.Slice(bridges, func(i, j int) bool { return bridges[i].Name < bridges[j].Name })
	return bridges
}

// releaseSharedPublicationBridge removes the temporary provider-to-shared
// roots only after their replacement authority is durable. It is idempotent
// across lost responses and coordinator turnover.
func (p *branchPublisher) releaseSharedPublicationBridge(snapshot ttcsnapshot.SnapshotID) error {
	if p == nil || p.authority == nil || p.graphShared == nil || p.sharedRefs == nil || snapshot == 0 {
		return errors.New("shared publication bridge release is unavailable")
	}
	bridges, err := p.publicationBridges(snapshot)
	if err != nil {
		return fmt.Errorf("reading publication bridge roots: %w", err)
	}
	lease, err := p.authority.SharedGCLease()
	if err != nil {
		return err
	}
	if lease.Generation == 0 || !time.Now().Before(lease.ExpiresAt) {
		return errors.New("shared reference coordinator has no active lease")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if lease.NodeID == p.nodeID && lease.Incarnation == p.incarnation {
		if err := p.sharedRefs.reconcileAuthoritativeColdReferences(); err != nil {
			return fmt.Errorf("reconciling replacement roots: %w", err)
		}
		for _, bridge := range bridges {
			if err := p.sharedRefs.mutateReference(bridge.ReleaseOperation, bridge.Name, bridge.Object, false); err != nil {
				return fmt.Errorf("releasing publication bridge %q: %w", bridge.Name, err)
			}
		}
	} else {
		if p.peers == nil {
			return errors.New("cluster peer routing is unavailable")
		}
		peer, err := p.peers.peer(lease.NodeID, false)
		if err != nil {
			return err
		}
		for _, bridge := range bridges {
			if err := peer.ApplySharedSnapshotReference(ctx, uint64(snapshot), bridge.ReleaseOperation, bridge.Name, bridge.Object.String(), false); err != nil {
				return fmt.Errorf("releasing publication bridge %q: %w", bridge.Name, err)
			}
		}
	}
	return removeSharedPublicationBridgeIntent(p.sharedRoot, snapshot)
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
	if len(manifest.ExecConsoleLogs) == 0 {
		if manifest.ConsolePackSize != 0 {
			return coldBranchManifest{}, errors.New("cold manifest has an unindexed exec console pack")
		}
	} else {
		if manifest.ConsolePackSize == 0 || manifest.ConsolePackSize > math.MaxInt64 {
			return coldBranchManifest{}, errors.New("cold manifest has an invalid exec console pack size")
		}
		seenExec := make(map[string]struct{}, len(manifest.ExecConsoleLogs))
		var end uint64
		for _, descriptor := range manifest.ExecConsoleLogs {
			if !validColdExecID(descriptor.ExecID) || descriptor.Length == 0 || descriptor.Offset != end || descriptor.Offset > manifest.ConsolePackSize || descriptor.Length > manifest.ConsolePackSize-descriptor.Offset {
				return coldBranchManifest{}, fmt.Errorf("cold manifest contains invalid retained output for exec session %q", descriptor.ExecID)
			}
			if _, duplicate := seenExec[descriptor.ExecID]; duplicate {
				return coldBranchManifest{}, fmt.Errorf("cold manifest repeats retained output for exec session %q", descriptor.ExecID)
			}
			seenExec[descriptor.ExecID] = struct{}{}
			end = descriptor.Offset + descriptor.Length
		}
		if end != manifest.ConsolePackSize {
			return coldBranchManifest{}, errors.New("cold manifest does not cover its exec console pack")
		}
		packInfo, err := os.Stat(coldConsolePackPath(path))
		if err != nil {
			return coldBranchManifest{}, fmt.Errorf("opening cold exec console pack: %w", err)
		}
		if packInfo.Size() < 0 || uint64(packInfo.Size()) != manifest.ConsolePackSize {
			return coldBranchManifest{}, fmt.Errorf("cold exec console pack has size %d, want %d", packInfo.Size(), manifest.ConsolePackSize)
		}
	}
	seenMounts := make(map[string]struct{}, len(manifest.InternalMounts))
	totalMountBytes := 0
	for _, mount := range manifest.InternalMounts {
		destination := filepath.Clean(mount.Destination)
		if destination != mount.Destination || !filepath.IsAbs(destination) || destination == "/" || mount.Mode&^0777 != 0 || len(mount.Contents) > 1<<20 {
			return coldBranchManifest{}, fmt.Errorf("cold manifest contains invalid internal mount %q", mount.Destination)
		}
		if _, duplicate := seenMounts[destination]; duplicate {
			return coldBranchManifest{}, fmt.Errorf("cold manifest repeats internal mount %q", destination)
		}
		seenMounts[destination] = struct{}{}
		totalMountBytes += len(mount.Contents)
		if totalMountBytes > 4<<20 {
			return coldBranchManifest{}, errors.New("cold manifest internal mounts exceed 4 MiB")
		}
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
		if _, err := ttcvolume.NewRemoteSnapshotDescriptor(root.RepositoryID, root.BranchID, root.Root, root.BaseRoot, root.NextInode, root.LogicalBytes); err != nil {
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

func portableInternalMountsFromColdManifest(manifest coldBranchManifest) []ttcbundle.PortableInternalMount {
	mounts := make([]ttcbundle.PortableInternalMount, len(manifest.InternalMounts))
	for i, mount := range manifest.InternalMounts {
		mounts[i] = ttcbundle.PortableInternalMount{
			Destination: mount.Destination,
			Contents:    append([]byte(nil), mount.Contents...),
			Mode:        os.FileMode(mount.Mode),
		}
	}
	return mounts
}

// validateColdManifestIdentity distinguishes a cold publication transaction
// from the immutable filesystem ancestor that remains after activation.
// RecordBranchActivation clears PublicationID to fence the consumed one-shot
// transaction, but deliberately retains StorageManifest: a later stop/start
// or incremental publication may still use those immutable roots. The
// container and branch identity must always match; only an activated local
// incarnation may refer to the retained manifest without retaining its
// transaction ID.
func validateColdManifestIdentity(info ttcbundle.ContainerInfo, branch ttcbundle.BranchInfo, manifest coldBranchManifest) error {
	if manifest.Container != info.ID || manifest.Branch != branch.ID {
		return fmt.Errorf("cold manifest belongs to %s@%s, branch is %s@%s", manifest.Container, manifest.Branch, info.ID, branch.ID)
	}
	if manifest.PublicationID == branch.PublicationID {
		return nil
	}
	state := normalizeLifecycleState(branch.State)
	if branch.PublicationID == "" && branch.StorageState == "local" &&
		(state == "running" || state == "paused" || state == "exited" || state == "failed" || state == "dead") {
		return nil
	}
	// Explicit stop of a durable dehydrated branch consumes its process
	// snapshot and publication transaction without consuming the immutable
	// filesystem ancestor. Until fresh-start materialization commits, that
	// exited branch correctly remains shared with an empty PublicationID.
	if branch.PublicationID == "" && branch.StorageState == "shared" && state == "exited" {
		return nil
	}
	return fmt.Errorf("cold manifest publication is %q, branch publication is %q", manifest.PublicationID, branch.PublicationID)
}

func (b *runtimeBackend) installColdInternalMounts(info ttcbundle.ContainerInfo, branch ttcbundle.BranchInfo) error {
	if branch.StorageManifest == "" {
		return nil
	}
	manifest, err := readColdBranchManifest(branch.StorageManifest)
	if err != nil {
		return err
	}
	if err := validateColdManifestIdentity(info, branch, manifest); err != nil {
		return err
	}
	if err := b.containerStore().InstallPortableInternalMounts(info.ID, portableInternalMountsFromColdManifest(manifest)); err != nil {
		return fmt.Errorf("materializing portable internal mounts: %w", err)
	}
	return nil
}

func (b *runtimeBackend) installColdExecConsoleLogs(info ttcbundle.ContainerInfo, branch ttcbundle.BranchInfo) error {
	if branch.StorageManifest == "" {
		return nil
	}
	manifest, err := readColdBranchManifest(branch.StorageManifest)
	if err != nil {
		return err
	}
	if err := validateColdManifestIdentity(info, branch, manifest); err != nil {
		return err
	}
	if err := installColdExecConsolePack(b.activeDataRoot(), info, branch.ID, manifest, branch.StorageManifest); err != nil {
		return fmt.Errorf("materializing retained exec output: %w", err)
	}
	return nil
}

func (b *runtimeBackend) ensureLocalColdBranch(info ttcbundle.ContainerInfo, branch ttcbundle.BranchInfo) error {
	// Portable internal mounts are not part of a TTC volume repository. Restore
	// them even when a valid local control image is already cached: the branch
	// may have returned to a node after its prior authoritative local state was
	// reclaimed.
	if err := b.installColdInternalMounts(info, branch); err != nil {
		return err
	}
	if err := b.installColdExecConsoleLogs(info, branch); err != nil {
		return err
	}
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
	manifest, err := readColdBranchManifest(branch.StorageManifest)
	if err != nil {
		return err
	}
	if err := validateColdManifestIdentity(info, branch, manifest); err != nil {
		return err
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
	if err := b.discardSupersededBranchFilesystems(info, branch, coldFilesystemRoots(manifest)); err != nil {
		return err
	}
	if _, err := b.materializeDistributedFilesystemRoots(info, branch.FilesystemBranch, coldFilesystemRoots(manifest), true, true); err != nil {
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
		if err := b.snapshotShared.CheckManifest(ttcsnapshot.SnapshotID(manifest.MemorySnapshot)); err != nil {
			return fmt.Errorf("opening shared graph memory snapshot: %w", err)
		}
		return nil
	}
	return errors.New("new-format cold manifest omits graph-native memory snapshot")
}

// ensureLocalColdFilesystem materializes the filesystem portion of a durable
// stopped branch without requiring dehydration memory records. A stopped
// branch has deliberately discarded execution state, but its next fresh start
// must still reconstruct the committed writable repositories on whichever
// node claims its unplaced authority record.
func (b *runtimeBackend) ensureLocalColdFilesystem(info ttcbundle.ContainerInfo, branch ttcbundle.BranchInfo) error {
	if branch.StorageManifest == "" {
		return nil
	}
	if err := b.installColdExecConsoleLogs(info, branch); err != nil {
		return err
	}
	if err := validateColdManifestPath(b.dataRoot, branch.StorageManifest); err != nil {
		return err
	}
	// Materializing a committed snapshot into this node's disposable cache is
	// not an authority transition. In particular, cp and cold-fork source
	// preparation may run on any node while the branch remains unplaced. The
	// caller records "local" only after it has claimed placement and committed
	// a live runtime.
	return b.materializeColdFilesystem(info, branch)
}

// discardSupersededBranchFilesystems removes branch's filesystems from this
// node's repositories if they are not the ones roots, the branch's committed
// filesystems, name. An earlier incarnation of the branch here leaves them,
// and importing roots keeps a branch a repository already has: a branch that
// ran and was captured elsewhere since would come back to its older files.
// A branch last committed here, or imported from roots, is kept.
func (b *runtimeBackend) discardSupersededBranchFilesystems(info ttcbundle.ContainerInfo, branch ttcbundle.BranchInfo, roots []cluster.FilesystemRootDescriptor) error {
	spec, err := b.containerStore().LoadSpec(info.ID)
	if err != nil {
		return err
	}
	configs, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		return err
	}
	bySource := make(map[string]string, len(roots))
	for _, root := range roots {
		bySource[root.Source] = root.Root
	}
	superseded := false
	seen := make(map[string]bool)
	for _, config := range configs {
		want, ok := bySource[config.Source]
		if !ok {
			continue
		}
		for _, path := range []string{config.Path, distributedRepositoryCachePath(b.activeDataRoot(), info.ID, config.Source)} {
			path = filepath.Clean(path)
			if seen[path] {
				continue
			}
			seen[path] = true
			if _, err := os.Stat(filepath.Join(path, "volume.json")); errors.Is(err, os.ErrNotExist) {
				continue
			} else if err != nil {
				return err
			}
			repository, err := ttcvolume.OpenRepository(path, ttcvolume.CopyMode(config.CopyMode))
			if err != nil {
				return err
			}
			current, branchErr := repository.Branch(branch.FilesystemBranch)
			if err := repository.Close(); err != nil {
				return err
			}
			if errors.Is(branchErr, os.ErrNotExist) {
				continue
			}
			if branchErr != nil {
				return branchErr
			}
			if current.Root != want && remoteCatalogRoot(current.RemoteCatalog) != want {
				superseded = true
			}
		}
	}
	if !superseded {
		return nil
	}
	if err := b.containerStore().RemoveBranchFilesystems(info.ID, branch); err != nil {
		return fmt.Errorf("discarding superseded local filesystems of %s@%s: %w", info.Name, branch.ID, err)
	}
	return nil
}

// remoteCatalogRoot returns the root that a branch imported from a remote
// descriptor was imported from, as its catalog names it, or "".
func remoteCatalogRoot(catalog string) string {
	parts := strings.Split(catalog, ":")
	if len(parts) != 3 {
		return ""
	}
	return parts[1]
}

// materializeColdFilesystem installs one committed filesystem snapshot in the
// node-local cache without changing branch lifecycle or storage authority. It
// is used by read-only management operations such as cp-from-dehydrated; the
// shared manifest remains authoritative and the local files are disposable.
func (b *runtimeBackend) materializeColdFilesystem(info ttcbundle.ContainerInfo, branch ttcbundle.BranchInfo) error {
	if branch.StorageManifest == "" {
		return nil
	}
	if err := validateColdManifestPath(b.dataRoot, branch.StorageManifest); err != nil {
		return err
	}
	manifest, err := readColdBranchManifest(branch.StorageManifest)
	if err != nil {
		return err
	}
	if err := validateColdManifestIdentity(info, branch, manifest); err != nil {
		return err
	}
	// Hostname, hosts, and resolv.conf are TTC-owned bind mounts outside the
	// volume graph. A cold branch may be inspected on any node, so materialize
	// these small portable objects alongside the repository cache rather than
	// leaving canonical paths that point into the former owner's scratch root.
	if err := b.containerStore().InstallPortableInternalMounts(info.ID, portableInternalMountsFromColdManifest(manifest)); err != nil {
		return fmt.Errorf("materializing portable internal mounts: %w", err)
	}
	if len(manifest.Filesystems) == 0 {
		return errors.New("cold manifest omits filesystem graph roots")
	}
	if err := b.discardSupersededBranchFilesystems(info, branch, coldFilesystemRoots(manifest)); err != nil {
		return err
	}
	if _, err := b.materializeDistributedFilesystemRoots(info, branch.FilesystemBranch, coldFilesystemRoots(manifest), true, true); err != nil {
		return err
	}
	// An ordinary rehydrated Sentry receives shared-pack relay FDs and keeps
	// this imported repository lazy. The short-lived offline cp helper has no
	// control RPC before its init process starts, so make its host repository
	// self-contained first. Whole-file materialization preserves repository
	// identity and CoW metadata; only this explicit management path pays the
	// recursive cost.
	sources, cleanup, err := b.prepareColdDirectFilesystemSources(info, branch, branch.FilesystemBranch)
	if err != nil {
		return err
	}
	defer cleanup()
	seen := make(map[*ttcvolume.Repository]struct{}, len(sources))
	for _, source := range sources {
		if source.Repository == nil {
			continue
		}
		if _, ok := seen[source.Repository]; ok {
			continue
		}
		seen[source.Repository] = struct{}{}
		if err := source.Repository.MaterializeRemoteObjects(); err != nil {
			return fmt.Errorf("materializing repository for %q: %w", branch.ID, err)
		}
	}
	return nil
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

// prepareLocalDirectFilesystemSources serves a process-free capture from the
// provider's authoritative local repositories while its asynchronous shared
// publication is still pending. The hidden filesystem branch pins every
// immutable object until the remote transaction becomes independent, so no
// shared manifest or bulk upload is required on the fork critical path.
func (b *runtimeBackend) prepareLocalDirectFilesystemSources(info ttcbundle.ContainerInfo, filesystemBranch string) ([]ttcvolume.RemoteObjectSource, func(), error) {
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
	cleanup := func() {
		for _, repository := range repositories {
			_ = repository.Close()
		}
	}
	fail := func(err error) ([]ttcvolume.RemoteObjectSource, func(), error) {
		cleanup()
		return nil, nil, err
	}
	sources := make([]ttcvolume.RemoteObjectSource, 0, len(configs))
	seenPaths := make(map[string]struct{})
	for _, config := range configs {
		path := filepath.Clean(config.Path)
		if _, duplicate := seenPaths[path]; duplicate {
			continue
		}
		seenPaths[path] = struct{}{}
		repository, err := ttcvolume.OpenRepository(config.Path, ttcvolume.CopyMode(config.CopyMode))
		if err != nil {
			return fail(err)
		}
		if _, err := repository.DescribeRemoteSnapshot(repositoryPhysicalBranch(config, filesystemBranch)); err != nil {
			_ = repository.Close()
			return fail(fmt.Errorf("opening local filesystem generation %q: %w", config.Source, err))
		}
		repositories = append(repositories, repository)
		key := ttcvolume.RemoteSourceKey(sha256.Sum256([]byte(config.Source)))
		sources = append(sources, ttcvolume.RemoteObjectSource{Key: key, Repository: repository})
	}
	if len(sources) == 0 {
		cleanup()
		return nil, nil, errors.New("dehydrated branch has no managed filesystem repositories")
	}
	return sources, cleanup, nil
}

// coldRootsQuery is the query of the authority records collectColdStorage
// reads: the branches, whose roots keep their cold manifests.
var coldRootsQuery = cluster.AuthorityQuery{Branches: cluster.All[string]()}

// collectColdStorage traces every authoritative and pending manifest before
// reclaiming immutable packs. A grace period protects a publisher between
// writing its pack and atomically committing its manifest.
func collectColdStorage(sharedRoot, localRoot string, publications *ttcstorage.Queue, transactions cluster.TransactionCatalog, authority *cluster.AuthorityState) error {
	return collectColdStorageOlderThan(sharedRoot, localRoot, publications, transactions, authority, time.Now().Add(-10*time.Minute))
}

// collectColdStorageOlderThan is the common tracer with an explicit physical
// publication grace. Periodic callers retain the ordinary ten-minute window
// that protects pack-before-manifest publication. The last clean cluster node
// may pass a future boundary after every node's publication queue has drained,
// because no unfenced publisher remains that could still name an unreferenced
// object.
func collectColdStorageOlderThan(sharedRoot, localRoot string, publications *ttcstorage.Queue, transactions cluster.TransactionCatalog, authority *cluster.AuthorityState, olderThan time.Time) error {
	store := ttcbundle.Store{Root: sharedRoot, ActiveRoot: localRoot}
	// Destructive tracing must include internal distributed physical records.
	// User-facing ListContainers intentionally hides them, but their cold
	// manifests remain authoritative after the authority unplaces a remote branch.
	containers, err := store.ListContainersForGC()
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
	// A live clustered branch may continue faulting immutable files from the
	// publication that materialized it after its old physical source record has
	// been reclaimed. The authority deliberately retains that filesystem root until a
	// later publication replaces it or branch removal drops it. Include those
	// logical roots directly: deriving reachability only from physical
	// container.json records creates a window in which ordinary cold GC can
	// delete the backing manifest of a running migrated branch.
	if authority != nil {
		for _, branch := range authority.Branches {
			if branch.Corrupted || branch.Roots.FilesystemRoot == "" {
				continue
			}
			queue = append(queue, filepath.Join(sharedRoot, branch.Roots.FilesystemRoot))
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
	// Graph handoff manifests are transaction-scoped restore descriptors. A
	// bound manifest remains authoritative after the transaction becomes
	// independent: the target may still contain unfaulted pages and needs the
	// mapping to recreate its detached shared-pack reader after a restart. Keep
	// it until the transaction itself is retired. A terminal transaction that
	// never bound this exact handoff may reclaim its abandoned candidate.
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
		if tx, exists := transactionsByID[transactionID]; exists {
			if !tx.Terminal() {
				continue
			}
			if tx.GraphHandoff != nil && filepath.Clean(tx.GraphHandoff.GraphManifest) == filepath.Join("cluster", "memory-manifests", entry.Name()) {
				continue
			}
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
		if err := os.Remove(coldConsolePackPath(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return os.Remove(path)
	}); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// A crash may leave a fully-written console pack before its manifest was
	// atomically published. Reclaim only TTC-named orphan packs after the same
	// grace boundary used for manifests; unknown files remain untouched.
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if errors.Is(walkErr, os.ErrNotExist) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".console.pack") {
			return nil
		}
		manifestPath := strings.TrimSuffix(path, ".console.pack") + ".json"
		if _, err := os.Stat(manifestPath); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
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
	var authority *cluster.AuthorityState
	if err == nil && b.distributedSource != nil && b.distributedSource.authority != nil {
		authority, err = b.distributedSource.authority.AuthorityStateOf(coldRootsQuery)
	}
	if err == nil {
		err = collectColdStorage(b.dataRoot, b.activeDataRoot(), b.publicationQueue, transactions, authority)
	}
	if err != nil && b.stderr != nil {
		fmt.Fprintf(b.stderr, "ttcd: warning: collecting terminal distributed storage: %v\n", err)
	}
}
