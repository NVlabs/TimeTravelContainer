// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kernel_test

import (
	stdcontext "context"
	"errors"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/abi"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/fspath"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/testutil"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/tmpfs"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	sentryplatform "gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	_ "gvisor.dev/gvisor/pkg/sentry/syscalls/linux"
	sentrytime "gvisor.dev/gvisor/pkg/sentry/time"
	"gvisor.dev/gvisor/pkg/sentry/ttcremote"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/usermem"
)

type captureCoherencePlatform struct {
	sentryplatform.Platform
	flushes int
}

var _ sentryplatform.CheckpointCoherence = (*captureCoherencePlatform)(nil)

func (p *captureCoherencePlatform) FlushForCheckpoint(context.Context) error {
	p.flushes++
	return nil
}

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

// residentBytes returns the bytes of the ranges ResidentFileRanges returns,
// which here do not overlap.
func residentBytes(t *testing.T, k *kernel.Kernel) uint64 {
	t.Helper()
	ranges, err := k.ResidentFileRanges()
	if err != nil {
		t.Fatalf("ResidentFileRanges: %v", err)
	}
	var bytes uint64
	for _, r := range ranges {
		bytes += r.Range.Length()
	}
	return bytes
}

func TestResidentFileRangesIncludesUnmappedTmpfs(t *testing.T) {
	k, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	// testutil.Boot leaves the externally supplied host mount unset. Kernel
	// teardown requires one, so use the ordinary test stand-in before Release.
	k.PipeMount().IncRef()
	k.SetHostMount(k.PipeMount())
	defer k.Release()
	ctx := k.SupervisorContext()
	creds := auth.CredentialsFromContext(ctx)
	mntns, err := k.VFS().NewMountNamespace(ctx, creds, "", tmpfs.Name, &vfs.MountOptions{}, k)
	if err != nil {
		t.Fatalf("NewMountNamespace: %v", err)
	}
	defer mntns.DecRef(ctx)
	root := mntns.Root(ctx)
	defer root.DecRef(ctx)

	before := residentBytes(t, k)
	fd, err := k.VFS().OpenAt(ctx, creds, &vfs.PathOperation{
		Root: root, Start: root, Path: fspath.Parse("resident"),
	}, &vfs.OpenOptions{Flags: linux.O_CREAT | linux.O_RDWR, Mode: 0600})
	if err != nil {
		t.Fatalf("OpenAt: %v", err)
	}
	data := make([]byte, 3*hostarch.PageSize)
	for i := range data {
		data[i] = byte(i + 1)
	}
	if n, err := fd.PWrite(ctx, usermem.BytesIOSequence(data), 0, vfs.WriteOptions{}); err != nil || n != int64(len(data)) {
		fd.DecRef(ctx)
		t.Fatalf("PWrite: n=%d err=%v", n, err)
	}
	fd.DecRef(ctx) // The file remains linked, but has no FD or VMA reference.

	if got, want := residentBytes(t, k)-before, uint64(3*hostarch.PageSize); got != want {
		t.Fatalf("resident increase after unmapped tmpfs write: got %d bytes, want %d", got, want)
	}
}

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
				Clocks: sentrytime.NewCalibratedClocks(false),
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
				Clocks: sentrytime.NewCalibratedClocks(false),
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
		Clocks:  sentrytime.NewCalibratedClocks(false),
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

