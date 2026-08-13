// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kernel

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/devutil"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/inet"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	sentrytime "gvisor.dev/gvisor/pkg/sentry/time"
	"gvisor.dev/gvisor/pkg/sentry/ttcchunk"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/state"
)

// DehydrationCapture is a metadata-only saved Kernel whose page ownership is
// retained by compact range reservations. The MemoryFiles remain the source of
// truth, so restoring this image preserves page sharing with live siblings.
//
// Close must be called if the capture is discarded instead of restored.
type DehydrationCapture struct {
	KernelMetadata      []byte
	MemoryFile          *pgalloc.MemoryFile
	PrivateMemoryFiles  map[checkpoint.ResourceID]*pgalloc.MemoryFile
	FilesystemResources vfs.LiveForkFilesystemResources
	MemoryImages        []DurableDehydrationMemory
	SparseBindings      []pgalloc.CapturedPageBinding
	SparseSnapshot      *pgalloc.RemoteSnapshot
	SparseManifest      ttcchunk.ManifestID

	reservations []*pgalloc.RemoteSnapshot
	emptyOwners  []*pgalloc.MemoryFile
	consumed     bool
	metadataFile *os.File
}

// DurableDehydrationMemory describes one MemoryFile in a process-independent
// dehydration image. The first entry always describes application memory;
// remaining entries are ordered by ResourceID and describe private tmpfs-like
// MemoryFiles. Snapshot is process-local and omitted from serialized metadata.
type DurableDehydrationMemory struct {
	ResourceID        checkpoint.ResourceID     `json:"resource_id"`
	AllocatorMetadata []byte                    `json:"allocator_metadata"`
	LoadRanges        []pgalloc.RemoteLoadRange `json:"load_ranges"`
	StreamBytes       uint64                    `json:"stream_bytes"`
	Snapshot          *pgalloc.RemoteSnapshot   `json:"-"`
}

// DurableDehydrationMetadata is sufficient to reconstruct a Kernel in a new
// Sentry when paired with one packed snapshot file per Memories entry.
type DurableDehydrationMetadata struct {
	Version        int                        `json:"version"`
	KernelMetadata []byte                     `json:"kernel_metadata"`
	Memories       []DurableDehydrationMemory `json:"memories"`
	SparseManifest ttcchunk.ManifestID        `json:"sparse_manifest,omitempty"`
}

const durableDehydrationVersion = 1

// DehydrationCaptureOptions controls a local hibernation capture.
type DehydrationCaptureOptions struct {
	BranchID     string
	MetadataFile *os.File
}

// DehydrationLoadOptions controls reconstruction of a dehydrated Kernel.
type DehydrationLoadOptions struct {
	Clocks           sentrytime.Clocks
	BranchID         string
	RestoreOptions   vfs.CompleteRestoreOptions
	ConfigureNetwork func(*Kernel, inet.Stack) error
}

// DurableDehydrationLoadMemory pairs one newly-created target MemoryFile with
// the packed page stream described by DurableDehydrationMetadata.Memories at
// the same index. LoadDurableDehydration consumes both fields.
type DurableDehydrationLoadMemory struct {
	MemoryFile *pgalloc.MemoryFile
	Reader     stateio.AsyncReader
}

// DurableDehydrationLoadOptions controls reconstruction in a new Sentry.
type DurableDehydrationLoadOptions struct {
	Clocks                     sentrytime.Clocks
	BranchID                   string
	RestoreOptions             vfs.CompleteRestoreOptions
	FilesystemResources        vfs.LiveForkFilesystemResources
	RestoreFilesystemResources vfs.RestoreFilesystemResources
	RestoreFilesystemFDMap     map[checkpoint.ResourceID]int
	ConfigureNetwork           func(*Kernel, inet.Stack) error
	MaxReadBytes               uint64
	MaxParallel                int
	OwnSparseRepository        bool
	// SparseBranch names the durable retention root for per-chunk reclamation.
	SparseBranch string
}

