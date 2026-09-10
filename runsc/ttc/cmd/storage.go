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
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"gvisor.dev/gvisor/pkg/sentry/ttcchunk"
	"gvisor.dev/gvisor/pkg/ttcpack"
	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	"gvisor.dev/gvisor/pkg/ttcvolume"
	"gvisor.dev/gvisor/runsc/boot"
	"gvisor.dev/gvisor/runsc/specutils"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
	"gvisor.dev/gvisor/runsc/ttc/cluster"
	ttcstorage "gvisor.dev/gvisor/runsc/ttc/storage"
)

type coldPackObjectReader struct{ directory *ttcpack.Directory }

func (r coldPackObjectReader) ReadObjectRange(object string, destination []byte, offset int64) (int, error) {
	return r.directory.ReadAt("objects/"+object, destination, offset)
}

type coldPackMemoryReader struct{ directory *ttcpack.Directory }

func (r coldPackMemoryReader) ReadMemoryChunk(id ttcchunk.ID) ([]byte, error) {
	path := filepath.ToSlash(filepath.Join("chunks", id.String()[:4], id.String()+".chunk"))
	size, err := r.directory.Size(path)
	if err != nil {
		return nil, err
	}
	data := make([]byte, size)
	n, err := r.directory.ReadAt(path, data, 0)
	if err != nil {
		return nil, err
	}
	if n != len(data) {
		return nil, io.ErrUnexpectedEOF
	}
	return data, nil
}

type coldFilesystemRelays struct {
	token        ttcvolume.RemoteToken
	clients      []*os.File
	done         []<-chan error
	memoryToken  ttcchunk.RemoteToken
	memoryClient *os.File
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
	if r.memoryClient != nil {
		_ = r.memoryClient.Close()
	}
}

type coldPackReference struct {
	Source string `json:"source"`
	Pack   string `json:"pack"`
}

