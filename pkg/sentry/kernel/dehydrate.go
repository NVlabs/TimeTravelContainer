// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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
	"slices"
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
	"gvisor.dev/gvisor/pkg/ttcsnapshot"
)

// DehydrationCapture is a metadata-only saved Kernel whose pages stay where
// they are in its MemoryFiles, held by a reservation of the pages the branch
// reaches, so restoring this image preserves page sharing with live siblings.
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

	// sparseRegions are what SparseSnapshot was made from, so that a parked
	// snapshot can be made again for another export. See ParkSparseSnapshot.
	sparseRegions []pgalloc.RemoteSnapshotRegion
	// reservation holds the pages of sparseRegions until the capture is
	// restored or closed. It holds no others: one a live sibling sharing the
	// MemoryFiles holds alone stays the sibling's to write in place and to
	// free.
	reservation *pgalloc.RemoteSnapshot
	// owners are the MemoryFiles of MemoryImages, in order, each with an
	// owner reference of the capture's.
	owners       []*pgalloc.MemoryFile
	consumed     bool
	metadataFile *os.File
}

// DurableDehydrationMemory describes one MemoryFile in a process-independent
// dehydration image. The first entry always describes application memory;
// remaining entries are ordered by ResourceID and describe private tmpfs-like
// MemoryFiles. StreamBytes counts the bytes of the file's pages the branch
// reaches.
type DurableDehydrationMemory struct {
	ResourceID  checkpoint.ResourceID `json:"resource_id"`
	StreamBytes uint64                `json:"stream_bytes"`
}

// DurableDehydrationMetadata is sufficient to reconstruct a Kernel in a new
// Sentry when paired with immutable snapshot-graph objects for each Memories
// entry.
type DurableDehydrationMetadata struct {
	Version        int                        `json:"version"`
	KernelMetadata []byte                     `json:"kernel_metadata"`
	Memories       []DurableDehydrationMemory `json:"memories"`
}

const durableDehydrationVersion = 1

// DehydrationCaptureOptions controls a local hibernation capture.
type DehydrationCaptureOptions struct {
	BranchID     string
	MetadataFile *os.File
}

// DehydrationLoadOptions controls reconstruction of a dehydrated Kernel.
type DehydrationLoadOptions struct {
	Clocks             sentrytime.Clocks
	BranchID           string
	Quarantined        bool
	OverrideQuarantine bool
	RestoreOptions     vfs.CompleteRestoreOptions
	ConfigureNetwork   func(*Kernel, inet.Stack) error
}

// DurableDehydrationLoadOptions controls reconstruction in a new Sentry.
type DurableDehydrationLoadOptions struct {
	Clocks                     sentrytime.Clocks
	BranchID                   string
	Quarantined                bool
	OverrideQuarantine         bool
	RestoreOptions             vfs.CompleteRestoreOptions
	FilesystemResources        vfs.LiveForkFilesystemResources
	RestoreFilesystemResources vfs.RestoreFilesystemResources
	RestoreFilesystemFDMap     map[checkpoint.ResourceID]int
	ConfigureNetwork           func(*Kernel, inet.Stack) error
	MaxReadBytes               uint64
	MaxParallel                int
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
		if err := k.vfs.PrepareSave(saveCtx, vfs.PrepareSaveOptions{}); err != nil {
			return fmt.Errorf("preparing filesystem resources for dehydration peer restore: %w", err)
		}
		defer k.vfs.BeforeResume(saveCtx)
		return load(resources)
	})
}

// MemoryFiles returns the MemoryFiles d refers to, each with an ownership
// reference of the caller's, which it releases with Destroy.
func (d *DehydrationCapture) MemoryFiles() []*pgalloc.MemoryFile {
	if d == nil || d.consumed {
		return nil
	}
	files := slices.Clone(d.owners)
	for _, file := range files {
		file.IncOwner()
	}
	return files
}

// LendPageLoads gives each of targets an owner reference on each loader d
// keeps alive. A branch of the Sentry that shares the MemoryFile d was
// captured from may still have pages to load from those loaders, without
// owning them, as when it read units an import of d's branch installed: a
// capture closed with its branch's destruction was their last owner, and
// stopped them, and every later dehydration of the branch that read them
// failed, "materializing branch memory before dehydration: context canceled".
func (d *DehydrationCapture) LendPageLoads(targets ...*Kernel) {
	if d == nil || d.consumed || len(targets) == 0 {
		return
	}
	var snapshots []*pgalloc.RemoteSnapshot
	for _, snapshot := range []*pgalloc.RemoteSnapshot{d.reservation, d.SparseSnapshot} {
		if snapshot != nil {
			snapshots = append(snapshots, snapshot)
		}
	}
	for _, target := range targets {
		for _, snapshot := range snapshots {
			for _, load := range snapshot.LendPageLoads() {
				target.adoptPageLoad(load)
			}
		}
	}
}

