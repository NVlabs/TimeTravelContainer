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

package kernel_test

import (
	stdcontext "context"
	"errors"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/testutil"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/tmpfs"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	_ "gvisor.dev/gvisor/pkg/sentry/syscalls/linux"
	sentrytime "gvisor.dev/gvisor/pkg/sentry/time"
	"gvisor.dev/gvisor/pkg/sentry/ttcchunk"
	"gvisor.dev/gvisor/pkg/sentry/ttcremote"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

type ownedKernelTCPReader struct {
	stateio.AsyncReader
	server    *ttcremote.TCPPageServer
	serveDone <-chan error
}

func (r *ownedKernelTCPReader) Close() error {
	readerErr := r.AsyncReader.Close()
	serverErr := r.server.Close()
	serveErr := <-r.serveDone
	if readerErr != nil {
		return readerErr
	}
	if serverErr != nil {
		return serverErr
	}
	return serveErr
}

func newRemoteKernelMemoryFile(t *testing.T) *pgalloc.MemoryFile {
	t.Helper()
	file, err := os.CreateTemp("", "ttc-remote-kernel-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file.Name()); err != nil {
		file.Close()
		t.Fatal(err)
	}
	mf, err := pgalloc.NewMemoryFile(file, pgalloc.MemoryFileOpts{
		DelayedEviction:         pgalloc.DelayedEvictionDisabled,
		DisableIMAWorkAround:    true,
		DisableMemoryAccounting: true,
	})
	if err != nil {
		file.Close()
		t.Fatal(err)
	}
	return mf
}

var errInjectedLiveForkFailure = errors.New("injected live-fork failure")

func TestLiveForkFailureCleanup(t *testing.T) {
	stages := []kernel.LiveForkStage{
		kernel.LiveForkStageValidated,
		kernel.LiveForkStageSharedMemoryPrepared,
		kernel.LiveForkStageMappingsInvalidated,
		kernel.LiveForkStageVFSPrepared,
		kernel.LiveForkStageMemoryPrepared,
		kernel.LiveForkStageOwnerAcquired,
		kernel.LiveForkStageChildInitialized,
		kernel.LiveForkStageMetadataLoaded,
		kernel.LiveForkStageVDSOPrivatized,
		kernel.LiveForkStageNetworkRestored,
		kernel.LiveForkStageVFSRestored,
		kernel.LiveForkStageChildPaused,
	}
	for _, stage := range stages {
		t.Run(string(stage), func(t *testing.T) {
			parent, err := testutil.Boot()
			if err != nil {
				t.Fatalf("Boot: %v", err)
			}
			parent.PipeMount().IncRef()
			parent.SetHostMount(parent.PipeMount())
			parent.Pause()

			child, err := parent.LiveFork(parent.SupervisorContext(), kernel.LiveForkOptions{
				Clocks: sentrytime.NewCalibratedClocks(),
				TestOnlyFailureInjector: func(completed kernel.LiveForkStage) error {
					if completed == stage {
						return errInjectedLiveForkFailure
					}
					return nil
				},
			})
			if child != nil || !errors.Is(err, errInjectedLiveForkFailure) {
				t.Fatalf("LiveFork failure at %s: got child=%p err=%v", stage, child, err)
			}
			if !parent.IsPaused() {
				t.Fatalf("parent resumed after failure at %s", stage)
			}

			// A successful retry proves that save hooks, CoW transitions, owner
			// references, and partial child state were all left reusable.
			child, err = parent.LiveFork(parent.SupervisorContext(), kernel.LiveForkOptions{
				Clocks: sentrytime.NewCalibratedClocks(),
			})
			if err != nil {
				t.Fatalf("LiveFork retry after %s: %v", stage, err)
			}
			if err := parent.StartPaused(); err != nil {
				t.Fatalf("parent.StartPaused after %s: %v", stage, err)
			}
			if err := child.StartPaused(); err != nil {
				t.Fatalf("child.StartPaused after %s: %v", stage, err)
			}
			parent.Unpause()
			child.Unpause()
			parent.Pause()
			child.Pause()
			child.Release()
			parent.Release()
		})
	}
}

func TestLiveForkTwoKernelsSharePlatformAndMemoryFile(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	parent.Pause()

	var metrics kernel.LiveForkMetrics
	child, err := parent.LiveFork(parent.SupervisorContext(), kernel.LiveForkOptions{
		Clocks:  sentrytime.NewCalibratedClocks(),
		Metrics: &metrics,
	})
	if err != nil {
		t.Fatalf("LiveFork: %v", err)
	}

	if parent.Platform != child.Platform {
		t.Error("parent and child do not share a platform")
	}
	if parent.MemoryFile() != child.MemoryFile() {
		t.Error("parent and child do not share a MemoryFile")
	}
	if parent.RootPIDNamespace() == child.RootPIDNamespace() {
		t.Error("parent and child share a PID namespace object")
	}
	if metrics.Total <= 0 || metrics.MetadataStream <= 0 || metrics.ChildRestore <= 0 {
		t.Errorf("incomplete live-fork metrics: %+v", metrics)
	}
	parentInitID := parent.RootPIDNamespace().IDOfThreadGroup(parent.GlobalInit())
	childInitID := child.RootPIDNamespace().IDOfThreadGroup(child.GlobalInit())
	if parentInitID != childInitID {
		t.Errorf("init PID changed across live fork: parent=%d child=%d", parentInitID, childInitID)
	}

	if err := parent.StartPaused(); err != nil {
		t.Fatalf("parent.StartPaused: %v", err)
	}
	if err := child.StartPaused(); err != nil {
		t.Fatalf("child.StartPaused: %v", err)
	}
	parent.Unpause()
	child.Unpause()

	parent.Pause()
	child.Pause()
	parent.Release()
	child.Release()
}

func TestLiveForkRemoteMemoryFileLoopback(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	parent.Pause()
	targetMF := newRemoteKernelMemoryFile(t)

	child, err := parent.LiveFork(parent.SupervisorContext(), kernel.LiveForkOptions{
		Clocks:           sentrytime.NewCalibratedClocks(),
		RemoteMemoryFile: targetMF,
	})
	if err != nil {
		parent.Release()
		t.Fatalf("remote LiveFork: %v", err)
	}
	if child.MemoryFile() == parent.MemoryFile() {
		t.Fatal("remote child shares the source MemoryFile")
	}
	if child.MemoryFile() != targetMF {
		t.Fatal("remote child did not adopt the provided target MemoryFile")
	}
	if !child.MemoryFile().IsAsyncLoading() {
		t.Fatal("remote child is not backed by asynchronous page loading")
	}
	if err := child.MemoryFile().AwaitLoadAll(); err != nil {
		t.Fatalf("draining remote child memory: %v", err)
	}
	if child.MemoryFile().IsAsyncLoading() {
		t.Fatal("remote child MemoryFile remained loading after AwaitLoadAll")
	}

	if err := parent.StartPaused(); err != nil {
		t.Fatalf("parent.StartPaused: %v", err)
	}
	if err := child.StartPaused(); err != nil {
		t.Fatalf("child.StartPaused: %v", err)
	}
	parent.Unpause()
	child.Unpause()
	parent.Pause()
	child.Pause()
	dehydrated, err := child.CaptureDehydration(child.SupervisorContext(), kernel.DehydrationCaptureOptions{BranchID: "remote"})
	if err != nil {
		t.Fatalf("dehydrating distributed target: %v", err)
	}
	child.Release()
	if err := dehydrated.SyncAndDropCache(); err != nil {
		t.Fatalf("evicting distributed target memory: %v", err)
	}
	restored, err := kernel.LoadDehydration(parent.SupervisorContext(), parent.Platform, dehydrated, kernel.DehydrationLoadOptions{
		Clocks: sentrytime.NewCalibratedClocks(), BranchID: "remote",
	})
	if err != nil {
		t.Fatalf("rehydrating distributed target: %v", err)
	}
	if err := restored.StartPaused(); err != nil {
		t.Fatalf("starting rehydrated distributed target: %v", err)
	}
	restored.Release()
	parent.Release()
}

func TestDurableDehydrationLoadsPackedMemoryOnDemand(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	parent.Pause()
	image, err := parent.CaptureDehydration(parent.SupervisorContext(), kernel.DehydrationCaptureOptions{BranchID: "durable"})
	if err != nil {
		parent.Release()
		t.Fatalf("CaptureDehydration: %v", err)
	}
	metadata := image.DurableMetadata()
	snapshots := make([]*os.File, len(metadata.Memories))
	for i := range snapshots {
		snapshots[i], err = os.CreateTemp(t.TempDir(), "packed-*.mem")
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := image.ExportPackedSnapshots(snapshots, true); err != nil {
		t.Fatalf("ExportPackedSnapshots: %v", err)
	}
	loads := make([]kernel.DurableDehydrationLoadMemory, len(metadata.Memories))
	for i := range loads {
		fd, err := unix.Dup(int(snapshots[i].Fd()))
		if err != nil {
			t.Fatal(err)
		}
		loads[i].Reader = stateio.NewFDReader(int32(fd), 256<<10, 16, 2)
		target, err := os.CreateTemp(t.TempDir(), "target-*.mem")
		if err != nil {
			t.Fatal(err)
		}
		loads[i].MemoryFile, err = pgalloc.NewMemoryFile(target, pgalloc.MemoryFileOpts{
			DiskBackedFile: true, DisableIMAWorkAround: true, DisableMemoryAccounting: true,
			ResourceID: metadata.Memories[i].ResourceID,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	var restored *kernel.Kernel
	if err := parent.WithDehydrationFilesystemResources(parent.SupervisorContext(), "durable", func(resources vfs.LiveForkFilesystemResources) error {
		var err error
		restored, err = kernel.LoadDurableDehydration(parent.SupervisorContext(), parent.Platform, metadata, loads, kernel.DurableDehydrationLoadOptions{
			Clocks: sentrytime.NewCalibratedClocks(), BranchID: "durable", FilesystemResources: resources,
		})
		return err
	}); err != nil {
		t.Fatalf("LoadDurableDehydration: %v", err)
	}
	if !restored.MemoryFile().IsAsyncLoading() {
		t.Fatal("durable application memory was eagerly loaded")
	}
	if err := image.Close(); err != nil {
		t.Fatalf("closing capture: %v", err)
	}
	if err := restored.StartPaused(); err != nil {
		t.Fatalf("StartPaused: %v", err)
	}
	restored.Release()
	parent.Release()
}

func TestDehydrationPublishesSparseBranchGraph(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatal(err)
	}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	parent.Pause()
	image, err := parent.CaptureDehydration(parent.SupervisorContext(), kernel.DehydrationCaptureOptions{BranchID: "sparse-dehydrated"})
	if err != nil {
		parent.Release()
		t.Fatal(err)
	}
	repository, err := ttcchunk.OpenMemoryRepository(t.TempDir())
	if err != nil {
		image.Close()
		parent.Release()
		t.Fatal(err)
	}
	manifestID, err := image.PublishSparse(repository)
	if err != nil {
		image.Close()
		parent.Release()
		t.Fatal(err)
	}
	if image.DurableMetadata().SparseManifest != manifestID {
		t.Fatal("durable metadata did not record its sparse manifest")
	}
	manifest, err := repository.LoadManifest(manifestID)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Runs) == 0 || len(manifest.Expand()) != len(image.SparseBindings) {
		t.Fatalf("published manifest has %d runs/%d bindings, want %d bindings", len(manifest.Runs), len(manifest.Expand()), len(image.SparseBindings))
	}
	memoryFiles := make(map[checkpoint.ResourceID]*pgalloc.MemoryFile)
	for _, run := range manifest.Runs {
		if _, ok := memoryFiles[run.ResourceID]; ok {
			continue
		}
		file, err := os.CreateTemp(t.TempDir(), "sparse-target-*.mem")
		if err != nil {
			t.Fatal(err)
		}
		mf, err := pgalloc.NewMemoryFile(file, pgalloc.MemoryFileOpts{
			ResourceID: run.ResourceID, DiskBackedFile: true,
			DisableIMAWorkAround: true, DisableMemoryAccounting: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		memoryFiles[run.ResourceID] = mf
	}
	var restored *kernel.Kernel
	if err := parent.WithDehydrationFilesystemResources(parent.SupervisorContext(), "sparse-dehydrated", func(resources vfs.LiveForkFilesystemResources) error {
		var err error
		restored, err = kernel.LoadSparseDurableDehydration(parent.SupervisorContext(), parent.Platform, image.DurableMetadata(), memoryFiles, repository, kernel.DurableDehydrationLoadOptions{
			Clocks: sentrytime.NewCalibratedClocks(), BranchID: "sparse-dehydrated", FilesystemResources: resources,
		})
		return err
	}); err != nil {
		t.Fatalf("LoadSparseDurableDehydration: %v", err)
	}
	// Boot's tiny test kernel fits in one sparse chunk. Materializing the VDSO
	// parameter page during restore therefore commits that complete 1 MiB
	// residency unit and may finish the loader before this check. Larger images
	// retain demand loaders for their untouched chunks.
	if err := restored.StartPaused(); err != nil {
		t.Fatal(err)
	}
	restored.Release()
	if err := image.Close(); err != nil {
		t.Fatal(err)
	}
	parent.Release()
}

func TestLiveForkRemoteMemoryFileTCP(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	parent.Pause()
	targetMF := newRemoteKernelMemoryFile(t)
	readerFactory := func(snapshot *pgalloc.RemoteSnapshot) (stateio.AsyncReader, error) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		token := ttcremote.SnapshotToken{0x54, 0x54, 0x43, 2}
		server, err := ttcremote.NewTCPPageServer(listener, token, snapshot, 256<<10)
		if err != nil {
			listener.Close()
			return nil, err
		}
		serveDone := make(chan error, 1)
		go func() { serveDone <- server.Serve() }()
		reader, err := ttcremote.NewTCPRemoteReader(listener.Addr().String(), token, 256<<10, 16, 4, 5*time.Second)
		if err != nil {
			server.Close()
			<-serveDone
			return nil, err
		}
		return &ownedKernelTCPReader{AsyncReader: reader, server: server, serveDone: serveDone}, nil
	}
	child, err := parent.LiveFork(parent.SupervisorContext(), kernel.LiveForkOptions{
		Clocks:                  sentrytime.NewCalibratedClocks(),
		RemoteMemoryFile:        targetMF,
		RemotePageReaderFactory: readerFactory,
	})
	if err != nil {
		parent.Release()
		t.Fatalf("TCP remote LiveFork: %v", err)
	}
	if err := child.MemoryFile().AwaitLoadAll(); err != nil {
		t.Fatalf("draining TCP-backed child: %v", err)
	}
	if err := parent.StartPaused(); err != nil {
		t.Fatal(err)
	}
	if err := child.StartPaused(); err != nil {
		t.Fatal(err)
	}
	parent.Unpause()
	child.Unpause()
	parent.Pause()
	child.Pause()
	child.Release()
	parent.Release()
}

func TestPortableDistributedForkCaptureLoadTCP(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	parent.Pause()
	capture, err := parent.CaptureDistributedFork(parent.SupervisorContext(), kernel.DistributedForkCaptureOptions{ChildBranchID: "remote"})
	if err != nil {
		parent.Release()
		t.Fatalf("CaptureDistributedFork: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	token := ttcremote.SnapshotToken{0x54, 0x54, 0x43, 3}
	server, err := ttcremote.NewTCPPageServer(listener, token, capture.Snapshot, 256<<10)
	if err != nil {
		t.Fatal(err)
	}
	capture.Snapshot = nil // ownership transferred to server
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve() }()
	reader, err := ttcremote.NewTCPRemoteReader(listener.Addr().String(), token, pgalloc.SparseChunkBytes, pgalloc.PagesPerSparseChunk(), 4, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ownedReader := &ownedKernelTCPReader{AsyncReader: reader, server: server, serveDone: serveDone}
	targetMF := newRemoteKernelMemoryFile(t)
	child, err := kernel.LoadDistributedFork(parent.SupervisorContext(), parent.Platform, targetMF,
		capture.KernelMetadata, capture.AllocatorMetadata, capture.LoadRanges, capture.HandoffBindings, ownedReader,
		kernel.DistributedForkLoadOptions{
			Clocks: sentrytime.NewCalibratedClocks(), ChildBranchID: "remote",
			FilesystemResources: capture.FilesystemResources, DemandOnly: true,
		})
	if err != nil {
		parent.Release()
		t.Fatalf("LoadDistributedFork: %v", err)
	}
	if child.MemoryFile() == parent.MemoryFile() {
		t.Fatal("portable distributed child shares source MemoryFile")
	}
	if err := child.MemoryFile().AwaitLoadAll(); err != nil {
		t.Fatalf("draining distributed child: %v", err)
	}
	if err := parent.StartPaused(); err != nil {
		t.Fatal(err)
	}
	if err := child.StartPaused(); err != nil {
		t.Fatal(err)
	}
	parent.Unpause()
	child.Unpause()
	parent.Pause()
	child.Pause()
	child.Release()
	parent.Release()
}

func TestDistributedForkHandsLiveReaderToSparseManifest(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	orphan, err := parent.MemoryFile().Allocate(hostarch.PageSize, pgalloc.AllocOpts{Mode: pgalloc.AllocateAndCommit})
	if err != nil {
		parent.Release()
		t.Fatal(err)
	}
	parent.Pause()
	capture, err := parent.CaptureDistributedFork(parent.SupervisorContext(), kernel.DistributedForkCaptureOptions{ChildBranchID: "handoff"})
	if err != nil {
		parent.MemoryFile().DecRef(orphan)
		parent.Release()
		t.Fatalf("CaptureDistributedFork: %v", err)
	}
	parent.MemoryFile().DecRef(orphan)
	if len(capture.HandoffBindings) <= len(capture.MemoryBindings) {
		capture.Close()
		parent.Release()
		t.Fatalf("test setup did not create allocator-only pages: handoff=%d exact=%d", len(capture.HandoffBindings), len(capture.MemoryBindings))
	}
	repositoryDir := t.TempDir()
	publisher, err := ttcchunk.OpenMemoryRepository(repositoryDir)
	if err != nil {
		capture.Close()
		parent.Release()
		t.Fatal(err)
	}
	objects, bindings, err := ttcchunk.ReadSnapshot(capture.HandoffSnapshot, capture.HandoffBindings)
	var manifest ttcchunk.ManifestID
	if err == nil {
		manifest, err = publisher.Publish(objects, bindings)
		if err == nil {
			if closeErr := capture.HandoffSnapshot.Close(); closeErr != nil {
				err = closeErr
			} else {
				capture.HandoffSnapshot = nil
			}
		}
	}
	if closeErr := publisher.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		capture.Close()
		parent.Release()
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		capture.Close()
		parent.Release()
		t.Fatal(err)
	}
	token := ttcremote.SnapshotToken{0x54, 0x54, 0x43, 4}
	server, err := ttcremote.NewTCPPageServer(listener, token, capture.Snapshot, 256<<10)
	if err != nil {
		capture.Close()
		parent.Release()
		t.Fatal(err)
	}
	capture.Snapshot = nil
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve() }()
	reader, err := ttcremote.NewTCPRemoteReader(listener.Addr().String(), token, pgalloc.SparseChunkBytes, pgalloc.PagesPerSparseChunk(), 4, 5*time.Second)
	if err != nil {
		server.Close()
		parent.Release()
		t.Fatal(err)
	}
	targetMF := newRemoteKernelMemoryFile(t)
	child, err := kernel.LoadDistributedFork(parent.SupervisorContext(), parent.Platform, targetMF,
		capture.KernelMetadata, capture.AllocatorMetadata, capture.LoadRanges, capture.HandoffBindings,
		&ownedKernelTCPReader{AsyncReader: reader, server: server, serveDone: serveDone},
		kernel.DistributedForkLoadOptions{
			Clocks: sentrytime.NewCalibratedClocks(), ChildBranchID: "handoff",
			FilesystemResources: capture.FilesystemResources, DemandOnly: true,
		})
	if err != nil {
		parent.Release()
		t.Fatalf("LoadDistributedFork: %v", err)
	}
	repository, err := ttcchunk.OpenMemoryRepository(repositoryDir)
	if err != nil {
		child.Release()
		parent.Release()
		t.Fatal(err)
	}
	if err := child.HandoffDistributedForkMemory(repository, manifest); err != nil {
		child.Release()
		parent.Release()
		t.Fatalf("HandoffDistributedForkMemory: %v", err)
	}
	// Retrying the same command after a lost daemon response is harmless.
	retryRepository, err := ttcchunk.OpenMemoryRepository(repositoryDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := child.HandoffDistributedForkMemory(retryRepository, manifest); err != nil {
		t.Fatalf("retrying HandoffDistributedForkMemory: %v", err)
	}
	child.DrainDistributedForkMemory()
	if err := child.MemoryFile().AwaitLoadAll(); err != nil {
		t.Fatalf("draining sparse handoff: %v", err)
	}
	child.Release()
	parent.Release()
}

func TestSparseDistributedForkLoadsIntoFamilyMemoryFile(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	// Keep an allocator page that is deliberately unreachable from this branch.
	// The legacy transport snapshot includes it, while the sparse snapshot must
	// use the exact branch binding layout.
	orphan, err := parent.MemoryFile().Allocate(hostarch.PageSize, pgalloc.AllocOpts{Mode: pgalloc.AllocateAndCommit})
	if err != nil {
		parent.Release()
		t.Fatal(err)
	}
	parent.Pause()
	capture, err := parent.CaptureDistributedFork(parent.SupervisorContext(), kernel.DistributedForkCaptureOptions{ChildBranchID: "sparse"})
	if err != nil {
		parent.MemoryFile().DecRef(orphan)
		parent.Release()
		t.Fatalf("CaptureDistributedFork: %v", err)
	}
	parent.MemoryFile().DecRef(orphan)
	if capture.SparseSnapshot == nil || capture.Snapshot == nil {
		capture.Close()
		parent.Release()
		t.Fatal("distributed capture omitted its sparse or legacy snapshot")
	}
	if sparseBytes, legacyBytes := capture.SparseSnapshot.StreamBytes(), capture.Snapshot.StreamBytes(); sparseBytes >= legacyBytes {
		capture.Close()
		parent.Release()
		t.Fatalf("sparse snapshot bytes=%d, legacy bytes=%d; unreachable allocator page was not excluded", sparseBytes, legacyBytes)
	}
	repository, err := ttcchunk.OpenMemoryRepository(t.TempDir())
	if err != nil {
		capture.Close()
		parent.Release()
		t.Fatal(err)
	}
	manifestID, err := capture.PublishSparse(repository)
	if err != nil {
		capture.Close()
		parent.Release()
		t.Fatal(err)
	}
	child, err := kernel.LoadSparseDistributedFork(parent.SupervisorContext(), parent.Platform, parent.MemoryFile(), capture.KernelMetadata, repository, manifestID, kernel.DistributedForkLoadOptions{
		Clocks: sentrytime.NewCalibratedClocks(), ChildBranchID: "sparse", FilesystemResources: capture.FilesystemResources,
	})
	if err != nil {
		parent.Release()
		t.Fatalf("LoadSparseDistributedFork: %v", err)
	}
	if child.MemoryFile() != parent.MemoryFile() {
		t.Fatal("sparse child did not join the family MemoryFile")
	}
	if err := child.MemoryFile().AwaitLoadAll(); err != nil {
		t.Fatalf("draining sparse child: %v", err)
	}
	if err := parent.StartPaused(); err != nil {
		t.Fatal(err)
	}
	if err := child.StartPaused(); err != nil {
		t.Fatal(err)
	}
	parent.Unpause()
	child.Unpause()
	parent.Pause()
	child.Pause()
	child.Release()
	parent.Release()
}

func TestPortableDistributedForkCaptureManySharesPages(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	parent.Pause()
	defer parent.Release()
	// Simulate a page owned by another branch in the family-wide MemoryFile.
	// The legacy allocator export includes it, but the branch-scoped binding
	// graph must not.
	orphan, err := parent.MemoryFile().Allocate(hostarch.PageSize, pgalloc.AllocOpts{Mode: pgalloc.AllocateAndCommit})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.MemoryFile().DecRef(orphan)
	orphanPage, ok := parent.MemoryFile().LogicalPageIDAt(orphan.Start)
	if !ok {
		t.Fatal("orphan allocation has no logical page ID")
	}
	capture, err := parent.CaptureDistributedFork(parent.SupervisorContext(), kernel.DistributedForkCaptureOptions{
		ChildBranchIDs: []string{"remote-a", "remote-b"},
	})
	if err != nil {
		t.Fatalf("CaptureDistributedFork: %v", err)
	}
	defer capture.Close()
	for _, binding := range capture.MemoryBindings {
		if binding.Page == orphanPage {
			t.Fatalf("branch-scoped capture included unrelated allocator page %+v", orphanPage)
		}
	}
	legacyIncludedOrphan := false
	for _, load := range capture.LoadRanges {
		if load.Target.Start <= orphan.Start && orphan.End <= load.Target.End {
			legacyIncludedOrphan = true
			break
		}
	}
	if !legacyIncludedOrphan {
		t.Fatal("test setup failed: allocator-wide export did not include unrelated page")
	}
	if capture.Snapshot != nil || len(capture.Snapshots) != 2 {
		t.Fatalf("capture snapshots: singular=%p siblings=%d", capture.Snapshot, len(capture.Snapshots))
	}
	if capture.SparseSnapshot != nil || len(capture.SparseSnapshots) != 2 {
		t.Fatalf("sparse capture snapshots: singular=%p siblings=%d", capture.SparseSnapshot, len(capture.SparseSnapshots))
	}
	// This portion tests legacy sibling acknowledgement in isolation. Sparse
	// consumers have independent reservations over the exact branch graph.
	for i, snapshot := range capture.SparseSnapshots {
		if err := snapshot.Close(); err != nil {
			t.Fatal(err)
		}
		capture.SparseSnapshots[i] = nil
	}
	for i, snapshot := range capture.HandoffSnapshots {
		if err := snapshot.Close(); err != nil {
			t.Fatal(err)
		}
		capture.HandoffSnapshots[i] = nil
	}
	if len(capture.LoadRanges) == 0 || capture.LoadRanges[0].Target.Length() < hostarch.PageSize {
		t.Fatal("capture has no remotely loadable page")
	}
	firstPage := memmap.FileRange{
		Start: capture.LoadRanges[0].Target.Start,
		End:   capture.LoadRanges[0].Target.Start + hostarch.PageSize,
	}
	if err := capture.Snapshots[0].AcknowledgeStreamRange(0, hostarch.PageSize); err != nil {
		t.Fatal(err)
	}
	if parent.MemoryFile().HasUniqueRef(firstPage) {
		t.Fatal("one sibling acknowledgement prematurely released the source page")
	}
	if err := capture.Snapshots[1].AcknowledgeStreamRange(0, hostarch.PageSize); err != nil {
		t.Fatal(err)
	}
	if !parent.MemoryFile().HasUniqueRef(firstPage) {
		t.Fatal("last sibling acknowledgement did not release the source page")
	}
}

func TestPortableDistributedForkKernelSubprocess(t *testing.T) {
	token := ttcremote.SnapshotToken{0x54, 0x54, 0x43, 4}
	if os.Getenv("TTC_KERNEL_TARGET_HELPER") == "1" {
		address := os.Getenv("TTC_KERNEL_TARGET_ADDRESS")
		encoded, err := ttcremote.FetchTCPMetadata(stdcontext.Background(), address, token, 256<<10, 32<<20, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		session, err := ttcremote.UnmarshalSessionMetadata(encoded, ttcremote.SessionMetadataLimits{
			MaxTotalBytes: 32 << 20, MaxKernelBytes: 24 << 20, MaxAllocatorBytes: 8 << 20, MaxLoadRanges: 1 << 16,
		})
		if err != nil {
			t.Fatal(err)
		}
		platformOwner, err := testutil.Boot()
		if err != nil {
			t.Fatal(err)
		}
		platformOwner.PipeMount().IncRef()
		platformOwner.SetHostMount(platformOwner.PipeMount())
		targetMF := newRemoteKernelMemoryFile(t)
		reader, err := ttcremote.NewTCPRemoteReader(address, token, pgalloc.SparseChunkBytes, pgalloc.PagesPerSparseChunk(), 4, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		child, err := kernel.LoadDistributedFork(platformOwner.SupervisorContext(), platformOwner.Platform, targetMF,
			session.KernelMetadata, session.AllocatorMetadata, session.LoadRanges, session.MemoryBindings, reader,
			kernel.DistributedForkLoadOptions{
				Clocks: sentrytime.NewCalibratedClocks(), ChildBranchID: session.TargetBranchID,
				FilesystemResources: make(vfs.LiveForkFilesystemResources), DemandOnly: true,
			})
		if err != nil {
			t.Fatal(err)
		}
		if err := child.MemoryFile().AwaitLoadAll(); err != nil {
			t.Fatal(err)
		}
		if err := child.StartPaused(); err != nil {
			t.Fatal(err)
		}
		child.Unpause()
		child.Pause()
		child.Release()
		platformOwner.Release()
		return
	}

	parent, err := testutil.Boot()
	if err != nil {
		t.Fatal(err)
	}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	parent.Pause()
	capture, err := parent.CaptureDistributedFork(parent.SupervisorContext(), kernel.DistributedForkCaptureOptions{ChildBranchID: "subprocess"})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := (&ttcremote.SessionMetadata{
		FamilyID: "kernel-subprocess", SourceBranchID: "main", TargetBranchID: "subprocess",
		KernelMetadata: capture.KernelMetadata, AllocatorMetadata: capture.AllocatorMetadata, LoadRanges: capture.LoadRanges, MemoryBindings: capture.HandoffBindings,
	}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := ttcremote.NewTCPPageServer(listener, token, capture.Snapshot, 256<<10)
	if err != nil {
		t.Fatal(err)
	}
	streamBytes := capture.Snapshot.StreamBytes()
	capture.Snapshot = nil
	if err := server.SetMetadata(envelope); err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve() }()
	cmd := exec.Command(os.Args[0], "-test.run=^TestPortableDistributedForkKernelSubprocess$")
	cmd.Env = append(os.Environ(), "TTC_KERNEL_TARGET_HELPER=1", "TTC_KERNEL_TARGET_ADDRESS="+listener.Addr().String())
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("distributed Kernel target subprocess: %v\n%s", err, output)
	}
	stats := server.Stats()
	if stats.BytesSent == 0 {
		t.Fatal("target started without demanding any remote memory")
	}
	if stats.BytesSent >= streamBytes {
		t.Logf("small test Kernel demanded its full %d-byte memory stream during startup", streamBytes)
	}
	server.Close()
	if err := <-serveDone; err != nil {
		t.Fatal(err)
	}
	parent.Release()
}

func TestLiveForkClonesPIDFDTarget(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	ctx := parent.SupervisorContext()
	creds := auth.CredentialsFromContext(ctx)
	mntns, err := parent.VFS().NewMountNamespace(ctx, creds, "", tmpfs.Name, &vfs.MountOptions{}, parent)
	if err != nil {
		t.Fatalf("NewMountNamespace: %v", err)
	}
	root := mntns.Root(ctx)
	if _, err := testutil.CreateTask(ctx, "init", parent.GlobalInit(), mntns, root, root); err != nil {
		root.DecRef(ctx)
		mntns.DecRef(ctx)
		t.Fatalf("CreateTask: %v", err)
	}
	root.DecRef(ctx)
	parentTask := parent.GlobalInit().Leader()
	tables := kernel.SyscallTables()
	if len(tables) == 0 {
		t.Fatal("no syscall tables are registered")
	}
	parentTask.TestOnlySetArchAndSyscallTable(arch.New(tables[0].Arch), tables[0])
	parentTID := parent.RootPIDNamespace().IDOfTask(parentTask)
	pidfd, err := parentTask.PIDFDOpen(parentTID, false /* isThread */, false /* nonBlock */)
	if err != nil {
		t.Fatalf("PIDFDOpen: %v", err)
	}
	const pidfdNumber = int32(10)
	if _, err := parentTask.FDTable().NewFDAt(parentTask, pidfdNumber, pidfd, kernel.FDFlags{}); err != nil {
		pidfd.DecRef(parentTask)
		t.Fatalf("installing pidfd: %v", err)
	}
	pidfd.DecRef(parentTask)
	parent.Pause()
	child, err := parent.LiveFork(parent.SupervisorContext(), kernel.LiveForkOptions{
		Clocks: sentrytime.NewCalibratedClocks(),
	})
	if err != nil {
		t.Fatalf("LiveFork with pidfd: %v", err)
	}
	childTask := child.GlobalInit().Leader()
	for name, check := range map[string]struct {
		task *kernel.Task
		want *kernel.Task
	}{
		"parent": {parentTask, parentTask},
		"child":  {childTask, childTask},
	} {
		file := check.task.GetFile(pidfdNumber)
		if file == nil {
			t.Fatalf("%s pidfd is missing", name)
		}
		got, err := kernel.PIDFDTask(file)
		file.DecRef(check.task)
		if err != nil {
			t.Fatalf("resolving %s pidfd: %v", name, err)
		}
		if got != check.want {
			t.Errorf("%s pidfd target=%p, want branch-local task %p", name, got, check.want)
		}
	}

	child.Release()
	parent.Release()
}

// TestLiveForkRunningKernelRepeatedly verifies that restarting the parent after
// one fork leaves all save-time services quiesceable for a later fork.
func TestLiveForkRunningKernelRepeatedly(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	parent.Pause()

	first, err := parent.LiveFork(parent.SupervisorContext(), kernel.LiveForkOptions{
		Clocks: sentrytime.NewCalibratedClocks(),
	})
	if err != nil {
		t.Fatalf("first LiveFork: %v", err)
	}
	if err := parent.StartPaused(); err != nil {
		t.Fatalf("parent.StartPaused: %v", err)
	}
	if err := first.StartPaused(); err != nil {
		t.Fatalf("first.StartPaused: %v", err)
	}
	parent.Unpause()
	first.Unpause()

	parent.Pause()
	second, err := parent.LiveFork(parent.SupervisorContext(), kernel.LiveForkOptions{
		Clocks: sentrytime.NewCalibratedClocks(),
	})
	if err != nil {
		t.Fatalf("second LiveFork: %v", err)
	}
	if err := second.StartPaused(); err != nil {
		t.Fatalf("second.StartPaused: %v", err)
	}
	parent.Unpause()
	second.Unpause()

	parent.Pause()
	first.Pause()
	second.Pause()
	second.Release()
	first.Release()
	parent.Release()
}