type coldBranchManifest struct {
	Version    int       `json:"version"`
	Container  string    `json:"container"`
	Branch     string    `json:"branch"`
	Generation uint64    `json:"generation"`
	Kind       string    `json:"kind"`
	CreatedAt  time.Time `json:"created_at"`
	Parent     string    `json:"parent,omitempty"`
	// MemorySnapshot names the graph-native immutable memory origin. Its bulk
	// data and metadata live in snapshot-packs rather than the legacy cold pack.
	MemorySnapshot uint64              `json:"memory_snapshot,omitempty"`
	Packs          []coldPackReference `json:"packs"`
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
		for _, reference := range existing.Packs {
			id, err := ttcpack.ParseID(reference.Pack)
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
	manifest := coldBranchManifest{Version: 1, Container: info.ID, Branch: branch.ID, Generation: intent.Generation, Kind: intent.Kind, CreatedAt: time.Now().UTC(), Parent: intent.ParentManifest}
	spec, err := store.LoadSpec(info.ID)
	if err != nil {
		return ttcstorage.Result{}, err
	}
	repositories, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		return ttcstorage.Result{}, err
	}
	seen := make(map[string]struct{})
	for index, config := range repositories {
		path := filepath.Clean(config.Path)
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		repository, err := ttcvolume.OpenRepository(path, ttcvolume.CopyMode(config.CopyMode))
		if err != nil {
			return ttcstorage.Result{}, fmt.Errorf("opening active repository %q: %w", config.Source, err)
		}
		var inherited map[string]struct{}
		if intent.ParentManifest != "" {
			inherited, err = p.inheritedObjectIDs(intent.ParentManifest, config.Source)
			if err != nil {
				repository.Close()
				return ttcstorage.Result{}, err
			}
		}
		snapshotPath := filepath.Join(staging, fmt.Sprintf("repository-%04d", index))
		_, snapshotErr := repository.ExportSnapshotDelta(intent.FilesystemBranch, snapshotPath, inherited)
		closeErr := repository.Close()
		if snapshotErr != nil || closeErr != nil {
			return ttcstorage.Result{}, errors.Join(snapshotErr, closeErr)
		}
		pack, err := ttcpack.WriteDirectory(snapshotPath, filepath.Join(p.sharedRoot, "cold"))
		if err != nil {
			return ttcstorage.Result{}, err
		}
		manifest.Packs = append(manifest.Packs, coldPackReference{Source: config.Source, Pack: pack.String()})
	}
	if intent.Kind == "dehydrate" {
		memory := publicationMemoryControlPath(p.localRoot, intent.MemoryControl)
		if _, err := os.Stat(memory); err != nil {
			return ttcstorage.Result{}, fmt.Errorf("opening local dehydration image: %w", err)
		}
		pack, err := ttcpack.WriteDirectory(memory, filepath.Join(p.sharedRoot, "cold"))
		if err != nil {
			return ttcstorage.Result{}, err
		}
		manifest.Packs = append(manifest.Packs, coldPackReference{Source: "memory-control", Pack: pack.String()})
		if branch.SnapshotID != 0 && len(branch.MemoryRoots) != 0 {
			if p.graph == nil || p.graphData == nil || p.graphShared == nil {
				return ttcstorage.Result{}, errors.New("graph-native snapshot publication is unavailable")
			}
			snapshot := ttcsnapshot.SnapshotID(branch.SnapshotID)
			if err := p.graphShared.Publish(context.Background(), p.graph, snapshot, p.graphData); err != nil {
				return ttcstorage.Result{}, fmt.Errorf("publishing graph memory snapshot %d: %w", snapshot, err)
			}
			manifest.MemorySnapshot = branch.SnapshotID
		} else {
			chunks := filepath.Join(p.localRoot, "containers", info.ID, "memory", "chunks")
			repository, err := ttcchunk.OpenMemoryRepository(chunks)
			if err != nil {
				return ttcstorage.Result{}, err
			}
			memorySnapshot := filepath.Join(staging, "memory-repository")
			var inheritedMemory map[ttcchunk.ID]struct{}
			if intent.ParentManifest != "" {
				inheritedMemory, err = p.inheritedMemoryObjectIDs(intent.ParentManifest)
				if err != nil {
					repository.Close()
					return ttcstorage.Result{}, err
				}
			}
			exportErr := repository.ExportBranchDelta(intent.MemoryBranch, branch.FilesystemBranch, memorySnapshot, inheritedMemory)
			closeErr := repository.Close()
			if exportErr != nil || closeErr != nil {
				return ttcstorage.Result{}, errors.Join(exportErr, closeErr)
			}
			pack, err = ttcpack.WriteDirectory(memorySnapshot, filepath.Join(p.sharedRoot, "cold"))
			if err != nil {
				return ttcstorage.Result{}, err
			}
			manifest.Packs = append(manifest.Packs, coldPackReference{Source: "memory-chunks", Pack: pack.String()})
		}
	}
	sort.Slice(manifest.Packs, func(i, j int) bool { return manifest.Packs[i].Source < manifest.Packs[j].Source })
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return ttcstorage.Result{}, err
	}
	if err := writeDurableJSON(path, &manifest); err != nil {
		return ttcstorage.Result{}, err
	}
	return ttcstorage.Result{Manifest: path}, nil
}

func (p *branchPublisher) inheritedMemoryObjectIDs(manifestPath string) (map[ttcchunk.ID]struct{}, error) {
	result := make(map[ttcchunk.ID]struct{})
	seen := make(map[string]struct{})
	for manifestPath != "" {
		if err := validateColdManifestPath(p.sharedRoot, manifestPath); err != nil {
			return nil, err
		}
		if _, ok := seen[manifestPath]; ok {
			return nil, errors.New("cold manifest parent cycle")
		}
		seen[manifestPath] = struct{}{}
		manifest, err := readColdBranchManifest(manifestPath)
		if err != nil {
			return nil, err
		}
		for _, reference := range manifest.Packs {
			if reference.Source != "memory-chunks" {
				continue
			}
			id, err := ttcpack.ParseID(reference.Pack)
			if err != nil {
				return nil, err
			}
			file, index, err := ttcpack.OpenIndexed(filepath.Join(p.sharedRoot, "cold"), id)
			if err != nil {
				return nil, err
			}
			if err := file.Close(); err != nil {
				return nil, err
			}
			for _, entry := range index.Entries {
				parts := strings.Split(filepath.ToSlash(entry.Path), "/")
				if len(parts) != 3 || parts[0] != "chunks" || !strings.HasSuffix(parts[2], ".chunk") {
					continue
				}
				object, err := ttcchunk.ParseID(strings.TrimSuffix(parts[2], ".chunk"))
				if err != nil {
					return nil, fmt.Errorf("cold memory pack %s contains invalid chunk %q: %w", id, entry.Path, err)
				}
				result[object] = struct{}{}
			}
		}
		manifestPath = manifest.Parent
	}
	return result, nil
}