// Close releases page and owner reservations held by d.
func (d *DehydrationCapture) Close() error {
	if d == nil || d.consumed {
		return nil
	}
	d.consumed = true
	var result error
	if d.reservation != nil {
		result = errors.Join(result, d.reservation.Close())
		d.reservation = nil
	}
	if d.SparseSnapshot != nil {
		result = errors.Join(result, d.SparseSnapshot.Close())
		d.SparseSnapshot = nil
	}
	for _, mf := range d.owners {
		mf.Destroy()
	}
	d.owners = nil
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
		if capture.reservation != nil {
			reservations = append(reservations, capture.reservation)
		}
	}
	return pgalloc.SyncAndDropSnapshotCache(reservations)
}

// addMemoryFile describes mf, whose resource is resourceID, in d's image, and
// makes d one of its owners.
func (d *DehydrationCapture) addMemoryFile(resourceID checkpoint.ResourceID, mf *pgalloc.MemoryFile) {
	mf.IncOwner()
	d.owners = append(d.owners, mf)
	d.MemoryImages = append(d.MemoryImages, DurableDehydrationMemory{ResourceID: resourceID})
}

// RetainGraphPageLoads keeps d's pages that are still loading from the
// snapshot graph readable after d's Kernel is released, and reports whether
// those are all of d's pages still loading; see
// pgalloc.RemoteSnapshot.RetainGraphPageLoads.
func (d *DehydrationCapture) RetainGraphPageLoads() (bool, error) {
	if d == nil || d.consumed {
		return false, errors.New("dehydration capture is unavailable")
	}
	if d.reservation == nil {
		return true, nil
	}
	return d.reservation.RetainGraphPageLoads()
}

// ownPageLoad makes k an owner of load, which loads pages k may read, unless it
// is one already.
func (k *Kernel) ownPageLoad(load *pgalloc.AsyncPagesFileLoad) {
	k.remoteLoadsMu.Lock()
	defer k.remoteLoadsMu.Unlock()
	if k.remotePageLoad == load || slices.Contains(k.auxiliaryRemotePageLoads, load) {
		return
	}
	load.IncOwner()
	k.auxiliaryRemotePageLoads = append(k.auxiliaryRemotePageLoads, load)
}

// adoptPageLoad is ownPageLoad for an owner reference the caller already
// holds, which k takes over.
func (k *Kernel) adoptPageLoad(load *pgalloc.AsyncPagesFileLoad) {
	k.remoteLoadsMu.Lock()
	owned := k.remotePageLoad == load || slices.Contains(k.auxiliaryRemotePageLoads, load)
	if !owned {
		k.auxiliaryRemotePageLoads = append(k.auxiliaryRemotePageLoads, load)
	}
	k.remoteLoadsMu.Unlock()
	if owned {
		_ = load.DecOwner()
	}
}

// DurableMetadata returns the process-independent description represented by
// d.
func (d *DehydrationCapture) DurableMetadata() DurableDehydrationMetadata {
	return DurableDehydrationMetadata{
		Version: durableDehydrationVersion, KernelMetadata: append([]byte(nil), d.KernelMetadata...), Memories: slices.Clone(d.MemoryImages),
	}
}

// ParkSparseSnapshot closes the capture's view of its branch-reachable memory
// until an export needs it again.
//
// A capture may wait a long time for its export: ttcd makes a dehydration
// durable only when the last live branch of its family on the node goes. An
// open view is told of every copy-on-write break in the MemoryFiles it reads,
// and each notice scans its regions, so while it waited it taxed every write
// the family's live branches made. The capture's reservation, which is not
// told of them, keeps the pages themselves, so the view can be made again from
// the same regions.
func (d *DehydrationCapture) ParkSparseSnapshot() error {
	if d == nil || d.consumed {
		return errors.New("dehydration capture is unavailable")
	}
	if d.SparseSnapshot == nil {
		return nil
	}
	err := d.SparseSnapshot.Close()
	d.SparseSnapshot = nil
	return err
}