// WithDehydrationFilesystemResources exposes an active branch's process-local
// filesystem handles while a durable peer is reconstructed. targetBranch is
// already present in every TTC repository, so this never creates or owns a
// parent/child filesystem relation.
func (k *Kernel) WithDehydrationFilesystemResources(ctx context.Context, targetBranch string, load func(vfs.LiveForkFilesystemResources) error) error {
	if !k.IsPaused() || targetBranch == "" || load == nil {
		return errors.New("dehydration filesystem restore requires a paused source, target branch, and loader")
	}
	return k.quiescePausedAnd(ctx, func() error {
		if err := k.invalidateUnsavableMappings(ctx); err != nil {
			return fmt.Errorf("invalidating mappings for dehydration peer restore: %w", err)
		}
		resources := make(vfs.LiveForkFilesystemResources)
		private := make(map[checkpoint.ResourceID]*pgalloc.MemoryFile)
		saveCtx := context.WithValues(k.SupervisorContext(), map[any]any{
			pgalloc.CtxMemoryFile:              k.mf,
			pgalloc.CtxMemoryFileMap:           private,
			pgalloc.CtxLiveForkSource:          true,
			vfs.CtxLiveForkFilesystemResources: resources,
			vfs.CtxLiveForkChildBranchID:       targetBranch,
			vfs.CtxLiveForkBranchesPrepared:    true,
		})
		if err := k.vfs.PrepareSave(saveCtx); err != nil {
			return fmt.Errorf("preparing filesystem resources for dehydration peer restore: %w", err)
		}
		defer k.vfs.BeforeResume(saveCtx)
		return load(resources)
	})
}

// Close releases page and owner reservations held by d.
func (d *DehydrationCapture) Close() error {
	if d == nil || d.consumed {
		return nil
	}
	d.consumed = true
	var result error
	for _, reservation := range d.reservations {
		result = errors.Join(result, reservation.Close())
	}
	if d.SparseSnapshot != nil {
		result = errors.Join(result, d.SparseSnapshot.Close())
		d.SparseSnapshot = nil
	}
	for _, mf := range d.emptyOwners {
		mf.Destroy()
	}
	d.reservations = nil
	d.emptyOwners = nil
	if d.metadataFile != nil {
		result = errors.Join(result, d.metadataFile.Close())
		d.metadataFile = nil
	}
	return result
}

var dehydrationMetadataMagic = [8]byte{'T', 'T', 'C', 'D', 'H', 'Y', '0', '2'}

func writeDehydrationMetadata(file *os.File, image DurableDehydrationMetadata) error {
	metadata, err := json.Marshal(image)
	if err != nil {
		return err
	}
	if err := file.Truncate(0); err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	var header [48]byte
	copy(header[:8], dehydrationMetadataMagic[:])
	binary.LittleEndian.PutUint64(header[8:16], uint64(len(metadata)))
	hash := sha256.Sum256(metadata)
	copy(header[16:], hash[:])
	if _, err := file.Write(header[:]); err != nil {
		return err
	}
	if _, err := file.Write(metadata); err != nil {
		return err
	}
	return file.Sync()
}

// ReadDurableDehydrationMetadata verifies and decodes a durable dehydration
// metadata file. Snapshot fields are always nil after decoding.
func ReadDurableDehydrationMetadata(file *os.File) (DurableDehydrationMetadata, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return DurableDehydrationMetadata{}, err
	}
	var header [48]byte
	if _, err := io.ReadFull(file, header[:]); err != nil {
		return DurableDehydrationMetadata{}, err
	}
	if !bytes.Equal(header[:8], dehydrationMetadataMagic[:]) {
		return DurableDehydrationMetadata{}, errors.New("invalid dehydration metadata magic")
	}
	length := binary.LittleEndian.Uint64(header[8:16])
	if length > uint64(^uint(0)>>1) {
		return DurableDehydrationMetadata{}, errors.New("dehydration metadata is too large")
	}
	metadata := make([]byte, int(length))
	if _, err := io.ReadFull(file, metadata); err != nil {
		return DurableDehydrationMetadata{}, err
	}
	hash := sha256.Sum256(metadata)
	if !bytes.Equal(header[16:], hash[:]) {
		return DurableDehydrationMetadata{}, errors.New("dehydration metadata checksum mismatch")
	}
	var image DurableDehydrationMetadata
	if err := json.Unmarshal(metadata, &image); err != nil {
		return DurableDehydrationMetadata{}, fmt.Errorf("decoding dehydration metadata: %w", err)
	}
	if image.Version != durableDehydrationVersion || len(image.KernelMetadata) == 0 || len(image.Memories) == 0 {
		return DurableDehydrationMetadata{}, fmt.Errorf("unsupported or incomplete dehydration metadata version %d", image.Version)
	}
	for i := range image.Memories {
		memory := &image.Memories[i]
		if i == 0 && memory.ResourceID.Ok() {
			return DurableDehydrationMetadata{}, errors.New("application dehydration memory unexpectedly has a resource ID")
		}
		if i != 0 && !memory.ResourceID.Ok() {
			return DurableDehydrationMetadata{}, fmt.Errorf("private dehydration memory %d has no resource ID", i)
		}
	}
	return image, nil
}