func (p *branchPublisher) inheritedObjectIDs(manifestPath, source string) (map[string]struct{}, error) {
	result := make(map[string]struct{})
	seen := make(map[string]struct{})
	for manifestPath != "" {
		if err := validateColdManifestPath(p.sharedRoot, manifestPath); err != nil {
			return nil, err
		}
		if _, ok := seen[manifestPath]; ok {
			return nil, errors.New("cold manifest parent cycle")
		}
		seen[manifestPath] = struct{}{}
		manifest, err := readColdBranchManifest(manifestPath)
		if err != nil {
			return nil, err
		}
		for _, reference := range manifest.Packs {
			if reference.Source != source {
				continue
			}
			id, err := ttcpack.ParseID(reference.Pack)
			if err != nil {
				return nil, err
			}
			file, index, err := ttcpack.OpenIndexed(filepath.Join(p.sharedRoot, "cold"), id)
			if err != nil {
				return nil, err
			}
			closeErr := file.Close()
			if closeErr != nil {
				return nil, closeErr
			}
			for _, entry := range index.Entries {
				if strings.HasPrefix(entry.Path, "objects/") {
					name := strings.TrimPrefix(entry.Path, "objects/")
					if !strings.ContainsRune(name, '/') && name != "" {
						result[name] = struct{}{}
					}
				}
			}
		}
		manifestPath = manifest.Parent
	}
	return result, nil
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
		_, branchErr := repository.Branch(intent.FilesystemBranch)
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
			record, ok := p.graph.Snapshot(ttcsnapshot.SnapshotID(branch.SnapshotID))
			if !ok || record.State != ttcsnapshot.SnapshotSealedVolatile {
				return false, nil
			}
		} else {
			repository, err := ttcchunk.OpenMemoryRepository(filepath.Join(p.localRoot, "containers", info.ID, "memory", "chunks"))
			if err != nil {
				return false, err
			}
			_, branchErr := repository.Branch(intent.MemoryBranch)
			closeErr := repository.Close()
			if errors.Is(branchErr, os.ErrNotExist) {
				return false, closeErr
			}
			if branchErr != nil || closeErr != nil {
				return false, errors.Join(branchErr, closeErr)
			}
		}
	}
	if p.authority != nil {
		lifecycle := "exited"
		if intent.Kind == "dehydrate" {
			lifecycle = "dehydrated"
		}
		if _, err := p.authority.EnsurePublicationReady(info.ID, intent.Branch, intent.Generation, lifecycle); err != nil {
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
		removeErr := repository.RemoveBranch(intent.FilesystemBranch)
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
		repository, err := ttcchunk.OpenMemoryRepository(filepath.Join(p.localRoot, "containers", info.ID, "memory", "chunks"))
		if err != nil {
			return err
		}
		removeErr := repository.DeleteBranch(intent.MemoryBranch)
		if errors.Is(removeErr, os.ErrNotExist) {
			removeErr = nil
		}
		_, gcErr := repository.CollectGarbage(ttcchunk.GCOptions{})
		closeErr := repository.Close()
		return errors.Join(removeErr, gcErr, closeErr, os.RemoveAll(publicationMemoryControlPath(p.localRoot, intent.MemoryControl)))
	}
	return nil
}

