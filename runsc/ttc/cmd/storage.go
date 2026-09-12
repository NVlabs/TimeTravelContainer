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
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"gvisor.dev/gvisor/pkg/ttcpack"
	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	"gvisor.dev/gvisor/pkg/ttcvolume"
	"gvisor.dev/gvisor/runsc/boot"
	"gvisor.dev/gvisor/runsc/specutils"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
	"gvisor.dev/gvisor/runsc/ttc/cluster"
	ttcstorage "gvisor.dev/gvisor/runsc/ttc/storage"
)

type sharedSnapshotObjectReader struct {
	store   *ttcsnapshot.SharedPackStore
	mu      sync.Mutex
	allowed map[ttcsnapshot.ObjectID]struct{}
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
	Version    int       `json:"version"`
	Container  string    `json:"container"`
	Branch     string    `json:"branch"`
	Generation uint64    `json:"generation"`
	Kind       string    `json:"kind"`
	CreatedAt  time.Time `json:"created_at"`
	// MemorySnapshot names the graph-native immutable memory origin. The
	// separate control pack contains only the serialized process/kernel state.
	MemorySnapshot    uint64                   `json:"memory_snapshot,omitempty"`
	MemoryControlPack string                   `json:"memory_control_pack,omitempty"`
	Filesystems       []coldFilesystemSnapshot `json:"filesystems,omitempty"`
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
	if branch.StorageGeneration != intent.Generation {
		return ttcstorage.Result{}, fmt.Errorf("storage generation changed to %d", branch.StorageGeneration)
	}
	path := coldManifestPath(p.sharedRoot, info.ID, branch.ID, intent.Generation)
	if existing, err := readColdBranchManifest(path); err == nil {
		if existing.Container != info.ID || existing.Branch != branch.ID || existing.Generation != intent.Generation || existing.Kind != intent.Kind {
			return ttcstorage.Result{}, errors.New("existing cold manifest has a conflicting identity")
		}
		if existing.MemoryControlPack != "" {
			id, err := ttcpack.ParseID(existing.MemoryControlPack)
			if err != nil {
				return ttcstorage.Result{}, err
			}
			file, _, err := ttcpack.Open(filepath.Join(p.sharedRoot, "cold"), id)
			if err != nil {
				return ttcstorage.Result{}, err
			}
			if err := file.Close(); err != nil {
				return ttcstorage.Result{}, err
			}
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
			if _, err := p.graphShared.Manifest(ttcsnapshot.SnapshotID(existing.MemorySnapshot)); err != nil {
				return ttcstorage.Result{}, fmt.Errorf("validating published graph memory snapshot: %w", err)
			}
		}
		return ttcstorage.Result{Manifest: path}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return ttcstorage.Result{}, err
	}
	staging := filepath.Join(p.localRoot, "publication-staging", intent.ID)
	if err := os.RemoveAll(staging); err != nil {
		return ttcstorage.Result{}, err
	}
	if err := os.MkdirAll(staging, 0700); err != nil {
		return ttcstorage.Result{}, err
	}
	defer os.RemoveAll(staging)
	manifest := coldBranchManifest{Version: 3, Container: info.ID, Branch: branch.ID, Generation: intent.Generation, Kind: intent.Kind, CreatedAt: time.Now().UTC()}
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
		descriptor, snapshotErr := repository.DescribeRemoteSnapshot(physicalBranch)
		origins, originsErr := repository.PrepareBranchPublication(physicalBranch)
		snapshotErr = errors.Join(snapshotErr, originsErr)
		for _, origin := range origins {
			if snapshotErr == nil {
				snapshotErr = repository.PublishOriginHandoff(context.Background(), p.graphShared, origin)
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
		memory := publicationMemoryControlPath(p.localRoot, intent.MemoryControl)
		if _, err := os.Stat(memory); err != nil {
			return ttcstorage.Result{}, fmt.Errorf("opening local dehydration image: %w", err)
		}
		packID := ttcpack.IDForKey("memory-control", info.ID, branch.ID, fmt.Sprint(intent.Generation))
		pack, err := ttcpack.WriteDirectory(memory, filepath.Join(p.sharedRoot, "cold"), packID)
		if err != nil {
			return ttcstorage.Result{}, err
		}
		manifest.MemoryControlPack = pack.String()
		if branch.SnapshotID != 0 && len(branch.MemoryRoots) != 0 {
			if p.graph == nil || p.graphData == nil || p.graphShared == nil {
				return ttcstorage.Result{}, errors.New("graph-native snapshot publication is unavailable")
			}
			snapshot := ttcsnapshot.SnapshotID(branch.SnapshotID)
			if err := p.graphShared.PublishHandoff(context.Background(), p.graph, snapshot, p.graphData); err != nil {
				return ttcstorage.Result{}, fmt.Errorf("publishing graph memory snapshot %d: %w", snapshot, err)
			}
			manifest.MemorySnapshot = branch.SnapshotID
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

func coldManifestPath(sharedRoot, container, branch string, generation uint64) string {
	return filepath.Join(sharedRoot, "cold", "manifests", hex.EncodeToString([]byte(container)), hex.EncodeToString([]byte(branch)), fmt.Sprintf("%016x.json", generation))
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
		if intent.MemoryControl == "" {
			return false, errors.New("dehydration publication omits sealed control state")
		}
		if stat, err := os.Stat(publicationMemoryControlPath(p.localRoot, intent.MemoryControl)); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return false, nil
			}
			return false, err
		} else if !stat.IsDir() {
			return false, errors.New("sealed dehydration control state is not a directory")
		}
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
		} else {
			return false, errors.New("new-format dehydration branch lacks graph-native snapshot identity")
		}
	}
	if p.authority != nil {
		lifecycle := "exited"
		manifestPath := coldManifestPath(p.sharedRoot, info.ID, intent.Branch, intent.Generation)
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
		if _, err := p.authority.EnsurePublicationReady(info.ID, intent.Branch, intent.Generation, lifecycle, captureRoots); err != nil {
			return false, fmt.Errorf("authorizing publication capture in Raft: %w", err)
		}
	}
	return true, nil
}