// SyncAndDropCache persists and evicts pages private to this dehydration
// capture. Shared CoW pages remain resident while an active branch uses them.
func (d *DehydrationCapture) SyncAndDropCache() error {
	if d == nil || d.consumed {
		return errors.New("dehydration capture is unavailable")
	}
	return SyncAndDropDehydrationCache([]*DehydrationCapture{d})
}

// SyncAndDropDehydrationCache persists and evicts pages referenced only by
// the supplied dehydration captures, including CoW pages shared by two or
// more dehydrated siblings.
func SyncAndDropDehydrationCache(captures []*DehydrationCapture) error {
	var reservations []*pgalloc.RemoteSnapshot
	for _, capture := range captures {
		if capture == nil || capture.consumed {
			return errors.New("dehydration capture is unavailable")
		}
		reservations = append(reservations, capture.reservations...)
	}
	return pgalloc.SyncAndDropSnapshotCache(reservations)
}

func (d *DehydrationCapture) reserveMemoryFile(ctx context.Context, resourceID checkpoint.ResourceID, mf *pgalloc.MemoryFile) error {
	allocatorMetadata, regions, loads, err := pgalloc.ExportMemoryFileMetadataForRemote(ctx, mf)
	if err != nil {
		return err
	}
	image := DurableDehydrationMemory{
		ResourceID: resourceID, AllocatorMetadata: allocatorMetadata, LoadRanges: loads,
	}
	if len(regions) == 0 {
		mf.IncOwner()
		d.emptyOwners = append(d.emptyOwners, mf)
		d.MemoryImages = append(d.MemoryImages, image)
		return nil
	}
	reservation, err := pgalloc.NewRemoteSnapshot(regions)
	if err != nil {
		return err
	}
	image.StreamBytes = reservation.StreamBytes()
	image.Snapshot = reservation
	d.reservations = append(d.reservations, reservation)
	d.MemoryImages = append(d.MemoryImages, image)
	return nil
}

// ExportPackedSnapshots writes one immutable packed page stream per memory
// image in deterministic metadata order.
func (d *DehydrationCapture) ExportPackedSnapshots(files []*os.File, allowCopy bool) error {
	if d == nil || d.consumed || len(files) != len(d.MemoryImages) {
		return fmt.Errorf("dehydration snapshot export has %d files, want %d", len(files), len(d.MemoryImages))
	}
	for i, memory := range d.MemoryImages {
		if memory.Snapshot == nil {
			if err := files[i].Truncate(0); err != nil {
				return err
			}
			if err := files[i].Sync(); err != nil {
				return err
			}
			continue
		}
		if err := memory.Snapshot.ExportPackedSnapshot(files[i], allowCopy); err != nil {
			return fmt.Errorf("exporting dehydration memory %d (%s): %w", i, memory.ResourceID, err)
		}
	}
	return nil
}

// DurableMetadata returns the process-independent description represented by
// d. Process-local snapshot pointers are removed.
func (d *DehydrationCapture) DurableMetadata() DurableDehydrationMetadata {
	memories := make([]DurableDehydrationMemory, len(d.MemoryImages))
	for i, memory := range d.MemoryImages {
		memory.Snapshot = nil
		memories[i] = memory
	}
	return DurableDehydrationMetadata{
		Version: durableDehydrationVersion, KernelMetadata: append([]byte(nil), d.KernelMetadata...), Memories: memories,
		SparseManifest: d.SparseManifest,
	}
}

// PublishSparse persists the exact branch-reachable memory graph as immutable
// 1 MiB objects. The coordinator installs the returned manifest as the durable
// branch root after it has committed the corresponding control metadata.
func (d *DehydrationCapture) PublishSparse(repository *ttcchunk.MemoryRepository) (ttcchunk.ManifestID, error) {
	if d == nil || d.consumed || d.SparseSnapshot == nil || repository == nil {
		return ttcchunk.ManifestID{}, errors.New("sparse dehydration publication is unavailable")
	}
	objects, bindings, err := ttcchunk.ReadSnapshot(d.SparseSnapshot, d.SparseBindings)
	if err != nil {
		return ttcchunk.ManifestID{}, err
	}
	manifest, err := repository.Publish(objects, bindings)
	if err != nil {
		return ttcchunk.ManifestID{}, err
	}
	if err := d.SparseSnapshot.Close(); err != nil {
		return ttcchunk.ManifestID{}, err
	}
	d.SparseSnapshot = nil
	d.SparseManifest = manifest
	if d.metadataFile != nil {
		if err := writeDehydrationMetadata(d.metadataFile, d.DurableMetadata()); err != nil {
			return ttcchunk.ManifestID{}, fmt.Errorf("updating sparse dehydration metadata: %w", err)
		}
	}
	return manifest, nil
}