func publicationMemoryControlPath(localRoot, capture string) string {
	return filepath.Join(localRoot, "publication-captures", "memory-control", capture)
}

// snapshotImmutableTree seals the small process/control portion of a
// dehydration image independently of the user-visible local image. Regular
// files are hard-linked within TTC_LOCAL_ROOT and copied only if the backing
// filesystem rejects links. The sparse memory payload is rooted separately in
// ttcchunk, so this tree normally contains only metadata and control JSON.
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
			if err := p.authority.PublishSnapshotOrigin(p.nodeID, p.incarnation, branch.SnapshotID); err != nil {
				return fmt.Errorf("publishing snapshot origin %d in Raft: %w", branch.SnapshotID, err)
			}
			if p.graph != nil {
				if err := p.graph.MarkShared(ttcsnapshot.SnapshotID(branch.SnapshotID)); err != nil {
					return fmt.Errorf("marking snapshot origin %d shared: %w", branch.SnapshotID, err)
				}
			}
		}
		digest, err := fileSHA256Digest(result.Manifest)
		if err != nil {
			return fmt.Errorf("digesting cold manifest: %w", err)
		}
		roots := cluster.DurableRoots{FilesystemRoot: digest}
		if intent.Kind == "dehydrate" {
			if branch.SnapshotID != 0 && len(branch.MemoryRoots) != 0 {
				// Snapshot identity, not a runtime checksum, is the protocol root.
				roots.MemorySnapshot = branch.SnapshotID
			} else {
				roots.MemoryRoot = digest
			}
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
	return manifest, nil
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
	packBySource := make(map[string]ttcpack.ID, len(manifest.Packs))
	for _, reference := range manifest.Packs {
		id, err := ttcpack.ParseID(reference.Pack)
		if err != nil {
			return err
		}
		packBySource[reference.Source] = id
	}
	if err := os.MkdirAll(filepath.Join(b.activeDataRoot(), "containers", info.ID), 0700); err != nil {
		return err
	}
	staging := filepath.Join(b.activeDataRoot(), "materialization-staging", fmt.Sprintf("%s-%s-%016x", info.ID, hex.EncodeToString([]byte(branch.ID)), branch.StorageGeneration))
	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	if err := os.MkdirAll(staging, 0700); err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	spec, err := store.LoadSpec(info.ID)
	if err != nil {
		return err
	}
	repositories, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		return err
	}
	manager, err := ttcvolume.OpenManager(b.activeDataRoot())
	if err != nil {
		return err
	}
	for index := range repositories {
		repository := &repositories[index]
		_, ok := packBySource[repository.Source]
		if !ok {
			return fmt.Errorf("cold manifest omits repository %q", repository.Source)
		}
		_, statErr := os.Stat(repository.Path)
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		snapshot := filepath.Join(staging, fmt.Sprintf("repository-%04d", index))
		if err := b.materializeColdSourceMetadata(manifest, repository.Source, snapshot); err != nil {
			return fmt.Errorf("materializing repository %q: %w", repository.Source, err)
		}
		if errors.Is(statErr, os.ErrNotExist) {
			if err := os.MkdirAll(filepath.Dir(repository.Path), 0700); err != nil {
				return err
			}
			imported, snapshotInfo, err := manager.ImportPrivateRemoteSnapshot(repository.Path, fmt.Sprintf("cold-%d", index), snapshot, branch.FilesystemBranch)
			if err != nil {
				return err
			}
			repository.RestoreID = snapshotInfo.RepositoryID
			repository.CopyMode = string(manager.Capability().Mode)
			if err := imported.Close(); err != nil {
				return err
			}
		} else {
			opened, err := ttcvolume.OpenRepository(repository.Path, ttcvolume.CopyMode(repository.CopyMode))
			if err != nil {
				return err
			}
			snapshotInfo, importErr := opened.ImportRemoteBranchSnapshot(snapshot, branch.FilesystemBranch)
			closeErr := opened.Close()
			if importErr != nil || closeErr != nil {
				return errors.Join(importErr, closeErr)
			}
			repository.RestoreID = snapshotInfo.RepositoryID
		}
	}
	if err := store.WriteActiveRepositoryMap(info.ID, repositories); err != nil {
		return err
	}
	controlPack, ok := packBySource["memory-control"]
	if !ok {
		return errors.New("cold manifest omits dehydration control state")
	}
	if _, err := os.Stat(localImage); errors.Is(err, os.ErrNotExist) {
		if err := ttcpack.Materialize(filepath.Join(b.dataRoot, "cold"), controlPack, localImage); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	memoryPack, ok := packBySource["memory-chunks"]
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
	if !ok {
		return errors.New("cold manifest omits sparse memory chunks")
	}
	memorySnapshot := filepath.Join(staging, "memory-repository")
	if err := ttcpack.MaterializeMetadata(filepath.Join(b.dataRoot, "cold"), memoryPack, memorySnapshot, "chunks"); err != nil {
		return err
	}
	memoryDestination := filepath.Join(b.activeDataRoot(), "containers", info.ID, "memory", "chunks")
	if _, err := os.Stat(memoryDestination); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(memoryDestination), 0700); err != nil {
			return err
		}
		if err := os.Rename(memorySnapshot, memoryDestination); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		sourceRepository, err := ttcchunk.OpenMemoryRepository(memorySnapshot)
		if err != nil {
			return err
		}
		destinationRepository, err := ttcchunk.OpenMemoryRepository(memoryDestination)
		if err != nil {
			sourceRepository.Close()
			return err
		}
		manifestID, branchErr := sourceRepository.Branch(branch.FilesystemBranch)
		if branchErr == nil {
			branchErr = destinationRepository.ImportRemoteManifest(sourceRepository, manifestID, branch.FilesystemBranch)
		}
		closeErr := errors.Join(sourceRepository.Close(), destinationRepository.Close())
		if branchErr != nil || closeErr != nil {
			return errors.Join(branchErr, closeErr)
		}
	}
	return store.UpdateBranchStorage(info.ID, branch.ID, branch.StorageGeneration, "materializing", "", time.Time{})
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
	packBySource := make(map[string]ttcpack.ID, len(manifest.Packs))
	for _, reference := range manifest.Packs {
		id, err := ttcpack.ParseID(reference.Pack)
		if err != nil {
			return err
		}
		packBySource[reference.Source] = id
	}
	if err := os.MkdirAll(filepath.Join(b.activeDataRoot(), "containers", info.ID), 0700); err != nil {
		return err
	}
	staging := filepath.Join(b.activeDataRoot(), "materialization-staging", fmt.Sprintf("fs-%s-%s-%016x", info.ID, hex.EncodeToString([]byte(branch.ID)), branch.StorageGeneration))
	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	if err := os.MkdirAll(staging, 0700); err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	spec, err := store.LoadSpec(info.ID)
	if err != nil {
		return err
	}
	repositories, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		return err
	}
	manager, err := ttcvolume.OpenManager(b.activeDataRoot())
	if err != nil {
		return err
	}
	for index := range repositories {
		repository := &repositories[index]
		if _, ok := packBySource[repository.Source]; !ok {
			return fmt.Errorf("cold manifest omits repository %q", repository.Source)
		}
		snapshot := filepath.Join(staging, fmt.Sprintf("repository-%04d", index))
		if err := b.materializeColdSourceMetadata(manifest, repository.Source, snapshot); err != nil {
			return fmt.Errorf("materializing repository %q: %w", repository.Source, err)
		}
		if err := os.MkdirAll(filepath.Dir(repository.Path), 0700); err != nil {
			return err
		}
		if _, statErr := os.Stat(filepath.Join(repository.Path, "volume.json")); errors.Is(statErr, os.ErrNotExist) {
			imported, snapshotInfo, err := manager.ImportPrivateRemoteSnapshot(repository.Path, fmt.Sprintf("cold-fs-%d", index), snapshot, branch.FilesystemBranch)
			if err != nil {
				return err
			}
			repository.RestoreID = snapshotInfo.RepositoryID
			repository.CopyMode = string(manager.Capability().Mode)
			if err := imported.Close(); err != nil {
				return err
			}
		} else if statErr != nil {
			return statErr
		} else {
			opened, err := ttcvolume.OpenRepository(repository.Path, ttcvolume.CopyMode(repository.CopyMode))
			if err != nil {
				return err
			}
			snapshotInfo, importErr := opened.ImportRemoteBranchSnapshot(snapshot, branch.FilesystemBranch)
			closeErr := opened.Close()
			if importErr != nil || closeErr != nil {
				return errors.Join(importErr, closeErr)
			}
			repository.RestoreID = snapshotInfo.RepositoryID
		}
	}
	return store.WriteActiveRepositoryMap(info.ID, repositories)
}