func (p *branchPublisher) Cleanup(intent ttcstorage.Intent) error {
	if p.authority != nil {
		if err := p.authority.CancelPublication(intent.Container, intent.Branch, intent.Generation); err != nil {
			return fmt.Errorf("canceling publication authority: %w", err)
		}
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
		removeErr := repository.RemoveBranch(repositoryPhysicalBranch(config, intent.FilesystemBranch))
		if errors.Is(removeErr, os.ErrNotExist) {
			removeErr = nil
		}
		gcErr := repository.GarbageCollect()
		closeErr := repository.Close()
		if removeErr != nil || gcErr != nil || closeErr != nil {
			return errors.Join(removeErr, gcErr, closeErr)
		}
	}
	if intent.MemoryBranch != "" {
		branch, branchErr := persistentBranchInfo(info, intent.Branch)
		if branchErr == nil && branch.SnapshotID != 0 && len(branch.MemoryRoots) != 0 {
			return os.RemoveAll(publicationMemoryControlPath(p.localRoot, intent.MemoryControl))
		}
		return errors.Join(branchErr, errors.New("new-format dehydration cleanup lacks graph-native snapshot identity"))
	}
	return nil
}

func publicationMemoryControlPath(localRoot, capture string) string {
	return filepath.Join(localRoot, "publication-captures", "memory-control", capture)
}

