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
	"errors"
	"fmt"
	"io"
	"os"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/devutil"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/inet"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	sentrytime "gvisor.dev/gvisor/pkg/sentry/time"
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

	reservations []*pgalloc.RemoteSnapshot
	emptyOwners  []*pgalloc.MemoryFile
	consumed     bool
	metadataFile *os.File
}

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

var dehydrationMetadataMagic = [8]byte{'T', 'T', 'C', 'D', 'H', 'Y', '0', '1'}

func writeDehydrationMetadata(file *os.File, metadata []byte) error {
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

func readDehydrationMetadata(file *os.File) ([]byte, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	var header [48]byte
	if _, err := io.ReadFull(file, header[:]); err != nil {
		return nil, err
	}
	if !bytes.Equal(header[:8], dehydrationMetadataMagic[:]) {
		return nil, errors.New("invalid dehydration metadata magic")
	}
	length := binary.LittleEndian.Uint64(header[8:16])
	if length > uint64(^uint(0)>>1) {
		return nil, errors.New("dehydration metadata is too large")
	}
	metadata := make([]byte, int(length))
	if _, err := io.ReadFull(file, metadata); err != nil {
		return nil, err
	}
	hash := sha256.Sum256(metadata)
	if !bytes.Equal(header[16:], hash[:]) {
		return nil, errors.New("dehydration metadata checksum mismatch")
	}
	return metadata, nil
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

func (d *DehydrationCapture) reserveMemoryFile(ctx context.Context, mf *pgalloc.MemoryFile) error {
	_, regions, _, err := pgalloc.ExportMemoryFileMetadataForRemote(ctx, mf)
	if err != nil {
		return err
	}
	if len(regions) == 0 {
		mf.IncOwner()
		d.emptyOwners = append(d.emptyOwners, mf)
		return nil
	}
	reservation, err := pgalloc.NewRemoteSnapshot(regions)
	if err != nil {
		return err
	}
	d.reservations = append(d.reservations, reservation)
	return nil
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
	// A remote target must become source-independent before its Kernel graph can
	// be released. This also makes every byte available to the local persistent
	// backing file before SyncAndDropCache runs.
	if k.remotePageLoad != nil {
		k.remotePageLoad.Drain()
		if err := k.remotePageLoad.Wait(); err != nil {
			return nil, fmt.Errorf("materializing remote memory before dehydration: %w", err)
		}
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
		saveCtx := context.WithValues(k.SupervisorContext(), map[any]any{
			pgalloc.CtxMemoryFile:              k.mf,
			pgalloc.CtxMemoryFileMap:           image.PrivateMemoryFiles,
			pgalloc.CtxLiveForkSource:          true,
			vfs.CtxLiveForkFilesystemResources: image.FilesystemResources,
			vfs.CtxLiveForkChildBranchID:       opts.BranchID,
			vfs.CtxLiveForkBranchesPrepared:    true,
		})
		if err := k.vfs.PrepareSave(saveCtx); err != nil {
			return fmt.Errorf("preparing VFS for dehydration: %w", err)
		}
		defer k.vfs.BeforeResume(saveCtx)
		k.prepareMemoryManagersForDistributedFork()
		k.mf.MarkSavable()
		for _, mf := range image.PrivateMemoryFiles {
			mf.MarkSavable()
		}
		if err := image.reserveMemoryFile(ctx, k.mf); err != nil {
			return fmt.Errorf("reserving application memory: %w", err)
		}
		for id, mf := range image.PrivateMemoryFiles {
			if err := image.reserveMemoryFile(ctx, mf); err != nil {
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
		if image.metadataFile != nil {
			if err := writeDehydrationMetadata(image.metadataFile, metadata.Bytes()); err != nil {
				return fmt.Errorf("writing dehydration metadata: %w", err)
			}
		} else {
			image.KernelMetadata = metadata.Bytes()
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
		var err error
		metadata, err = readDehydrationMetadata(image.metadataFile)
		if err != nil {
			return nil, fmt.Errorf("reading dehydrated Kernel metadata: %w", err)
		}
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