// CaptureDehydration serializes Kernel metadata and pins each used allocator
// range once. Page bytes remain in their existing MemoryFiles and are not
// copied into the metadata stream.
//
// Preconditions: k must remain paused throughout this call.
func (k *Kernel) CaptureDehydration(ctx context.Context, opts DehydrationCaptureOptions) (*DehydrationCapture, error) {
	if !k.IsPaused() {
		return nil, errors.New("dehydration capture requires a paused kernel")
	}
	if opts.BranchID == "" {
		return nil, errors.New("dehydration capture requires a filesystem branch ID")
	}
	k.tasks.waitForExternalStop()
	k.ReceiveTaskStates()
	image := &DehydrationCapture{
		MemoryFile:          k.mf,
		PrivateMemoryFiles:  make(map[checkpoint.ResourceID]*pgalloc.MemoryFile),
		FilesystemResources: make(vfs.LiveForkFilesystemResources),
		metadataFile:        opts.MetadataFile,
	}
	err := k.quiescePausedAnd(ctx, func() error {
		if rootNS := k.rootNetworkNamespace; rootNS != nil && rootNS.Stack() != nil {
			stack := rootNS.Stack()
			stack.SetRemoveConf(false)
			stack.Pause()
			defer stack.Resume()
		}
		if err := k.invalidateUnsavableMappings(ctx); err != nil {
			return fmt.Errorf("invalidating unsavable mappings for dehydration: %w", err)
		}
		memoryCapture := pgalloc.NewMemoryRangeCapture()
		saveCtx := context.WithValues(k.SupervisorContext(), map[any]any{
			pgalloc.CtxMemoryFile:              k.mf,
			pgalloc.CtxMemoryFileMap:           image.PrivateMemoryFiles,
			pgalloc.CtxMemoryRangeCapture:      memoryCapture,
			pgalloc.CtxLiveForkSource:          true,
			vfs.CtxLiveForkFilesystemResources: image.FilesystemResources,
			vfs.CtxLiveForkChildBranchID:       opts.BranchID,
			vfs.CtxLiveForkBranchesPrepared:    true,
		})
		if err := k.vfs.PrepareSave(saveCtx); err != nil {
			return fmt.Errorf("preparing VFS for dehydration: %w", err)
		}
		defer k.vfs.BeforeResume(saveCtx)
		if err := k.prepareMemoryManagersForDistributedFork(memoryCapture); err != nil {
			return fmt.Errorf("capturing dehydration memory graph: %w", err)
		}
		if err := k.captureFileDescriptionMemory(memoryCapture); err != nil {
			return fmt.Errorf("capturing dehydration file memory graph: %w", err)
		}
		if err := k.captureSharedMemoryRanges(ctx, memoryCapture); err != nil {
			return fmt.Errorf("capturing dehydration SysV SHM graph: %w", err)
		}
		if err := k.captureFilesystemMemory(ctx, memoryCapture); err != nil {
			return fmt.Errorf("capturing dehydration filesystem memory graph: %w", err)
		}
		if err := k.captureKernelOwnedMemory(memoryCapture); err != nil {
			return fmt.Errorf("capturing dehydration kernel memory graph: %w", err)
		}
		k.mf.MarkSavable()
		for _, mf := range image.PrivateMemoryFiles {
			mf.MarkSavable()
		}
		if err := image.reserveMemoryFile(ctx, checkpoint.ResourceID{}, k.mf); err != nil {
			return fmt.Errorf("reserving application memory: %w", err)
		}
		privateIDs := make([]checkpoint.ResourceID, 0, len(image.PrivateMemoryFiles))
		for id := range image.PrivateMemoryFiles {
			privateIDs = append(privateIDs, id)
		}
		sort.Slice(privateIDs, func(i, j int) bool { return privateIDs[i].String() < privateIDs[j].String() })
		for _, id := range privateIDs {
			if err := image.reserveMemoryFile(ctx, id, image.PrivateMemoryFiles[id]); err != nil {
				return fmt.Errorf("reserving private memory file %q: %w", id, err)
			}
		}
		var metadata bytes.Buffer
		buffered := bufio.NewWriterSize(&metadata, 64<<10)
		if _, err := state.Save(saveCtx, buffered, k); err != nil {
			return fmt.Errorf("saving dehydrated Kernel metadata: %w", err)
		}
		if err := buffered.Flush(); err != nil {
			return fmt.Errorf("flushing dehydrated Kernel metadata: %w", err)
		}
		layout, err := memoryCapture.SnapshotLayout()
		if err != nil {
			return fmt.Errorf("resolving dehydration memory graph: %w", err)
		}
		sparseSnapshot, err := pgalloc.NewRemoteSnapshot(layout.Regions)
		if err != nil {
			return fmt.Errorf("reserving sparse dehydration memory: %w", err)
		}
		image.SparseBindings = layout.Bindings
		image.SparseSnapshot = sparseSnapshot
		image.KernelMetadata = metadata.Bytes()
		if image.metadataFile != nil {
			if err := writeDehydrationMetadata(image.metadataFile, image.DurableMetadata()); err != nil {
				return fmt.Errorf("writing dehydration metadata: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		_ = image.Close()
		return nil, err
	}
	for _, resource := range image.FilesystemResources {
		preserver, ok := resource.(vfs.DehydrationFilesystemResource)
		if !ok {
			continue
		}
		preserver.PreserveForDehydration()
	}
	return image, nil
}

// LoadDehydration reconstructs a paused Kernel from image. It transfers the
// image's page reservations into ordinary Kernel references only after the
// complete task and filesystem graph has been restored successfully.
func LoadDehydration(ctx context.Context, p platform.Platform, image *DehydrationCapture, opts DehydrationLoadOptions) (*Kernel, error) {
	if p == nil || image == nil || image.consumed || image.MemoryFile == nil || opts.Clocks == nil || opts.BranchID == "" {
		return nil, errors.New("dehydration load is missing required state")
	}
	allMemoryFiles := make([]*pgalloc.MemoryFile, 0, len(image.PrivateMemoryFiles)+1)
	allMemoryFiles = append(allMemoryFiles, image.MemoryFile)
	for _, mf := range image.PrivateMemoryFiles {
		allMemoryFiles = append(allMemoryFiles, mf)
	}
	for _, mf := range allMemoryFiles {
		mf.IncOwner()
	}
	ownersTransferred := false
	defer func() {
		if !ownersTransferred {
			for _, mf := range allMemoryFiles {
				mf.Destroy()
			}
		}
	}()

	child := &Kernel{Platform: p}
	child.SetMemoryFile(image.MemoryFile)
	child.initRuntimeFields()
	loadCtx := context.WithValues(child.SupervisorContext(), map[any]any{
		pgalloc.CtxMemoryFile:              image.MemoryFile,
		pgalloc.CtxMemoryFileMap:           image.PrivateMemoryFiles,
		pgalloc.CtxLiveForkClone:           true,
		vfs.CtxLiveForkFilesystemResources: image.FilesystemResources,
		vfs.CtxLiveForkChildBranchID:       opts.BranchID,
		devutil.CtxDevGoferClientProvider:  child,
	})
	metadata := image.KernelMetadata
	if image.metadataFile != nil {
		durable, err := ReadDurableDehydrationMetadata(image.metadataFile)
		if err != nil {
			return nil, fmt.Errorf("reading dehydrated Kernel metadata: %w", err)
		}
		metadata = durable.KernelMetadata
	}
	if _, err := state.Load(loadCtx, bufio.NewReaderSize(bytes.NewReader(metadata), 64<<10), child); err != nil {
		return nil, fmt.Errorf("loading dehydrated Kernel metadata: %w", err)
	}
	ownersTransferred = true
	// The saved VVAR page may have been updated between metadata capture and
	// final Kernel teardown, and may also remain shared with a live sibling.
	// Rebind it before SetClocks resets the restored sequence counter.
	if err := child.privatizeLiveForkVDSOParamPage(); err != nil {
		child.Release()
		return nil, err
	}
	child.Timekeeper().SetClocks(opts.Clocks, child.vdsoParams)
	if rootNS := child.rootNetworkNamespace; rootNS != nil && rootNS.Stack() != nil {
		if opts.ConfigureNetwork == nil {
			child.Release()
			return nil, errors.New("dehydrated network stack requires a configurator")
		}
		rootNS.Stack().ResetConfig()
		if err := opts.ConfigureNetwork(child, rootNS.Stack()); err != nil {
			child.Release()
			return nil, fmt.Errorf("configuring rehydrated network: %w", err)
		}
		rootNS.Stack().Restore()
	}
	if err := child.vfs.CompleteRestore(loadCtx, &opts.RestoreOptions); err != nil {
		child.Release()
		return nil, vfs.PrependErrMsg("VFS dehydration restore failed", err)
	}
	child.Pause()
	if err := image.Close(); err != nil {
		child.Release()
		return nil, fmt.Errorf("releasing dehydration reservations: %w", err)
	}
	return child, nil
}

// LoadDurableDehydration reconstructs a paused Kernel in a new Sentry. Unlike
// LoadDehydration, it has no pointers into the old process: allocator metadata
// is installed into new MemoryFiles and page bytes are faulted on demand from
// immutable packed snapshot files.
func LoadDurableDehydration(ctx context.Context, p platform.Platform, image DurableDehydrationMetadata, memories []DurableDehydrationLoadMemory, opts DurableDehydrationLoadOptions) (*Kernel, error) {
	if p == nil || opts.Clocks == nil || opts.BranchID == "" || len(image.KernelMetadata) == 0 || len(memories) == 0 || len(memories) != len(image.Memories) {
		return nil, errors.New("durable dehydration load is missing required state")
	}
	for i := range memories {
		if memories[i].MemoryFile == nil || memories[i].Reader == nil {
			return nil, fmt.Errorf("durable dehydration memory %d is incomplete", i)
		}
	}
	loads := make([]*pgalloc.AsyncPagesFileLoad, 0, len(memories))
	readersConsumed := 0
	loaded := false
	defer func() {
		if loaded {
			return
		}
		for _, load := range loads {
			load.Cancel()
			_ = load.Wait()
		}
		for i := readersConsumed; i < len(memories); i++ {
			_ = memories[i].Reader.Close()
		}
		for i := range memories {
			memories[i].MemoryFile.Destroy()
		}
	}()
	for i := range memories {
		if err := pgalloc.ImportMemoryFileMetadataForRemote(ctx, memories[i].MemoryFile, image.Memories[i].AllocatorMetadata); err != nil {
			return nil, fmt.Errorf("importing dehydration allocator metadata %d: %w", i, err)
		}
		reader := memories[i].Reader
		readersConsumed++
		load, err := pgalloc.StartAsyncPagesFileLoadWithOpts(reader, nil, nil, pgalloc.AsyncPagesFileLoadOpts{DemandOnly: true})
		if err != nil {
			return nil, fmt.Errorf("starting dehydration page loader %d: %w", i, err)
		}
		loads = append(loads, load)
		if err := memories[i].MemoryFile.AttachRemotePageLoad(load, image.Memories[i].LoadRanges, nil, nil); err != nil {
			return nil, fmt.Errorf("attaching dehydration page loader %d: %w", i, err)
		}
		load.MemoryFilesDone()
	}

	private := make(map[checkpoint.ResourceID]*pgalloc.MemoryFile, len(memories)-1)
	for i := 1; i < len(memories); i++ {
		private[image.Memories[i].ResourceID] = memories[i].MemoryFile
	}
	child := &Kernel{Platform: p}
	child.SetMemoryFile(memories[0].MemoryFile)
	child.initRuntimeFields()
	loadValues := map[any]any{
		pgalloc.CtxMemoryFile:                memories[0].MemoryFile,
		pgalloc.CtxMemoryFileMap:             private,
		pgalloc.CtxLiveForkClone:             true,
		pgalloc.CtxLiveForkRemoteMemoryClone: true,
		vfs.CtxLiveForkChildBranchID:         opts.BranchID,
		devutil.CtxDevGoferClientProvider:    child,
	}
	if opts.FilesystemResources != nil {
		loadValues[vfs.CtxLiveForkFilesystemResources] = opts.FilesystemResources
	}
	if opts.RestoreFilesystemResources != nil {
		loadValues[vfs.CtxRestoreFilesystemResources] = opts.RestoreFilesystemResources
	}
	if opts.RestoreFilesystemFDMap != nil {
		loadValues[vfs.CtxRestoreFilesystemFDMap] = opts.RestoreFilesystemFDMap
	}
	loadCtx := context.WithValues(child.SupervisorContext(), loadValues)
	if _, err := state.Load(loadCtx, bufio.NewReaderSize(bytes.NewReader(image.KernelMetadata), 64<<10), child); err != nil {
		return nil, fmt.Errorf("loading durable dehydrated Kernel metadata: %w", err)
	}
	child.remotePageLoad = loads[0]
	child.auxiliaryRemotePageLoads = append(child.auxiliaryRemotePageLoads, loads[1:]...)
	loaded = true
	if _, err := child.vdsoParams.access(); err != nil {
		child.Release()
		return nil, fmt.Errorf("materializing dehydrated VDSO parameter page: %w", err)
	}
	if err := child.privatizeLiveForkVDSOParamPage(); err != nil {
		child.Release()
		return nil, err
	}
	child.Timekeeper().SetClocks(opts.Clocks, child.vdsoParams)
	if rootNS := child.rootNetworkNamespace; rootNS != nil && rootNS.Stack() != nil {
		if opts.ConfigureNetwork == nil {
			child.Release()
			return nil, errors.New("durable dehydration network configurator was not provided")
		}
		rootNS.Stack().ResetConfig()
		if err := opts.ConfigureNetwork(child, rootNS.Stack()); err != nil {
			child.Release()
			return nil, fmt.Errorf("configuring rehydrated network: %w", err)
		}
		rootNS.Stack().Restore()
	}
	if err := child.vfs.CompleteRestore(loadCtx, &opts.RestoreOptions); err != nil {
		child.Release()
		return nil, vfs.PrependErrMsg("VFS durable dehydration restore failed", err)
	}
	child.Pause()
	return child, nil
}

// LoadSparseDurableDehydration reconstructs a paused Kernel from an immutable
// sparse manifest. memoryFiles must contain the application MemoryFile under
// the zero ResourceID and every private MemoryFile named by the manifest. The
// function consumes all MemoryFiles on success and failure.
func LoadSparseDurableDehydration(ctx context.Context, p platform.Platform, image DurableDehydrationMetadata, memoryFiles map[checkpoint.ResourceID]*pgalloc.MemoryFile, repository *ttcchunk.MemoryRepository, opts DurableDehydrationLoadOptions) (*Kernel, error) {
	applicationID := checkpoint.ResourceID{}
	application := memoryFiles[applicationID]
	if p == nil || application == nil || repository == nil || opts.Clocks == nil || opts.BranchID == "" || len(image.KernelMetadata) == 0 || image.SparseManifest == (ttcchunk.ManifestID{}) {
		for _, mf := range memoryFiles {
			if mf != nil {
				mf.Destroy()
			}
		}
		return nil, errors.New("sparse durable dehydration load is missing required state")
	}
	consumed := false
	defer func() {
		if !consumed {
			for _, mf := range memoryFiles {
				mf.Destroy()
			}
		}
	}()
	manifest, err := repository.LoadManifest(image.SparseManifest)
	if err != nil {
		return nil, fmt.Errorf("loading sparse dehydration manifest: %w", err)
	}
	installation, err := repository.InstallManifestSet(memoryFiles, manifest, usage.Anonymous, 0)
	if err != nil {
		return nil, fmt.Errorf("installing sparse dehydration manifest: %w", err)
	}
	installationCommitted := false
	defer func() {
		if !installationCommitted {
			_ = installation.Abort()
		}
	}()

	var pages *pgalloc.AsyncPagesFileLoad
	var chunkReader *ttcchunk.ChunkReader
	consumptionActivated := false
	defer func() {
		if chunkReader != nil && !consumptionActivated {
			chunkReader.AbortConsumption()
		}
	}()
	if len(installation.Chunks) != 0 {
		if opts.OwnSparseRepository && opts.SparseBranch != "" {
			chunkReader, err = ttcchunk.NewOwnedConsumingChunkReader(repository, installation.Chunks, opts.SparseBranch, image.SparseManifest)
		} else if opts.OwnSparseRepository {
			chunkReader, err = ttcchunk.NewOwnedChunkReader(repository, installation.Chunks)
		} else {
			chunkReader, err = ttcchunk.NewChunkReader(repository, installation.Chunks)
		}
		if err != nil {
			return nil, err
		}
		pages, err = pgalloc.StartAsyncPagesFileLoadWithOpts(chunkReader.AsyncReader(4), nil, nil, pgalloc.AsyncPagesFileLoadOpts{DemandOnly: true, RangeLoaded: chunkReader.RangeLoaded})
		if err != nil {
			chunkReader.AbortConsumption()
			return nil, err
		}
		rangesByFile := make(map[*pgalloc.MemoryFile][]pgalloc.RemoteLoadRange)
		for _, chunk := range installation.Chunks {
			rangesByFile[chunk.MemoryFile] = append(rangesByFile[chunk.MemoryFile], pgalloc.RemoteLoadRange{Target: chunk.Target, StreamOffset: chunk.StreamOffset})
		}
		for mf, ranges := range rangesByFile {
			if err := mf.AttachSparseChunkPageLoad(pages, ranges, nil, nil); err != nil {
				pages.Cancel()
				_ = pages.Wait()
				return nil, err
			}
		}
		pages.MemoryFilesDone()
	}
	repositoryOwned := len(installation.Chunks) == 0 && opts.OwnSparseRepository
	defer func() {
		if repositoryOwned {
			_ = repository.Close()
		}
	}()
	pagesOwned := pages != nil
	defer func() {
		if pagesOwned {
			pages.Cancel()
			_ = pages.Wait()
		}
	}()

	child := &Kernel{Platform: p}
	child.SetMemoryFile(application)
	child.initRuntimeFields()
	private := make(map[checkpoint.ResourceID]*pgalloc.MemoryFile, len(memoryFiles)-1)
	for resource, mf := range memoryFiles {
		if resource.Ok() {
			private[resource] = mf
		}
	}
	referenceTransaction := pgalloc.NewSparseReferenceTransaction()
	loadValues := map[any]any{
		pgalloc.CtxMemoryFile:             application,
		pgalloc.CtxMemoryFileMap:          private,
		pgalloc.CtxLiveForkClone:          true,
		pgalloc.CtxSparseRelocation:       installation.Relocation,
		pgalloc.CtxSparseRestoreRefs:      referenceTransaction,
		vfs.CtxLiveForkChildBranchID:      opts.BranchID,
		devutil.CtxDevGoferClientProvider: child,
	}
	if opts.FilesystemResources != nil {
		loadValues[vfs.CtxLiveForkFilesystemResources] = opts.FilesystemResources
	}
	if opts.RestoreFilesystemResources != nil {
		loadValues[vfs.CtxRestoreFilesystemResources] = opts.RestoreFilesystemResources
	}
	if opts.RestoreFilesystemFDMap != nil {
		loadValues[vfs.CtxRestoreFilesystemFDMap] = opts.RestoreFilesystemFDMap
	}
	loadCtx := context.WithValues(child.SupervisorContext(), loadValues)
	if _, err := state.Load(loadCtx, bufio.NewReaderSize(bytes.NewReader(image.KernelMetadata), 64<<10), child); err != nil {
		referenceTransaction.Abort()
		return nil, fmt.Errorf("loading sparse dehydrated Kernel metadata: %w", err)
	}
	if err := referenceTransaction.Commit(); err != nil {
		return nil, err
	}
	if err := installation.Commit(); err != nil {
		return nil, err
	}
	installationCommitted = true
	consumed = true
	if pages != nil {
		child.remotePageLoad = pages
		pagesOwned = false
	}
	if _, err := child.vdsoParams.access(); err != nil {
		child.Release()
		return nil, fmt.Errorf("materializing sparse dehydrated VDSO parameter page: %w", err)
	}
	if err := child.privatizeLiveForkVDSOParamPage(); err != nil {
		child.Release()
		return nil, err
	}
	child.Timekeeper().SetClocks(opts.Clocks, child.vdsoParams)
	if rootNS := child.rootNetworkNamespace; rootNS != nil && rootNS.Stack() != nil {
		if opts.ConfigureNetwork == nil {
			child.Release()
			return nil, errors.New("sparse dehydrated network configurator was not provided")
		}
		rootNS.Stack().ResetConfig()
		if err := opts.ConfigureNetwork(child, rootNS.Stack()); err != nil {
			child.Release()
			return nil, fmt.Errorf("configuring sparse rehydrated network: %w", err)
		}
		rootNS.Stack().Restore()
	}
	if err := child.vfs.CompleteRestore(loadCtx, &opts.RestoreOptions); err != nil {
		child.Release()
		return nil, vfs.PrependErrMsg("VFS sparse dehydration restore failed", err)
	}
	if chunkReader != nil {
		chunkReader.ActivateConsumption(installation.ResidentObjects())
		consumptionActivated = true
	} else if opts.SparseBranch != "" {
		_ = repository.MarkConsumed(opts.SparseBranch, image.SparseManifest, installation.Objects)
	}
	child.Pause()
	return child, nil
}