// releasePublishedProviderGraphRoots drops the temporary roots installed for
// remote consumers once Raft routes the origin to its committed shared pack.
// Branch-local roots remain untouched, and targets now resolve the immutable
// data from shared storage, so the provider copy is cache rather than
// authority after this boundary.
func releasePublishedProviderGraphRoots(graph *ttcsnapshot.Store, data *ttcsnapshot.LocalDataStore, snapshot ttcsnapshot.SnapshotID) error {
	if graph == nil || snapshot == 0 {
		return nil
	}
	prefix := fmt.Sprintf("remote/%d/", snapshot)
	bindings := make(map[string]ttcsnapshot.ObjectID)
	for name, object := range graph.Inspect().Roots {
		if !strings.HasPrefix(name, prefix) && !(strings.HasPrefix(name, "bridge/edge/") && object.Snapshot == snapshot) {
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

// snapshotImmutableTree seals the small process/control portion of a
// dehydration image independently of the user-visible local image. Regular
// files are hard-linked within TTC_LOCAL_ROOT and copied only if the backing
// filesystem rejects links. The sparse memory payload is rooted separately in
// the graph object store, so this tree normally contains only metadata and
// control JSON.
func snapshotImmutableTree(source, destination string) error {
	if err := os.RemoveAll(destination); err != nil {
		return err
	}
	if err := filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case entry.Type().IsRegular():
			if err := os.Link(path, target); err == nil {
				return nil
			} else if !errors.Is(err, syscall.EXDEV) && !errors.Is(err, syscall.EPERM) && !errors.Is(err, syscall.EOPNOTSUPP) {
				return err
			}
			input, err := os.Open(path)
			if err != nil {
				return err
			}
			defer input.Close()
			output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(output, input)
			return errors.Join(copyErr, output.Sync(), output.Close())
		default:
			return fmt.Errorf("unsupported dehydration control entry %q (%s)", relative, info.Mode())
		}
	}); err != nil {
		_ = os.RemoveAll(destination)
		return err
	}
	directory, err := os.Open(destination)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
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
		if intent.Kind == "dehydrate" && branch.SnapshotID != 0 && len(branch.MemoryRoots) != 0 {
			// The shared graph manifest was committed by Publish before the cold
			// branch manifest. Switch object routing to shared first; a crash here
			// leaves a retryable publishing branch whose data is already durable.
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
				if err := p.authority.PublishSnapshotOrigin(p.nodeID, p.incarnation, branch.SnapshotID); err != nil {
					return fmt.Errorf("publishing snapshot origin %d in Raft: %w", branch.SnapshotID, err)
				}
			}
			if p.graph != nil {
				if err := p.graph.MarkShared(snapshot); err != nil {
					return fmt.Errorf("marking snapshot origin %d shared: %w", branch.SnapshotID, err)
				}
				if err := p.graph.UnfreezeSnapshotLedger(snapshot); err != nil {
					return fmt.Errorf("consuming snapshot origin %d reference handoff: %w", branch.SnapshotID, err)
				}
				if err := releasePublishedProviderGraphRoots(p.graph, p.graphData, snapshot); err != nil {
					return fmt.Errorf("releasing volatile provider roots for snapshot origin %d: %w", branch.SnapshotID, err)
				}
			}
		}
		manifest, err := readColdBranchManifest(result.Manifest)
		if err != nil {
			return fmt.Errorf("reading committed cold manifest: %w", err)
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
					if err := p.authority.PublishSnapshotOrigin(p.nodeID, p.incarnation, encodedOrigin); err != nil {
						repository.Close()
						return fmt.Errorf("publishing filesystem snapshot origin %d in Raft: %w", origin, err)
					}
				}
				if err := repository.CompleteOriginHandoff(origin); err != nil {
					repository.Close()
					return fmt.Errorf("completing filesystem snapshot origin %d handoff: %w", origin, err)
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
		if _, err := p.authority.CommitPublication(info.ID, intent.Branch, intent.Generation, roots); err != nil {
			return fmt.Errorf("committing publication roots in Raft: %w", err)
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
	if err := store.CompleteBranchPublication(intent.Container, intent.Branch, intent.Generation, result.Manifest, lifecycle, true); err != nil {
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
	var applyHandoff func(context.Context, uint64, uint64, []cluster.SharedReferenceMutation) error
	var flushOrigin func(context.Context, uint64) error
	if lease.NodeID == p.nodeID && lease.Incarnation == p.incarnation {
		importOrigin = func(ctx context.Context, snapshot uint64) error {
			return p.sharedRefs.ImportSharedSnapshot(ctx, p.nodeID, snapshot)
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
		applyHandoff = peer.ApplySharedSnapshotHandoff
		flushOrigin = peer.FlushSharedSnapshot
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := importOrigin(ctx, uint64(snapshot)); err != nil {
		return err
	}
	const batchSize = 4096
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
// releasing the placement. Both transitions are generation-fenced and a
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
	err := store.UpdateBranchStorage(intent.Container, intent.Branch, intent.Generation, "error", cause.Error(), retryAt)
	if err == nil {
		p.recordStorageEvent(intent, "error", cause.Error(), "")
	}
	return err
}

func (p *branchPublisher) recordStorageEvent(intent ttcstorage.Intent, state, storageError, manifest string) {
	if p.event == nil {
		return
	}
	attributes := map[string]string{"state": state, "generation": fmt.Sprint(intent.Generation), "kind": intent.Kind}
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
	if manifest.Version != 3 {
		return coldBranchManifest{}, fmt.Errorf("cold manifest format %d is unsupported; TTC requires snapshot-graph format 3", manifest.Version)
	}
	if manifest.Container == "" || manifest.Branch == "" || manifest.Generation == 0 || (manifest.Kind != "stop" && manifest.Kind != "dehydrate") {
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
	if manifest.MemoryControlPack != "" {
		if _, err := ttcpack.ParseID(manifest.MemoryControlPack); err != nil {
			return coldBranchManifest{}, fmt.Errorf("validating memory-control pack: %w", err)
		}
	}
	if manifest.Kind == "dehydrate" {
		if manifest.MemorySnapshot == 0 {
			return coldBranchManifest{}, errors.New("dehydration manifest omits graph-native memory snapshot")
		}
		if manifest.MemoryControlPack == "" {
			return coldBranchManifest{}, errors.New("dehydration manifest omits memory control state")
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
		// generation. Branch-name-only local control paths are caches, not
		// authority; discard an old or malformed tree before materializing the
		// manifest selected by the persistent branch generation.
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
	if err := store.UpdateBranchStorage(info.ID, branch.ID, branch.StorageGeneration, "materializing", "", time.Time{}); err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			_ = store.UpdateBranchStorage(info.ID, branch.ID, branch.StorageGeneration, "error", retErr.Error(), time.Time{})
		}
	}()
	manifest, err := readColdBranchManifest(branch.StorageManifest)
	if err != nil {
		return err
	}
	if manifest.Container != info.ID || manifest.Branch != branch.ID || manifest.Generation != branch.StorageGeneration {
		return errors.New("cold manifest identity does not match branch generation")
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
	if manifest.MemoryControlPack == "" {
		return errors.New("cold manifest omits dehydration control state")
	}
	controlPack, err := ttcpack.ParseID(manifest.MemoryControlPack)
	if err != nil {
		return err
	}
	if _, err := os.Stat(localImage); errors.Is(err, os.ErrNotExist) {
		if err := ttcpack.Materialize(filepath.Join(b.dataRoot, "cold"), controlPack, localImage); err != nil {
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
		return store.UpdateBranchStorage(info.ID, branch.ID, branch.StorageGeneration, "materializing", "", time.Time{})
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
	if err := store.UpdateBranchStorage(info.ID, branch.ID, branch.StorageGeneration, "materializing", "", time.Time{}); err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			_ = store.UpdateBranchStorage(info.ID, branch.ID, branch.StorageGeneration, "error", retErr.Error(), time.Time{})
		}
	}()
	manifest, err := readColdBranchManifest(branch.StorageManifest)
	if err != nil {
		return err
	}
	if manifest.Container != info.ID || manifest.Branch != branch.ID || manifest.Generation != branch.StorageGeneration {
		return errors.New("cold manifest identity does not match branch generation")
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
		reader, err := newSharedSnapshotObjectReader(b.snapshotShared, filesystem.Root.Root)
		if err != nil {
			return fail(err)
		}
		fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
		if err != nil {
			return fail(err)
		}
		server := os.NewFile(uintptr(fds[0]), "ttc-cold-graph-server")
		client := os.NewFile(uintptr(fds[1]), "ttc-cold-graph-client")
		done := make(chan error, 1)
		key := ttcvolume.RemoteSourceKey(sha256.Sum256([]byte(config.Source)))
		go func(reader *sharedSnapshotObjectReader) {
			err := ttcvolume.ServeRemoteObjects(server, relays.token, []ttcvolume.RemoteObjectSource{{
				Key: key, Reader: reader, Catalog: reader,
			}})
			b.reportColdRelayError("filesystem", err)
			done <- err
			close(done)
		}(reader)
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
		reader, err := newSharedSnapshotObjectReader(b.snapshotShared, filesystem.Root.Root)
		if err != nil {
			return fail(err)
		}
		repository, err := ttcvolume.OpenRepository(config.Path, ttcvolume.CopyMode(config.CopyMode))
		if err != nil {
			return fail(err)
		}
		physicalBranch := repositoryPhysicalBranch(config, filesystemBranch)
		server, client := net.Pipe()
		var token ttcvolume.RemoteToken
		if _, err := rand.Read(token[:]); err != nil {
			repository.Close()
			server.Close()
			client.Close()
			return fail(err)
		}
		key := ttcvolume.RemoteSourceKey(sha256.Sum256([]byte(config.Source)))
		go func(reader *sharedSnapshotObjectReader) {
			_ = ttcvolume.ServeRemoteObjects(server, token, []ttcvolume.RemoteObjectSource{{Key: key, Reader: reader, Catalog: reader}})
		}(reader)
		if err := repository.AttachRemoteObjectSource(client, token, key, physicalBranch); err != nil {
			server.Close()
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
			path := coldManifestPath(sharedRoot, intent.Container, intent.Branch, intent.Generation)
			if _, err := os.Stat(path); err == nil {
				queue = append(queue, path)
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	manifests := make(map[string]struct{})
	packs := make(map[ttcpack.ID]struct{})
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
		manifest, err := readColdBranchManifest(path)
		if err != nil {
			return fmt.Errorf("tracing cold manifest %q: %w", path, err)
		}
		manifests[path] = struct{}{}
		if manifest.MemoryControlPack != "" {
			id, err := ttcpack.ParseID(manifest.MemoryControlPack)
			if err != nil {
				return err
			}
			packs[id] = struct{}{}
		}
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
	_, err = ttcpack.Collect(filepath.Join(sharedRoot, "cold"), packs, olderThan.UnixNano())
	return err
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