// AttachMetadataFile makes file the capture's durable metadata record and
// writes the capture's process-independent description there at once, so the
// record is complete even if a later export fails. The capture owns file from
// then on, and PublishGraphStream rewrites it.
func (d *DehydrationCapture) AttachMetadataFile(file *os.File) error {
	if d == nil || d.consumed || file == nil {
		if file != nil {
			_ = file.Close()
		}
		return errors.New("dehydration capture is unavailable")
	}
	if err := writeDehydrationMetadata(file, d.DurableMetadata()); err != nil {
		_ = file.Close()
		return fmt.Errorf("writing dehydration metadata: %w", err)
	}
	if d.metadataFile != nil {
		_ = d.metadataFile.Close()
	}
	d.metadataFile = file
	return nil
}

// PublishGraphStream sends the exact branch-reachable memory graph to the
// daemon without creating a persistent compatibility repository. The daemon
// is the sole writer of the authoritative snapshot graph. It may be called
// again, for example when the daemon failed to record an earlier export.
//
// If filtered is set, the daemon answers the manifest on stream with the
// pages its graph lacks, and only those are sent.
func (d *DehydrationCapture) PublishGraphStream(stream io.ReadWriter, filtered bool) error {
	if d == nil || d.consumed || stream == nil || (d.SparseSnapshot == nil && len(d.sparseRegions) == 0) {
		return errors.New("graph dehydration publication is unavailable")
	}
	if d.SparseSnapshot == nil {
		snapshot, err := pgalloc.NewRemoteSnapshot(d.sparseRegions)
		if err != nil {
			return fmt.Errorf("reopening dehydrated memory: %w", err)
		}
		d.SparseSnapshot = snapshot
	}
	var err error
	if filtered {
		err = ttcchunk.WriteFilteredGraphCapture(stream, d.SparseSnapshot, d.SparseBindings)
	} else {
		err = ttcchunk.WriteGraphCapture(stream, d.SparseSnapshot, d.SparseBindings)
	}
	if err != nil {
		return err
	}
	if err := d.ParkSparseSnapshot(); err != nil {
		return err
	}
	if d.metadataFile != nil {
		if err := writeDehydrationMetadata(d.metadataFile, d.DurableMetadata()); err != nil {
			return fmt.Errorf("updating graph dehydration metadata: %w", err)
		}
	}
	return nil
}