func (b *runtimeBackend) materializeColdSourceMetadata(manifest coldBranchManifest, source, destination string) error {
	for _, reference := range manifest.Packs {
		if reference.Source != source {
			continue
		}
		id, err := ttcpack.ParseID(reference.Pack)
		if err != nil {
			return err
		}
		return ttcpack.MaterializeMetadata(filepath.Join(b.dataRoot, "cold"), id, destination, "objects")
	}
	return fmt.Errorf("cold manifest omits source %q", source)
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
		if repository.RestoreID != "" {
			ids[repository.Source] = repository.RestoreID
		}
	}
	return ids, nil
}

func (b *runtimeBackend) coldPackChain(manifest coldBranchManifest, source string) ([]ttcpack.ID, error) {
	var ids []ttcpack.ID
	seen := make(map[string]struct{})
	current := manifest
	for {
		for _, reference := range current.Packs {
			if reference.Source != source {
				continue
			}
			id, err := ttcpack.ParseID(reference.Pack)
			if err != nil {
				return nil, err
			}
			ids = append(ids, id)
			break
		}
		if current.Parent == "" {
			break
		}
		if err := validateColdManifestPath(b.dataRoot, current.Parent); err != nil {
			return nil, err
		}
		if _, ok := seen[current.Parent]; ok {
			return nil, errors.New("cold manifest parent cycle")
		}
		seen[current.Parent] = struct{}{}
		parent, err := readColdBranchManifest(current.Parent)
		if err != nil {
			return nil, err
		}
		current = parent
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("cold manifest chain omits source %q", source)
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
	for _, config := range repositories {
		ids, err := b.coldPackChain(manifest, config.Source)
		if err != nil {
			return fail(err)
		}
		directory, err := ttcpack.OpenDirectory(filepath.Join(b.dataRoot, "cold"), ids)
		if err != nil {
			return fail(err)
		}
		repository, err := ttcvolume.OpenRepository(config.Path, ttcvolume.CopyMode(config.CopyMode))
		if err != nil {
			directory.Close()
			return fail(err)
		}
		objects, objectErr := repository.BranchObjects(branch.FilesystemBranch)
		closeErr := repository.Close()
		if objectErr != nil || closeErr != nil {
			directory.Close()
			return fail(errors.Join(objectErr, closeErr))
		}
		fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
		if err != nil {
			directory.Close()
			return fail(err)
		}
		server := os.NewFile(uintptr(fds[0]), "ttc-cold-pack-server")
		client := os.NewFile(uintptr(fds[1]), "ttc-cold-pack-client")
		done := make(chan error, 1)
		key := ttcvolume.RemoteSourceKey(sha256.Sum256([]byte(config.Source)))
		go func() {
			err := ttcvolume.ServeRemoteObjects(server, relays.token, []ttcvolume.RemoteObjectSource{{
				Key: key, Reader: coldPackObjectReader{directory: directory}, Objects: objects,
			}})
			b.reportColdRelayError("filesystem", err)
			done <- err
			_ = directory.Close()
			close(done)
		}()
		relays.clients = append(relays.clients, client)
		relays.done = append(relays.done, done)
	}
	if manifest.MemorySnapshot != 0 {
		// Graph-native memory is resolved independently through the snapshot
		// origin resolver. Only legacy dehydration images need a ttcchunk relay.
		return relays, nil
	}
	if ids, err := b.coldPackChain(manifest, "memory-chunks"); err == nil {
		directory, err := ttcpack.OpenDirectory(filepath.Join(b.dataRoot, "cold"), ids)
		if err != nil {
			return fail(err)
		}
		repository, err := ttcchunk.OpenMemoryRepository(filepath.Join(b.activeDataRoot(), "containers", info.ID, "memory", "chunks"))
		if err != nil {
			directory.Close()
			return fail(err)
		}
		manifestID, manifestErr := repository.Branch(branch.FilesystemBranch)
		memoryManifest, loadErr := repository.LoadManifest(manifestID)
		closeErr := repository.Close()
		if manifestErr != nil || loadErr != nil || closeErr != nil {
			directory.Close()
			return fail(errors.Join(manifestErr, loadErr, closeErr))
		}
		allowed := make(map[ttcchunk.ID]struct{})
		for _, run := range memoryManifest.Runs {
			allowed[run.Object] = struct{}{}
		}
		if _, err := rand.Read(relays.memoryToken[:]); err != nil {
			directory.Close()
			return fail(err)
		}
		fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
		if err != nil {
			directory.Close()
			return fail(err)
		}
		server := os.NewFile(uintptr(fds[0]), "ttc-cold-memory-server")
		relays.memoryClient = os.NewFile(uintptr(fds[1]), "ttc-cold-memory-client")
		done := make(chan error, 1)
		go func() {
			err := ttcchunk.ServeRemoteChunks(server, relays.memoryToken, coldPackMemoryReader{directory: directory}, allowed)
			b.reportColdRelayError("memory", err)
			done <- err
			_ = directory.Close()
			close(done)
		}()
		relays.done = append(relays.done, done)
	} else {
		return fail(err)
	}
	return relays, nil
}

func (b *runtimeBackend) materializeColdSource(manifest coldBranchManifest, source, destination string) error {
	current := manifest
	first := true
	seen := make(map[string]struct{})
	for {
		var found *coldPackReference
		for i := range current.Packs {
			if current.Packs[i].Source == source {
				found = &current.Packs[i]
				break
			}
		}
		if found != nil {
			id, err := ttcpack.ParseID(found.Pack)
			if err != nil {
				return err
			}
			if first {
				err = ttcpack.Materialize(filepath.Join(b.dataRoot, "cold"), id, destination)
			} else {
				err = ttcpack.MaterializeMissing(filepath.Join(b.dataRoot, "cold"), id, destination)
			}
			if err != nil {
				return err
			}
			first = false
		}
		if current.Parent == "" {
			break
		}
		if err := validateColdManifestPath(b.dataRoot, current.Parent); err != nil {
			return err
		}
		if _, ok := seen[current.Parent]; ok {
			return errors.New("cold manifest parent cycle")
		}
		seen[current.Parent] = struct{}{}
		parent, err := readColdBranchManifest(current.Parent)
		if err != nil {
			return err
		}
		current = parent
	}
	if first {
		return fmt.Errorf("cold manifest chain omits source %q", source)
	}
	return nil
}

// collectColdStorage traces every authoritative and pending manifest before
// reclaiming immutable packs. A grace period protects a publisher between
// writing its pack and atomically committing its manifest.
func collectColdStorage(sharedRoot, localRoot string, publications *ttcstorage.Queue) error {
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
			if intent.ParentManifest != "" {
				queue = append(queue, intent.ParentManifest)
			}
		}
	}
	manifests := make(map[string]struct{})
	packs := make(map[ttcpack.ID]struct{})
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
		if manifest.Parent != "" {
			queue = append(queue, manifest.Parent)
		}
		for _, reference := range manifest.Packs {
			id, err := ttcpack.ParseID(reference.Pack)
			if err != nil {
				return err
			}
			packs[id] = struct{}{}
		}
	}
	grace := time.Now().Add(-10 * time.Minute)
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
		if info.ModTime().After(grace) {
			return nil
		}
		return os.Remove(path)
	}); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, err = ttcpack.Collect(filepath.Join(sharedRoot, "cold"), packs, grace.UnixNano())
	return err
}