// A dehydration holds the pages its branch reaches, and none that only a live
// sibling holds, which the sibling writes in place and frees while the
// capture waits. The capture still restores.
func TestDehydrationHoldsOnlyItsBranchsPages(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	parent.Pause()
	child, err := parent.LiveFork(parent.SupervisorContext(), kernel.LiveForkOptions{
		Clocks: sentrytime.NewCalibratedClocks(false),
	})
	if err != nil {
		parent.Release()
		t.Fatalf("LiveFork: %v", err)
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
	// A page the parent holds alone, as one it wrote after the fork.
	mf := parent.MemoryFile()
	own, err := mf.Allocate(hostarch.PageSize, pgalloc.AllocOpts{Kind: usage.Anonymous})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	dehydrated, err := child.CaptureDehydration(child.SupervisorContext(), kernel.DehydrationCaptureOptions{BranchID: "dehydrated"})
	if err != nil {
		t.Fatalf("CaptureDehydration: %v", err)
	}
	child.Release()
	if !mf.HasUniqueRef(own) {
		t.Error("the dehydration holds a page only its live sibling refers to")
	}
	mf.DecRef(own)
	restored, err := kernel.LoadDehydration(parent.SupervisorContext(), parent.Platform, dehydrated, kernel.DehydrationLoadOptions{
		Clocks: sentrytime.NewCalibratedClocks(false), BranchID: "dehydrated",
	})
	if err != nil {
		t.Fatalf("LoadDehydration: %v", err)
	}
	if err := restored.StartPaused(); err != nil {
		t.Fatalf("starting the rehydrated branch: %v", err)
	}
	restored.Release()
	parent.Release()
}

func TestLiveForkFlushesPlatformExecutionCaches(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	coherent := &captureCoherencePlatform{Platform: parent.Platform}
	parent.Platform = coherent
	parent.Pause()

	child, err := parent.LiveFork(parent.SupervisorContext(), kernel.LiveForkOptions{
		Clocks: sentrytime.NewCalibratedClocks(false),
	})
	if err != nil {
		parent.Release()
		t.Fatalf("LiveFork: %v", err)
	}
	if coherent.flushes != 1 {
		t.Errorf("platform state-capture flushes = %d, want 1", coherent.flushes)
	}
	child.Release()
	parent.Release()
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
		Clocks:           sentrytime.NewCalibratedClocks(false),
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
		Clocks: sentrytime.NewCalibratedClocks(false), BranchID: "remote",
	})
	if err != nil {
		t.Fatalf("rehydrating distributed target: %v", err)
	}
	if err := restored.StartPaused(); err != nil {
		t.Fatalf("starting rehydrated distributed target: %v", err)
	}
	grandchild, err := restored.LiveFork(restored.SupervisorContext(), kernel.LiveForkOptions{
		Clocks: sentrytime.NewCalibratedClocks(false),
	})
	if err != nil {
		t.Fatalf("live-forking rehydrated distributed target: %v", err)
	}
	if err := grandchild.StartPaused(); err != nil {
		t.Fatalf("starting live fork of rehydrated distributed target: %v", err)
	}
	grandchild.Release()
	restored.Release()
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
		Clocks:                  sentrytime.NewCalibratedClocks(false),
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
		capture.KernelMetadata, capture.AllocatorMetadata, capture.LoadRanges, capture.MemoryBindings, ownedReader,
		kernel.DistributedForkLoadOptions{
			Clocks: sentrytime.NewCalibratedClocks(false), ChildBranchID: "remote",
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

func TestDistributedForkRepeatedImportSharesMemoryFile(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatal(err)
	}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	parent.Pause()
	capture, err := parent.CaptureDistributedFork(parent.SupervisorContext(), kernel.DistributedForkCaptureOptions{ChildBranchIDs: []string{"remote-one", "remote-two"}})
	if err != nil {
		parent.Release()
		t.Fatal(err)
	}
	// The target installs the pages the bindings name: the capture has no use
	// for the family's allocator.
	if len(capture.MemoryBindings) == 0 || len(capture.AllocatorMetadata) != 0 {
		t.Errorf("the capture has %d bindings and %d bytes of allocator metadata, want bindings only", len(capture.MemoryBindings), len(capture.AllocatorMetadata))
	}
	targetMF := newRemoteKernelMemoryFile(t)
	children := make([]*kernel.Kernel, 0, 2)
	for i, id := range []string{"remote-one", "remote-two"} {
		reader := pgalloc.NewLoopbackRemoteReader(capture.Snapshots[i], pgalloc.SparseChunkBytes, pgalloc.PagesPerSparseChunk(), 4)
		capture.Snapshots[i] = nil
		child, err := kernel.LoadDistributedFork(parent.SupervisorContext(), parent.Platform, targetMF,
			capture.KernelMetadata, capture.AllocatorMetadata, capture.LoadRanges, capture.MemoryBindings, reader,
			kernel.DistributedForkLoadOptions{
				Clocks: sentrytime.NewCalibratedClocks(false), ChildBranchID: id,
				FilesystemResources: capture.FilesystemResources, DemandOnly: true,
			})
		if err != nil {
			t.Fatalf("import %d: %v", i, err)
		}
		if child.MemoryFile() != targetMF {
			t.Fatalf("import %d used a different family MemoryFile", i)
		}
		if err := child.StartPaused(); err != nil {
			t.Fatal(err)
		}
		children = append(children, child)
	}
	// The second import reused the first import's still-unresolved physical
	// chunks and therefore has no reader of its own for those chunks. Transfer
	// the owning import's readers before release, as SandboxFamily.Destroy does.
	children[0].TransferDistributedForkMemorySources(children[1])
	children[0].Release()
	children = children[1:]
	if err := children[0].MemoryFile().AwaitLoadAll(); err != nil {
		t.Fatal(err)
	}
	if err := parent.StartPaused(); err != nil {
		t.Fatal(err)
	}
	for _, child := range children {
		child.Release()
	}
	parent.Release()
	// Each import took an owner of its own: once they and the caller let go,
	// the file is destroyed.
	targetMF.Destroy()
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the target MemoryFile outlived its owners")
			}
		}()
		targetMF.IncOwner()
	}()
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
	// No branch-scoped stream may include it.
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
	for _, load := range capture.LoadRanges {
		if load.Target.Start <= orphan.Start && orphan.End <= load.Target.End {
			t.Fatal("branch-scoped transport included unrelated allocator page")
		}
	}
	if capture.Snapshot != nil || len(capture.Snapshots) != 2 {
		t.Fatalf("capture snapshots: singular=%p siblings=%d", capture.Snapshot, len(capture.Snapshots))
	}
	if capture.SparseSnapshot != nil || len(capture.SparseSnapshots) != 2 {
		t.Fatalf("sparse capture snapshots: singular=%p siblings=%d", capture.SparseSnapshot, len(capture.SparseSnapshots))
	}
	// Test sibling acknowledgement in isolation. Sparse consumers have
	// independent reservations over the same exact branch graph.
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
				Clocks: sentrytime.NewCalibratedClocks(false), ChildBranchID: session.TargetBranchID,
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
		KernelMetadata: capture.KernelMetadata, AllocatorMetadata: capture.AllocatorMetadata, LoadRanges: capture.LoadRanges, MemoryBindings: capture.MemoryBindings,
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
	table, ok := kernel.LookupSyscallTable(abi.Linux, arch.Host)
	if !ok {
		t.Fatalf("no syscall table is registered for %v", arch.Host)
	}
	parentTask.TestOnlySetArchAndSyscallTable(arch.New(table.Arch), table)
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
		Clocks: sentrytime.NewCalibratedClocks(false),
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
		Clocks: sentrytime.NewCalibratedClocks(false),
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
		Clocks: sentrytime.NewCalibratedClocks(false),
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