// CaptureDehydration serializes Kernel metadata and holds the pages the
// branch reaches. Page bytes remain in their existing MemoryFiles and are not
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
		if err := k.flushPlatformForStateCapture(ctx); err != nil {
			return err
		}
		resumeNetwork, err := k.pauseLiveForkNetworkStacks(true, false)
		if err != nil {
			return err
		}
		defer resumeNetwork()
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
		if err := k.vfs.PrepareSave(saveCtx, vfs.PrepareSaveOptions{}); err != nil {
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
		image.addMemoryFile(checkpoint.ResourceID{}, k.mf)
		privateIDs := make([]checkpoint.ResourceID, 0, len(image.PrivateMemoryFiles))
		for id := range image.PrivateMemoryFiles {
			privateIDs = append(privateIDs, id)
		}
		sort.Slice(privateIDs, func(i, j int) bool { return privateIDs[i].String() < privateIDs[j].String() })
		for _, id := range privateIDs {
			image.addMemoryFile(id, image.PrivateMemoryFiles[id])
		}
		var metadata bytes.Buffer
		buffered := bufio.NewWriterSize(&metadata, 64<<10)
		if _, err := k.saveStateGraph(saveCtx, buffered, false); err != nil {
			return fmt.Errorf("saving dehydrated Kernel metadata: %w", err)
		}
		if err := buffered.Flush(); err != nil {
			return fmt.Errorf("flushing dehydrated Kernel metadata: %w", err)
		}
		// A 2 MiB page is stored whole.
		layout, err := memoryCapture.HugePageSnapshotLayout()
		if err != nil {
			return fmt.Errorf("resolving dehydration memory graph: %w", err)
		}
		// The capture holds the pages its branch reaches, and no others, so
		// that while it waits the family's live branches still write in place
		// the pages they hold alone, and free them.
		reservation, err := pgalloc.NewRemoteSnapshotReservation(layout.Regions)
		if err != nil {
			return fmt.Errorf("reserving dehydration memory: %w", err)
		}
		image.reservation = reservation
		streamBytes := make(map[*pgalloc.MemoryFile]uint64)
		for _, region := range layout.Regions {
			streamBytes[region.File] += region.Range.Length()
		}
		for i, mf := range image.owners {
			image.MemoryImages[i].StreamBytes = streamBytes[mf]
		}
		sparseSnapshot, err := pgalloc.NewRemoteSnapshot(layout.Regions)
		if err != nil {
			return fmt.Errorf("reserving sparse dehydration memory: %w", err)
		}
		image.SparseBindings = layout.Bindings
		image.SparseSnapshot = sparseSnapshot
		image.sparseRegions = layout.Regions
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
	references := pgalloc.NewSparseReferenceTransaction()
	loadCtx := context.WithValues(child.SupervisorContext(), map[any]any{
		pgalloc.CtxMemoryFile:              image.MemoryFile,
		pgalloc.CtxMemoryFileMap:           image.PrivateMemoryFiles,
		pgalloc.CtxLiveForkClone:           true,
		pgalloc.CtxSparseRestoreRefs:       references,
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
	if _, err := state.LoadWithOptions(loadCtx, bufio.NewReaderSize(bytes.NewReader(metadata), 64<<10), child, state.LoadOptions{NoStats: true}); err != nil {
		references.Abort()
		return nil, fmt.Errorf("loading dehydrated Kernel metadata: %w", err)
	}
	// A page the state refers to that the capture did not hold may have been
	// freed and reused since.
	if err := references.CheckHeldBy(image.reservation); err != nil {
		references.Abort()
		return nil, fmt.Errorf("loading dehydrated Kernel metadata: %w", err)
	}
	if err := references.Commit(); err != nil {
		return nil, err
	}
	ownersTransferred = true
	// The saved VVAR page may have been updated between metadata capture and
	// final Kernel teardown, and may also remain shared with a live sibling.
	// Rebind it before SetClocks resets the restored sequence counter.
	if err := child.privatizeLiveForkVDSOParamPage(); err != nil {
		child.releaseLoaded(opts.Clocks)
		return nil, err
	}
	child.Timekeeper().SetClocks(opts.Clocks, child.vdsoParams)
	if opts.OverrideQuarantine {
		child.liveForkQuarantine = opts.Quarantined
	}
	if err := child.restoreReconfiguredNetworkStacks(func(stack inet.Stack) error {
		if opts.ConfigureNetwork == nil {
			return errors.New("dehydrated network stack requires a configurator")
		}
		if err := opts.ConfigureNetwork(child, stack); err != nil {
			return fmt.Errorf("configuring rehydrated network: %w", err)
		}
		return nil
	}); err != nil {
		child.Release()
		return nil, err
	}
	if err := child.vfs.CompleteRestore(loadCtx, &opts.RestoreOptions); err != nil {
		child.Release()
		return nil, vfs.PrependErrMsg("VFS dehydration restore failed", err)
	}
	child.Pause()
	// Pages the capture kept loading from the snapshot graph are the child's
	// to read now; see RetainGraphPageLoads.
	if image.reservation != nil {
		for _, load := range image.reservation.PageLoads() {
			child.ownPageLoad(load)
		}
	}
	if err := image.Close(); err != nil {
		child.Release()
		return nil, fmt.Errorf("releasing dehydration reservations: %w", err)
	}
	return child, nil
}

type sparseDehydrationReader interface {
	AsyncReader(int) stateio.AsyncReader
	RangeLoaded(uint64, uint64)
	ActivateConsumption([]ttcchunk.ID)
	AbortConsumption()
	Close() error
}

type sparseDehydrationSource struct {
	manifest     ttcchunk.Manifest
	newReader    func([]ttcchunk.InstalledChunk) (sparseDehydrationReader, error)
	close        func() error
	markConsumed func([]ttcchunk.ID)
}

// LoadGraphDurableDehydration reconstructs a paused Kernel directly from
// graph-native 1 MiB objects donated through data. It consumes data.
func LoadGraphDurableDehydration(ctx context.Context, p platform.Platform, image DurableDehydrationMetadata, memoryFiles map[checkpoint.ResourceID]*pgalloc.MemoryFile, manifest ttcchunk.Manifest, chunks map[ttcchunk.ID]ttcsnapshot.ObjectID, data *ttcsnapshot.LocalDataStore, remote *ttcsnapshot.ObjectRelayClient, opts DurableDehydrationLoadOptions) (*Kernel, error) {
	if data == nil || len(manifest.Runs) == 0 || len(chunks) == 0 {
		for _, mf := range memoryFiles {
			if mf != nil {
				mf.Destroy()
			}
		}
		if data != nil {
			_ = data.Close()
		}
		if remote != nil {
			_ = remote.Close()
		}
		return nil, errors.New("graph durable dehydration load is missing required memory state")
	}
	source := sparseDehydrationSource{
		manifest: manifest,
		newReader: func(installed []ttcchunk.InstalledChunk) (sparseDehydrationReader, error) {
			return ttcchunk.NewGraphChunkReaderWithRelay(data, chunks, installed, remote, true)
		},
		close: data.Close,
	}
	return loadSparseDurableDehydration(ctx, p, image, memoryFiles, source, opts)
}

func loadSparseDurableDehydration(ctx context.Context, p platform.Platform, image DurableDehydrationMetadata, memoryFiles map[checkpoint.ResourceID]*pgalloc.MemoryFile, source sparseDehydrationSource, opts DurableDehydrationLoadOptions) (*Kernel, error) {
	applicationID := checkpoint.ResourceID{}
	application := memoryFiles[applicationID]
	if p == nil || application == nil || opts.Clocks == nil || opts.BranchID == "" || len(image.KernelMetadata) == 0 || source.newReader == nil {
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
	installation, err := ttcchunk.InstallManifestSet(memoryFiles, source.manifest, usage.Anonymous, 0)
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
	var chunkReader sparseDehydrationReader
	consumptionActivated := false
	defer func() {
		if chunkReader != nil && !consumptionActivated {
			chunkReader.AbortConsumption()
		}
	}()
	if len(installation.Chunks) != 0 {
		chunkReader, err = source.newReader(installation.Chunks)
		if err != nil {
			return nil, err
		}
		pages, err = pgalloc.StartAsyncPagesFileLoadWithOpts(chunkReader.AsyncReader(4), nil, nil, pgalloc.AsyncPagesFileLoadOpts{
			DemandOnly: true, DurableBacking: true, RangeLoaded: chunkReader.RangeLoaded,
		})
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
	repositoryOwned := len(installation.Chunks) == 0 && source.close != nil
	defer func() {
		if repositoryOwned {
			_ = source.close()
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
	if _, err := state.LoadWithOptions(loadCtx, bufio.NewReaderSize(bytes.NewReader(image.KernelMetadata), 64<<10), child, state.LoadOptions{NoStats: true}); err != nil {
		referenceTransaction.Abort()
		return nil, fmt.Errorf("loading sparse dehydrated Kernel metadata: %w", err)
	}
	if err := referenceTransaction.Commit(); err != nil {
		return nil, err
	}
	// The units the installation reused may still be loading for the owners
	// that installed them, whose going would stop their loaders: the child,
	// which reads them too, keeps those loaders.
	for _, load := range installation.TakePageLoads() {
		child.adoptPageLoad(load)
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
	if err := child.vdsoParams.materialize(); err != nil {
		child.releaseLoaded(opts.Clocks)
		return nil, fmt.Errorf("materializing sparse dehydrated VDSO parameter page: %w", err)
	}
	if err := child.privatizeLiveForkVDSOParamPage(); err != nil {
		child.releaseLoaded(opts.Clocks)
		return nil, err
	}
	child.Timekeeper().SetClocks(opts.Clocks, child.vdsoParams)
	if opts.OverrideQuarantine {
		child.liveForkQuarantine = opts.Quarantined
	}
	if err := child.restoreReconfiguredNetworkStacks(func(stack inet.Stack) error {
		if opts.ConfigureNetwork == nil {
			return errors.New("sparse dehydrated network configurator was not provided")
		}
		if err := opts.ConfigureNetwork(child, stack); err != nil {
			return fmt.Errorf("configuring sparse rehydrated network: %w", err)
		}
		return nil
	}); err != nil {
		child.Release()
		return nil, err
	}
	if err := child.vfs.CompleteRestore(loadCtx, &opts.RestoreOptions); err != nil {
		child.Release()
		return nil, vfs.PrependErrMsg("VFS sparse dehydration restore failed", err)
	}
	if chunkReader != nil {
		chunkReader.ActivateConsumption(installation.ResidentObjects())
		consumptionActivated = true
	} else if source.markConsumed != nil {
		source.markConsumed(installation.Objects)
	}
	child.Pause()
	return child, nil
}
