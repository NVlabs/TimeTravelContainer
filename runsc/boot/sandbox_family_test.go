// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package boot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	gcontext "gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/testutil"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	sentryplatform "gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/ttcchunk"
	"gvisor.dev/gvisor/pkg/sentry/ttcremote"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sentry/watchdog"
	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	"gvisor.dev/gvisor/pkg/ttcvolume"
	"gvisor.dev/gvisor/pkg/urpc"
	"gvisor.dev/gvisor/pkg/usermem"
)

// pageRanges returns one MemoryFile's pages as the ranges a branch holds them
// in, one range per page.
func pageRanges(mf *pgalloc.MemoryFile, pages ...uint64) []pgalloc.CapturedMemoryRange {
	ranges := make([]pgalloc.CapturedMemoryRange, 0, len(pages))
	for _, page := range pages {
		start := page * hostarch.PageSize
		ranges = append(ranges, pgalloc.CapturedMemoryRange{MemoryFile: mf, Range: memmap.FileRange{Start: start, End: start + hostarch.PageSize}})
	}
	return ranges
}

// Ranges that overlap or touch, of one MemoryFile, merge; another file's do
// not, at the same offsets.
func TestMergeResidentRanges(t *testing.T) {
	file1, file2 := &pgalloc.MemoryFile{}, &pgalloc.MemoryFile{}
	files := residentFiles{index: make(map[*pgalloc.MemoryFile]int)}
	ranges := append(pageRanges(file1, 4, 1, 2, 9), pageRanges(file2, 1)...)
	ranges = append(ranges, pgalloc.CapturedMemoryRange{MemoryFile: file1, Range: memmap.FileRange{Start: hostarch.PageSize, End: 3 * hostarch.PageSize}})
	got := mergeResidentRanges(ranges, &files)
	page := func(n uint64) uint64 { return n * hostarch.PageSize }
	want := []residentRange{{0, page(1), page(3)}, {0, page(4), page(5)}, {0, page(9), page(10)}, {1, page(1), page(2)}}
	if !slices.Equal(got, want) {
		t.Errorf("merged ranges %+v, want %+v", got, want)
	}
	if len(files.files) != 2 || files.files[0] != file1 || files.files[1] != file2 {
		t.Errorf("files %v, want file1 and file2 in order", files.files)
	}
}

// Branches share a page where their ranges of one MemoryFile overlap: each
// branch's private and shared bytes, the family's unique bytes, and the
// hyperedges of shared pages are exact, and the edges bounded.
func TestResidentMemoryAccountsExactAndBounded(t *testing.T) {
	file1, file2 := &pgalloc.MemoryFile{}, &pgalloc.MemoryFile{}
	accounts := func(ids []string, branchRanges [][]pgalloc.CapturedMemoryRange, limit int) ([]uint64, []uint64, uint64, []ResidentMemorySharingEdge, bool, uint64) {
		files := residentFiles{index: make(map[*pgalloc.MemoryFile]int)}
		merged := make([][]residentRange, len(branchRanges))
		for index, ranges := range branchRanges {
			merged[index] = mergeResidentRanges(ranges, &files)
		}
		return residentMemoryAccounts(ids, merged, limit)
	}
	const page = uint64(hostarch.PageSize)
	ids := []string{"a", "b", "c"}
	// file2's page 3 is another page than file1's page 3.
	branches := [][]pgalloc.CapturedMemoryRange{
		pageRanges(file1, 1, 2, 3),
		pageRanges(file1, 2, 3, 4),
		append(pageRanges(file1, 3, 5), pageRanges(file2, 3)...),
	}
	private, shared, unique, edges, truncated, omitted := accounts(ids, branches, maxResidentMemorySharingEdges)
	if want := []uint64{page, page, 2 * page}; !slices.Equal(private, want) {
		t.Errorf("private bytes %v, want %v", private, want)
	}
	if want := []uint64{2 * page, 2 * page, page}; !slices.Equal(shared, want) {
		t.Errorf("shared bytes %v, want %v", shared, want)
	}
	if unique != 6*page {
		t.Errorf("unique bytes %d, want %d", unique, 6*page)
	}
	if truncated || omitted != 0 || len(edges) != 2 {
		t.Fatalf("edges=%+v truncated=%t omitted=%d", edges, truncated, omitted)
	}
	if got := edges[0]; got.Bytes != page || strings.Join(got.Branches, ",") != "a,b" {
		t.Errorf("first edge=%+v, want one a,b page", got)
	}
	if got := edges[1]; got.Bytes != page || strings.Join(got.Branches, ",") != "a,b,c" {
		t.Errorf("second edge=%+v, want one a,b,c page", got)
	}
	bounded, truncated, omitted := func() ([]ResidentMemorySharingEdge, bool, uint64) {
		_, _, _, edges, truncated, omitted := accounts(ids, branches, 1)
		return edges, truncated, omitted
	}()
	if !truncated || len(bounded) != 1 || omitted != page {
		t.Fatalf("bounded edges=%+v truncated=%t omitted=%d", bounded, truncated, omitted)
	}

	// More branches than a word of bits.
	ids = make([]string, 65)
	branches = make([][]pgalloc.CapturedMemoryRange, len(ids))
	for index := range ids {
		ids[index] = fmt.Sprintf("b%02d", index)
		branches[index] = pageRanges(file1, uint64(1000+index))
	}
	branches[0] = append(branches[0], pageRanges(file1, 2000)...)
	branches[64] = append(branches[64], pageRanges(file1, 2000)...)
	_, shared, unique, edges, truncated, omitted = accounts(ids, branches, maxResidentMemorySharingEdges)
	if truncated || omitted != 0 || len(edges) != 1 || strings.Join(edges[0].Branches, ",") != "b00,b64" || edges[0].Bytes != page {
		t.Fatalf("large-family edges=%+v truncated=%t omitted=%d", edges, truncated, omitted)
	}
	if shared[0] != page || shared[64] != page || shared[1] != 0 || unique != 66*page {
		t.Errorf("large-family shared %d %d %d unique %d", shared[0], shared[64], shared[1], unique)
	}
}

func TestWaitExecSessionConsumesAlreadyExitedTransientSession(t *testing.T) {
	status := uint32(linux.WaitStatusExit(7))
	transient := &execSession{info: ExecSessionInfo{
		ID: "ttc-cp-helper", State: ExecSessionExited, ExitStatus: &status,
	}}
	retained := &execSession{info: ExecSessionInfo{
		ID: "named", State: ExecSessionExited, ExitStatus: &status,
	}, retain: true}
	family := &SandboxFamily{
		loaderBranchID: "main",
		branches: map[string]*sandboxBranch{"main": {
			id: "main", state: BranchRunning,
			execSessions: map[string]*execSession{
				"ttc-cp-helper": transient,
				"named":         retained,
			},
		}},
	}
	got, err := family.WaitExecSession(&ExecSessionArgs{BranchID: "main", ExecID: "ttc-cp-helper"}, nil)
	if err != nil || got != status {
		t.Fatalf("WaitExecSession()=(%d, %v), want (%d, nil)", got, err, status)
	}
	family.mu.Lock()
	_, transientExists := family.branches["main"].execSessions["ttc-cp-helper"]
	_, retainedExists := family.branches["main"].execSessions["named"]
	family.mu.Unlock()
	if transientExists {
		t.Fatal("already-exited transient exec session was retained")
	}
	if !retainedExists {
		t.Fatal("unrelated named exec session was removed")
	}
	if got, err := family.WaitExecSession(&ExecSessionArgs{BranchID: "main", ExecID: "named"}, nil); err != nil || got != status {
		t.Fatalf("retained WaitExecSession()=(%d, %v), want (%d, nil)", got, err, status)
	}
	family.mu.Lock()
	_, retainedExists = family.branches["main"].execSessions["named"]
	family.mu.Unlock()
	if !retainedExists {
		t.Fatal("retained named exec session was consumed")
	}
}

// Versions become bitmaps of 256, from runs of them, whatever MemoryFile holds
// them: a version held twice is one bit.
func TestResidentMemoryVersionChunksBoundFragmentation(t *testing.T) {
	allocator := pgalloc.LogicalAllocatorID{1, 2, 3}
	var runs []residentVersionRun
	for sequence := uint64(0); sequence < 256; sequence += 2 {
		runs = append(runs, residentVersionRun{allocator: allocator, first: sequence, pages: 1})
	}
	// A physical alias of the same stable version must not create another bit.
	runs = append(runs, residentVersionRun{allocator: allocator, first: 10, pages: 1})
	chunks := residentMemoryVersionChunks([]string{"main"}, [][]residentVersionRun{runs})
	if len(chunks) != 1 {
		t.Fatalf("chunks=%+v, want one bounded record", chunks)
	}
	if got := chunks[0]; got.Branch != "main" || got.Chunk != 0 || got.Bitmap != strings.Repeat("55", 32) {
		t.Fatalf("chunk=%+v, want even-slot bitmap", got)
	}
	// A run that crosses chunks sets bits in each.
	chunks = residentMemoryVersionChunks([]string{"main"}, [][]residentVersionRun{{{allocator: allocator, first: 250, pages: 12}}})
	if len(chunks) != 2 || chunks[0].Chunk != 0 || chunks[1].Chunk != 1 ||
		chunks[0].Bitmap != strings.Repeat("00", 31)+"fc" || chunks[1].Bitmap != "3f"+strings.Repeat("00", 31) {
		t.Fatalf("chunks=%+v, want versions 250-255 and 256-261", chunks)
	}
}

type testDistributedBranchResource struct {
	removed []string
}

type testMemoryCompactingPlatform struct {
	sentryplatform.Platform
	compactCalls int
}

var _ sentryplatform.MemoryCompactor = (*testMemoryCompactingPlatform)(nil)

func (p *testMemoryCompactingPlatform) CompactMemory(gcontext.Context) (sentryplatform.MemoryCompaction, error) {
	p.compactCalls++
	return sentryplatform.MemoryCompaction{}, nil
}

func TestSandboxFamilyWaivedLiveForkIsSystematicallyRejected(t *testing.T) {
	family := NewSandboxFamily(&kernel.Kernel{}, SandboxFamilyOptions{LiveForkEligible: false})
	if _, err := family.Fork(&ForkBranchArgs{ID: "child"}); err == nil || !strings.Contains(err.Error(), "waived live fork eligibility") {
		t.Fatalf("Fork() error=%v, want immutable eligibility rejection", err)
	}
	if got := family.List(); len(got) != 1 || got[0].ID != DefaultBranchID {
		t.Fatalf("rejected fork changed branches: %+v", got)
	}
}

func (*testDistributedBranchResource) Fork(string, string) error { return nil }

func (r *testDistributedBranchResource) RemoveBranch(branch string) error {
	r.removed = append(r.removed, branch)
	return nil
}

func TestRemoveDistributedFilesystemBranchDeduplicatesResources(t *testing.T) {
	resource := &testDistributedBranchResource{}
	resources := vfs.LiveForkFilesystemResources{
		checkpoint.ResourceID{Path: "one"}: resource,
		checkpoint.ResourceID{Path: "two"}: resource,
	}
	if err := removeDistributedFilesystemBranch(resources, "remote"); err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(resource.removed), "[remote]"; got != want {
		t.Fatalf("removed branches=%s, want %s", got, want)
	}
}

func TestLoaderKernelTransferRetargetsOCIInit(t *testing.T) {
	old, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot old Kernel: %v", err)
	}
	old.PipeMount().IncRef()
	old.SetHostMount(old.PipeMount())
	old.Pause()
	if err := old.StartPaused(); err != nil {
		t.Fatalf("Start old Kernel: %v", err)
	}
	old.Unpause()
	defer func() {
		old.Pause()
		old.Release()
	}()
	replacement, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot replacement Kernel: %v", err)
	}
	replacement.PipeMount().IncRef()
	replacement.SetHostMount(replacement.PipeMount())
	replacement.Pause()
	if err := replacement.StartPaused(); err != nil {
		t.Fatalf("Start replacement Kernel: %v", err)
	}
	replacement.Unpause()
	defer func() {
		replacement.Pause()
		replacement.Release()
	}()

	const sandboxID = "sandbox"
	l := &Loader{
		k:         old,
		sandboxID: sandboxID,
		processes: map[execID]*execProcess{
			{cid: sandboxID}: {tg: old.GlobalInit()},
		},
	}
	l.setLoaderKernel(replacement, nil)
	if got := l.k; got != replacement {
		t.Fatalf("Loader Kernel=%p, want replacement %p", got, replacement)
	}
	if got := l.processes[execID{cid: sandboxID}].tg; got != replacement.GlobalInit() {
		t.Fatalf("OCI init thread group=%p, want replacement %p", got, replacement.GlobalInit())
	}
}

func TestSandboxFamilyDistributedForkSessionLifecycle(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	parent.Pause()
	if err := parent.StartPaused(); err != nil {
		t.Fatalf("StartPaused: %v", err)
	}
	parent.Unpause()
	defer func() {
		parent.Pause()
		parent.Release()
	}()

	family := NewSandboxFamily(parent, SandboxFamilyOptions{LiveForkEligible: true})
	info, err := family.CaptureDistributedFork(&DistributedForkCaptureArgs{
		SessionID: "tx-1", FamilyID: "family-1", TargetBranchID: "remote-1",
		ListenAddress: "127.0.0.1:0", MaxTransferBytes: 64 << 10,
	})
	if err != nil {
		t.Fatalf("CaptureDistributedFork: %v", err)
	}
	if info.Address == "" || info.MetadataBytes == 0 || info.StreamBytes == 0 {
		t.Fatalf("incomplete session info: %+v", info)
	}
	if parent.IsPaused() {
		t.Fatal("source remained paused after distributed capture")
	}
	encoded, err := ttcremote.FetchTCPMetadata(context.Background(), info.Address, info.Token, 64<<10, 64<<20, 5*time.Second)
	if err != nil {
		t.Fatalf("FetchTCPMetadata: %v", err)
	}
	metadata, err := ttcremote.UnmarshalSessionMetadata(encoded, ttcremote.SessionMetadataLimits{
		MaxTotalBytes: 64 << 20, MaxKernelBytes: 60 << 20,
		MaxAllocatorBytes: 4 << 20, MaxLoadRanges: 1 << 20,
	})
	if err != nil {
		t.Fatalf("UnmarshalSessionMetadata: %v", err)
	}
	if metadata.FamilyID != "family-1" || metadata.SourceBranchID != DefaultBranchID || metadata.TargetBranchID != "remote-1" {
		t.Fatalf("metadata identity: %+v", metadata)
	}
	status, err := family.DistributedForkSessionStatus("tx-1")
	if err != nil {
		t.Fatalf("DistributedForkSessionStatus: %v", err)
	}
	if status.Snapshot.PagesOutstanding == 0 || status.Transport.MetadataBytesSent != uint64(len(encoded)) {
		t.Fatalf("unexpected session status: %+v", status)
	}
	streamFDs, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("Socketpair: %v", err)
	}
	stream := os.NewFile(uintptr(streamFDs[0]), "distributed-page-stream")
	peerStream := os.NewFile(uintptr(streamFDs[1]), "distributed-page-peer")
	defer stream.Close()
	defer peerStream.Close()
	served := make(chan error, 1)
	go func() {
		served <- family.ServeDistributedForkConnection(&DistributedForkServeArgs{
			FilePayload: urpc.FilePayload{Files: []*os.File{stream}}, SessionID: "tx-1",
		})
	}()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("ServeDistributedForkConnection: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ServeDistributedForkConnection occupied its control caller")
	}
	graphStream, err := os.CreateTemp(t.TempDir(), "graph-capture-")
	if err != nil {
		t.Fatal(err)
	}
	graphExport, err := family.ExportDistributedForkSnapshot(&DistributedForkExportArgs{
		FilePayload: urpc.FilePayload{Files: []*os.File{graphStream}}, SessionID: "tx-1",
	})
	if err != nil {
		t.Fatalf("ExportDistributedForkSnapshot graph: %v", err)
	}
	if graphExport.FilesystemBranch != "remote-1" {
		t.Fatalf("graph export filesystem branch=%q, want remote-1", graphExport.FilesystemBranch)
	}
	if _, err := graphStream.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	graphRoot := t.TempDir()
	graph, err := ttcsnapshot.Open(filepath.Join(graphRoot, "graph"))
	if err != nil {
		t.Fatal(err)
	}
	defer graph.Close()
	data, err := ttcsnapshot.OpenLocalDataStore(filepath.Join(graphRoot, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	origin, err := graph.AllocateWorking()
	if err != nil {
		t.Fatal(err)
	}
	graphImported, err := ttcchunk.ImportGraphCapture(graphStream, graph, data, origin, "remote/1/tx-1")
	if err != nil {
		t.Fatalf("ImportGraphCapture: %v", err)
	}
	if len(graphImported.Roots) == 0 || graphImported.Pages == 0 {
		t.Fatalf("empty graph export: %+v", graphImported)
	}
	graphStream.Close()
	if err := family.CancelDistributedForkSession("tx-1"); err != nil {
		t.Fatalf("CancelDistributedForkSession: %v", err)
	}
	if err := family.CancelDistributedForkSession("tx-1"); err != nil {
		t.Fatalf("idempotent CancelDistributedForkSession: %v", err)
	}
	if _, err := family.DistributedForkSessionStatus("tx-1"); err == nil {
		t.Fatal("canceled session remained visible")
	}
	migration, err := family.CaptureDistributedFork(&DistributedForkCaptureArgs{
		SessionID: "migration-1", FamilyID: "family-1", TargetBranchID: "migration-remote",
		ListenAddress: "127.0.0.1:0", MaxTransferBytes: 64 << 10, LeaveSourcePaused: true,
	})
	if err != nil {
		t.Fatalf("migration CaptureDistributedFork: %v", err)
	}
	if migration.Address == "" || !parent.IsPaused() || branchStateForTest(family.List(), DefaultBranchID) != BranchPaused {
		t.Fatalf("migration capture did not retain a paused source: info=%+v branches=%+v", migration, family.List())
	}
	if err := family.CancelDistributedForkSession("migration-1"); err != nil {
		t.Fatalf("cancel migration session: %v", err)
	}
	if err := family.Resume(DefaultBranchID); err != nil {
		t.Fatalf("resume migration source: %v", err)
	}
}

func TestSandboxFamilyDistributedForkCaptureMany(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	parent.Pause()
	if err := parent.StartPaused(); err != nil {
		t.Fatalf("StartPaused: %v", err)
	}
	parent.Unpause()
	defer func() {
		parent.Pause()
		parent.Release()
	}()

	family := NewSandboxFamily(parent, SandboxFamilyOptions{LiveForkEligible: true})
	infos, err := family.CaptureDistributedForkMany(&DistributedForkCaptureManyArgs{
		FamilyID: "family-many", MaxTransferBytes: 64 << 10,
		Children: []DistributedForkCaptureChildArgs{
			{SessionID: "tx-a", TargetBranchID: "remote-a", AdvertiseAddress: "127.0.0.1:10001"},
			{SessionID: "tx-b", TargetBranchID: "remote-b", AdvertiseAddress: "127.0.0.1:10002"},
		},
	})
	if err != nil {
		t.Fatalf("CaptureDistributedForkMany: %v", err)
	}
	if len(infos) != 2 || infos[0].SessionID != "tx-a" || infos[1].SessionID != "tx-b" || infos[0].StreamBytes == 0 {
		t.Fatalf("session infos: %+v", infos)
	}
	if parent.IsPaused() {
		t.Fatal("source remained paused after sibling capture")
	}
	first, err := family.DistributedForkSessionStatus("tx-a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := family.DistributedForkSessionStatus("tx-b")
	if err != nil {
		t.Fatal(err)
	}
	if first.Snapshot.PagesOutstanding == 0 || first.Snapshot.PagesOutstanding != second.Snapshot.PagesOutstanding {
		t.Fatalf("sibling snapshot status: first=%+v second=%+v", first.Snapshot, second.Snapshot)
	}
	if err := family.CancelDistributedForkSession("tx-a"); err != nil {
		t.Fatal(err)
	}
	if status, err := family.DistributedForkSessionStatus("tx-b"); err != nil || status.Snapshot.PagesOutstanding == 0 {
		t.Fatalf("second sibling after first cancellation: status=%+v err=%v", status, err)
	}
	if err := family.CancelDistributedForkSession("tx-b"); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxFamilyDehydrateRehydrateForkedBranch(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	compactingPlatform := &testMemoryCompactingPlatform{Platform: parent.Platform}
	parent.Platform = compactingPlatform
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	parent.Pause()
	if err := parent.StartPaused(); err != nil {
		t.Fatalf("StartPaused: %v", err)
	}
	parent.Unpause()
	family := NewSandboxFamily(parent, SandboxFamilyOptions{LiveForkEligible: true})
	defer func() {
		family.DestroyNonLoaderBranches()
		parent.Pause()
		parent.Release()
	}()

	if _, err := family.Fork(&ForkBranchArgs{ID: "sleeping", Paused: true}); err != nil {
		t.Fatalf("Fork: %v", err)
	}
	// Model both halves of the loader's persistent console follower: its
	// process-lifetime lease must not block memory compaction, while its
	// transient console wait must be canceled before compaction.
	_, finishLifetimeWait, err := family.beginLifetimeControl(DefaultBranchID, false)
	if err != nil {
		t.Fatalf("beginLifetimeControl: %v", err)
	}
	defer finishLifetimeWait()
	_, consoleWait, finishConsoleWait, err := family.beginConsoleWait(DefaultBranchID)
	if err != nil {
		t.Fatalf("beginConsoleWait: %v", err)
	}
	consoleCanceled := make(chan struct{})
	go func() {
		<-consoleWait.cancel
		finishConsoleWait()
		close(consoleCanceled)
	}()
	family.mu.Lock()
	original := family.branches["sleeping"].k
	exitStatus := uint32(linux.WaitStatusExit(7))
	family.branches["sleeping"].execSessions["completed"] = &execSession{info: ExecSessionInfo{
		ID: "completed", ContainerID: "container", PID: 4242, ConsoleID: "console-completed",
		State: ExecSessionExited, ExitStatus: &exitStatus,
	}, retain: true}
	family.mu.Unlock()
	metadata, err := os.CreateTemp(t.TempDir(), "dehydration-*.state")
	if err != nil {
		t.Fatal(err)
	}
	if err := family.Dehydrate("sleeping", metadata); err != nil {
		t.Fatalf("Dehydrate: %v", err)
	}
	select {
	case <-consoleCanceled:
	case <-time.After(time.Second):
		t.Fatal("sibling console follower was not canceled before compaction")
	}
	if compactingPlatform.compactCalls != 1 {
		t.Fatalf("platform compaction calls=%d, want 1", compactingPlatform.compactCalls)
	}
	if stat, err := os.Stat(metadata.Name()); err != nil || stat.Size() <= 48 {
		t.Fatalf("metadata stat=%v err=%v", stat, err)
	}
	infos := family.List()
	if got := branchStateForTest(infos, "sleeping"); got != BranchDehydrated {
		t.Fatalf("state after dehydration=%q, want %q", got, BranchDehydrated)
	}
	if parent.IsPaused() {
		t.Fatal("dehydrating child paused the parent")
	}
	if err := family.Rehydrate("sleeping"); err != nil {
		t.Fatalf("Rehydrate: %v", err)
	}
	family.mu.Lock()
	restored := family.branches["sleeping"].k
	completed := family.branches["sleeping"].execSessions["completed"]
	family.mu.Unlock()
	if restored == nil || restored == original {
		t.Fatalf("rehydrated Kernel=%p, original=%p", restored, original)
	}
	if !restored.IsPaused() {
		t.Fatal("previously paused branch resumed during rehydration")
	}
	if completed == nil || completed.tg != nil || completed.info.State != ExecSessionExited || completed.info.ExitStatus == nil || *completed.info.ExitStatus != exitStatus {
		t.Fatalf("rehydrated completed exec session=%+v", completed)
	}
	if got := branchStateForTest(family.List(), "sleeping"); got != BranchPaused {
		t.Fatalf("state after rehydration=%q, want %q", got, BranchPaused)
	}

	if err := family.Resume("sleeping"); err != nil {
		t.Fatalf("Resume before remote capture: %v", err)
	}
	session, err := family.CaptureDistributedFork(&DistributedForkCaptureArgs{
		SessionID: "dehydration-remote", Source: "sleeping", FamilyID: "family-1",
		TargetBranchID: "remote-child", ListenAddress: "127.0.0.1:0", MaxTransferBytes: 64 << 10,
	})
	if err != nil {
		t.Fatalf("CaptureDistributedFork: %v", err)
	}
	defer family.CancelDistributedForkSession("dehydration-remote")
	remoteMetadata, err := os.CreateTemp(t.TempDir(), "dehydration-remote-*.state")
	if err != nil {
		t.Fatal(err)
	}
	if err := family.Dehydrate("sleeping", remoteMetadata); err != nil {
		t.Fatalf("Dehydrate with remote session: %v", err)
	}
	if _, err := ttcremote.FetchTCPMetadata(context.Background(), session.Address, session.Token, 64<<10, 64<<20, 5*time.Second); err != nil {
		t.Fatalf("remote metadata after source dehydration: %v", err)
	}
	if status, err := family.DistributedForkSessionStatus("dehydration-remote"); err != nil || status.Snapshot.PagesOutstanding == 0 {
		t.Fatalf("remote session after source dehydration: status=%+v err=%v", status, err)
	}
	if err := family.Rehydrate("sleeping"); err != nil {
		t.Fatalf("Rehydrate with remote session: %v", err)
	}
	if got := branchStateForTest(family.List(), "sleeping"); got != BranchRunning {
		t.Fatalf("running state after second rehydration=%q, want %q", got, BranchRunning)
	}
}

// A dehydration image holds the branch's unlinked files open, so the teardown
// of its Kernel must leave them in the filesystem branch: until rehydration,
// or until the image is discarded, when only a filesystem that stays must lose
// them.
func TestSandboxFamilyDehydrationRetainsOrphans(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	parent.Platform = &testMemoryCompactingPlatform{Platform: parent.Platform}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	parent.Pause()
	if err := parent.StartPaused(); err != nil {
		t.Fatalf("StartPaused: %v", err)
	}
	parent.Unpause()
	var events []string
	family := NewSandboxFamily(parent, SandboxFamilyOptions{
		LiveForkEligible: true,
		RetainFilesystemOrphans: func(branch string) func(bool) error {
			events = append(events, "retain "+branch)
			return func(reap bool) error {
				events = append(events, fmt.Sprintf("end %s reap=%t", branch, reap))
				return nil
			}
		},
	})
	defer func() {
		family.DestroyNonLoaderBranches()
		parent.Pause()
		parent.Release()
	}()
	dehydrate := func(id string) {
		t.Helper()
		metadata, err := os.CreateTemp(t.TempDir(), "dehydration-*.state")
		if err != nil {
			t.Fatal(err)
		}
		if err := family.Dehydrate(id, metadata); err != nil {
			t.Fatalf("Dehydrate(%q): %v", id, err)
		}
	}
	expect := func(want ...string) {
		t.Helper()
		if !slices.Equal(events, want) {
			t.Fatalf("events=%q, want %q", events, want)
		}
		events = nil
	}

	filesystemBranches := make(map[string]string)
	for _, id := range []string{"rehydrated", "stopped", "deleted"} {
		if _, err := family.Fork(&ForkBranchArgs{ID: id, Paused: true}); err != nil {
			t.Fatalf("Fork(%q): %v", id, err)
		}
		family.mu.Lock()
		filesystemBranches[id] = family.branches[id].filesystemBranch
		family.mu.Unlock()
	}
	dehydrate("rehydrated")
	expect("retain " + filesystemBranches["rehydrated"])
	if err := family.Rehydrate("rehydrated"); err != nil {
		t.Fatalf("Rehydrate: %v", err)
	}
	expect("end " + filesystemBranches["rehydrated"] + " reap=false")

	dehydrate("stopped")
	expect("retain " + filesystemBranches["stopped"])
	if err := family.Destroy("stopped"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	expect("end " + filesystemBranches["stopped"] + " reap=true")

	dehydrate("deleted")
	expect("retain " + filesystemBranches["deleted"])
	if err := family.DestroyAndReclaim("deleted"); err != nil {
		t.Fatalf("DestroyAndReclaim: %v", err)
	}
	expect("end " + filesystemBranches["deleted"] + " reap=false")
}

func TestSandboxFamilyDehydrateLoaderTransfersRole(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	parent.Pause()
	if err := parent.StartPaused(); err != nil {
		t.Fatalf("StartPaused: %v", err)
	}
	parent.Unpause()
	var loaderChanges []*kernel.Kernel
	family := NewSandboxFamily(parent, SandboxFamilyOptions{
		LiveForkEligible: true,
		OnLoaderKernelChanged: func(k *kernel.Kernel, _ *watchdog.Watchdog) {
			loaderChanges = append(loaderChanges, k)
		},
	})
	if _, err := family.Fork(&ForkBranchArgs{ID: "survivor", Paused: true}); err != nil {
		t.Fatalf("Fork: %v", err)
	}
	family.mu.Lock()
	survivor := family.branches["survivor"].k
	family.mu.Unlock()
	metadata, err := os.CreateTemp(t.TempDir(), "loader-dehydration-*.state")
	if err != nil {
		t.Fatal(err)
	}
	if err := family.Dehydrate(DefaultBranchID, metadata); err != nil {
		t.Fatalf("Dehydrate loader: %v", err)
	}
	if got := family.LoaderBranchID(); got != "survivor" {
		t.Fatalf("loader branch after dehydration=%q, want survivor", got)
	}
	if len(loaderChanges) != 1 || loaderChanges[0] != survivor {
		t.Fatalf("loader callbacks=%p, want [%p]", loaderChanges, survivor)
	}
	if got := branchStateForTest(family.List(), DefaultBranchID); got != BranchDehydrated {
		t.Fatalf("dehydrated loader state=%q, want %q", got, BranchDehydrated)
	}
	if got := branchStateForTest(family.List(), "survivor"); got != BranchPaused {
		t.Fatalf("survivor state=%q, want %q", got, BranchPaused)
	}
	if err := family.Destroy(DefaultBranchID); err != nil {
		t.Fatalf("Destroy dehydrated branch: %v", err)
	}
	survivor.Kill(linux.WaitStatusTerminationSignal(linux.SIGKILL))
	survivor.Unpause()
	survivor.WaitExited()
	survivor.Release()
}

// A capture taken without a metadata file can be exported later, more than
// once, and still rehydrated: ttcd exports a resident dehydration only when the
// family's last live branch goes, and repeats an export whose recording failed.
func TestSandboxFamilyExportsAResidentCaptureRepeatedly(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	parent.Pause()
	if err := parent.StartPaused(); err != nil {
		t.Fatalf("StartPaused: %v", err)
	}
	parent.Unpause()
	family := NewSandboxFamily(parent, SandboxFamilyOptions{LiveForkEligible: true})
	defer func() {
		family.DestroyNonLoaderBranches()
		parent.Pause()
		parent.Release()
	}()
	if _, err := family.Fork(&ForkBranchArgs{ID: "resident", Paused: true}); err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if err := family.Dehydrate("resident", nil); err != nil {
		t.Fatalf("Dehydrate: %v", err)
	}
	export := func(withMetadata bool) ([]byte, *BranchDehydrationExportInfo, *os.File) {
		t.Helper()
		stream, err := os.CreateTemp(t.TempDir(), "stream-*")
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		files := []*os.File{stream}
		var metadata *os.File
		if withMetadata {
			if metadata, err = os.CreateTemp(t.TempDir(), "metadata-*"); err != nil {
				t.Fatal(err)
			}
			files = append(files, metadata)
		}
		info, err := family.ExportDehydration(&BranchDehydrationExportArgs{
			FilePayload: urpc.FilePayload{Files: files}, ID: "resident",
		})
		if err != nil {
			t.Fatalf("ExportDehydration: %v", err)
		}
		data, err := os.ReadFile(stream.Name())
		if err != nil {
			t.Fatal(err)
		}
		return data, info, metadata
	}
	first, info, _ := export(false)
	if len(first) == 0 || len(info.Capture.Memories) == 0 || info.Capture.FilesystemBranch == "" {
		t.Fatalf("first export: %d stream bytes, capture %+v", len(first), info.Capture)
	}
	second, _, metadata := export(true)
	defer metadata.Close()
	if string(first) != string(second) {
		t.Fatalf("a repeated export wrote %d bytes that differ from the first export's %d", len(second), len(first))
	}
	if stat, err := metadata.Stat(); err != nil || stat.Size() <= 48 {
		t.Fatalf("metadata written with the export: stat=%v err=%v", stat, err)
	}
	if err := family.Rehydrate("resident"); err != nil {
		t.Fatalf("Rehydrate after exports: %v", err)
	}
	if got := branchStateForTest(family.List(), "resident"); got != BranchPaused {
		t.Fatalf("state after rehydration=%q, want %q", got, BranchPaused)
	}
}

// WaitDefaultExit keeps the Sentry up after its last live branch exits for as
// long as a branch is dehydrated, because the capture may exist nowhere else.
// (Its final status comes from a loader init that this test kernel lacks; the
// daemon-level tests exercise that exit.)
func TestSandboxFamilyHoldsWhileABranchIsDehydrated(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	parent.Pause()
	if err := parent.StartPaused(); err != nil {
		t.Fatalf("StartPaused: %v", err)
	}
	parent.Unpause()
	family := NewSandboxFamily(parent, SandboxFamilyOptions{LiveForkEligible: true})
	defer func() {
		family.DestroyNonLoaderBranches()
		parent.Pause()
		parent.Release()
	}()
	holds := func() bool {
		family.mu.Lock()
		defer family.mu.Unlock()
		return family.holdsDehydrationLocked()
	}
	if _, err := family.Fork(&ForkBranchArgs{ID: "held", Paused: true}); err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if holds() {
		t.Fatal("a family with only live branches held its exit")
	}
	if err := family.Dehydrate("held", nil); err != nil {
		t.Fatalf("Dehydrate: %v", err)
	}
	if !holds() {
		t.Fatal("a family holding a dehydrated branch would exit")
	}
	if err := family.Rehydrate("held"); err != nil {
		t.Fatalf("Rehydrate: %v", err)
	}
	if holds() {
		t.Fatal("a rehydrated branch still held the family's exit")
	}
	if err := family.Dehydrate("held", nil); err != nil {
		t.Fatalf("Dehydrate again: %v", err)
	}
	if err := family.Destroy("held"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if holds() {
		t.Fatal("a destroyed dehydrated branch still held the family's exit")
	}
}

func branchStateForTest(infos []BranchInfo, id string) BranchState {
	for _, info := range infos {
		if info.ID == id {
			return info.State
		}
	}
	return "missing"
}

func TestVolumeBranchIDIsSandboxScoped(t *testing.T) {
	first := &SandboxFamily{volumeBranchScope: "sandbox-a"}
	second := &SandboxFamily{volumeBranchScope: "sandbox-b"}
	if got := first.volumeBranchID(DefaultBranchID); got == ttcvolume.MainBranch {
		t.Fatalf("a newly-created branch named main reused the initial volume pointer %q", got)
	}
	if got, want := first.volumeBranchID("child"), first.volumeBranchID("child"); got != want {
		t.Fatalf("volume branch ID is not deterministic: got %q, want %q", got, want)
	}
	if first.volumeBranchID("child") == first.volumeBranchID("other") {
		t.Fatal("distinct visible branch IDs collided")
	}
	if first.volumeBranchID("child") == second.volumeBranchID("child") {
		t.Fatal("identically named branches in distinct sandboxes collided")
	}
	if got := (&SandboxFamily{}).volumeBranchID("child"); got != "child" {
		t.Fatalf("unscoped test family branch ID=%q, want child", got)
	}
}

func TestSandboxFamilyInitialBranchIdentity(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	family := NewSandboxFamily(parent, SandboxFamilyOptions{
		InitialBranchID:         "restarted-child",
		InitialBranchParentID:   "former-parent",
		InitialFilesystemBranch: "filesystem-child",
	})
	branches := family.List()
	if len(branches) != 1 || branches[0].ID != "restarted-child" || branches[0].ParentID != "former-parent" || branches[0].FilesystemBranch != "filesystem-child" || family.LoaderBranchID() != "restarted-child" {
		t.Fatalf("initial branch identity: loader=%q branches=%+v", family.LoaderBranchID(), branches)
	}
}

func TestSandboxFamilyDestroyReclaimsReleasedFilesystemBranch(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	parent.Pause()
	if err := parent.StartPaused(); err != nil {
		t.Fatalf("StartPaused: %v", err)
	}
	parent.Unpause()
	defer func() {
		parent.Pause()
		parent.Release()
	}()

	var reclaimed []string
	family := NewSandboxFamily(parent, SandboxFamilyOptions{
		LiveForkEligible: true,
		ReclaimFilesystemBranch: func(branch string) error {
			reclaimed = append(reclaimed, branch)
			return nil
		},
	})
	child, err := family.Fork(&ForkBranchArgs{ID: "short-lived", Paused: true})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if err := family.Destroy(child.ID); err != nil {
		t.Fatalf("Destroy without reclaim: %v", err)
	}
	if len(reclaimed) != 0 {
		t.Fatalf("runtime-only destroy reclaimed filesystem branches=%q", reclaimed)
	}
	child, err = family.Fork(&ForkBranchArgs{ID: "reclaimed", Paused: true})
	if err != nil {
		t.Fatalf("second Fork: %v", err)
	}
	if err := family.DestroyAndReclaim(child.ID); err != nil {
		t.Fatalf("DestroyAndReclaim: %v", err)
	}
	if len(reclaimed) != 1 || reclaimed[0] != child.FilesystemBranch {
		t.Fatalf("reclaimed filesystem branches=%q, want [%q]", reclaimed, child.FilesystemBranch)
	}
}

func TestDestroyLoaderPrefersLivePeerOverExitedPeer(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	parent.Pause()
	if err := parent.StartPaused(); err != nil {
		t.Fatalf("StartPaused: %v", err)
	}
	parent.Unpause()
	family := NewSandboxFamily(parent, SandboxFamilyOptions{LiveForkEligible: true})
	if _, err := family.Fork(&ForkBranchArgs{ID: "a-exited", Paused: true}); err != nil {
		t.Fatalf("Fork exited candidate: %v", err)
	}
	if _, err := family.Fork(&ForkBranchArgs{ID: "z-live", Paused: true}); err != nil {
		t.Fatalf("Fork live candidate: %v", err)
	}
	family.mu.Lock()
	family.branches["a-exited"].state = BranchExited
	family.mu.Unlock()
	if err := family.Destroy(DefaultBranchID); err != nil {
		t.Fatalf("Destroy loader: %v", err)
	}
	if got := family.LoaderBranchID(); got != "z-live" {
		t.Fatalf("loader branch=%q, want live peer z-live", got)
	}
	if err := family.Destroy("a-exited"); err != nil {
		t.Fatalf("Destroy exited peer: %v", err)
	}
	family.mu.Lock()
	remaining := family.branches["z-live"].k
	family.mu.Unlock()
	remaining.Kill(linux.WaitStatusTerminationSignal(linux.SIGKILL))
	remaining.Unpause()
	remaining.WaitExited()
	remaining.Release()
}

func TestReplaceDefaultKernelBeforeRestoreLoad(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	family := NewSandboxFamily(parent, SandboxFamilyOptions{LiveForkEligible: true})
	replacement := &kernel.Kernel{}
	if err := family.ReplaceDefaultKernel(replacement); err != nil {
		t.Fatal(err)
	}
	if err := family.SetDefaultFilesystemBranch("remote-filesystem-branch"); err != nil {
		t.Fatal(err)
	}
	family.mu.Lock()
	branch := family.branches[DefaultBranchID]
	family.mu.Unlock()
	if branch.k != replacement || branch.state != BranchPaused || branch.filesystemBranch != "remote-filesystem-branch" {
		t.Fatalf("replacement branch=%+v", branch)
	}
}

func TestReplaceLoaderBranchPreservesPeerIdentity(t *testing.T) {
	placeholder, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	family := NewSandboxFamily(placeholder, SandboxFamilyOptions{})
	replacement := &kernel.Kernel{}
	if err := family.ReplaceLoaderBranchFromDehydration("experiment-b", "experiment-a", "volume-b", replacement, nil, []ExecSessionInfo{{PID: 1, State: ExecSessionRunning}}); err == nil {
		t.Fatal("replacement with invalid exec-session metadata succeeded")
	}
	if got := family.LoaderBranchID(); got != DefaultBranchID {
		t.Fatalf("failed replacement changed loader branch to %q", got)
	}
	if got := family.List(); len(got) != 1 || got[0].ID != DefaultBranchID {
		t.Fatalf("failed replacement changed branch set: %+v", got)
	}
	if err := family.ReplaceLoaderBranchFromDehydration("experiment-b", "experiment-a", "volume-b", replacement, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := family.LoaderBranchID(); got != "experiment-b" {
		t.Fatalf("loader branch=%q, want experiment-b", got)
	}
	family.mu.Lock()
	restored := family.branches["experiment-b"]
	_, retainedMain := family.branches[DefaultBranchID]
	branchCount := len(family.branches)
	family.mu.Unlock()
	if branchCount != 1 || restored == nil || restored.parentID != "experiment-a" {
		t.Fatalf("branch identity changed during loader replacement: count=%d branch=%+v", branchCount, restored)
	}
	if retainedMain {
		t.Fatal("cold loader replacement implicitly woke or renamed main")
	}
}

func TestSandboxFamilyForkFailureCleanup(t *testing.T) {
	stages := []SandboxForkStage{
		SandboxForkStagePlaceholderRegistered,
		SandboxForkStageSourcePaused,
		SandboxForkStageKernelCloned,
		SandboxForkStageChildStarted,
		SandboxForkStageBranchReady,
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
			if err := parent.StartPaused(); err != nil {
				t.Fatalf("StartPaused: %v", err)
			}
			parent.Unpause()

			injected := errors.New("injected sandbox-family fork failure")
			failOnce := true
			family := NewSandboxFamily(parent, SandboxFamilyOptions{
				LiveForkEligible: true,
				TestOnlyForkFailureInjector: func(completed SandboxForkStage) error {
					if failOnce && completed == stage {
						failOnce = false
						return injected
					}
					return nil
				},
			})
			if child, err := family.Fork(&ForkBranchArgs{ID: "failed", Paused: true}); child != nil || !errors.Is(err, injected) {
				t.Fatalf("Fork failure at %s: got child=%+v err=%v", stage, child, err)
			}
			if got := family.List(); len(got) != 1 || got[0].ID != DefaultBranchID || got[0].State != BranchRunning {
				t.Fatalf("family after failure at %s: %+v", stage, got)
			}
			child, err := family.Fork(&ForkBranchArgs{ID: "retry", Paused: true})
			if err != nil {
				t.Fatalf("Fork retry after %s: %v", stage, err)
			}
			if child.State != BranchPaused {
				t.Fatalf("retry child after %s: %+v", stage, child)
			}
			if err := family.Destroy(child.ID); err != nil {
				t.Fatalf("Destroy retry after %s: %v", stage, err)
			}
			parent.Pause()
			parent.Release()
		})
	}
}

func TestSandboxFamilyConsoleWaitReadinessAndLifecycleIndependence(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	parent.Pause()
	if err := parent.StartPaused(); err != nil {
		t.Fatalf("StartPaused: %v", err)
	}
	parent.Unpause()
	defer func() {
		parent.Pause()
		parent.Release()
	}()

	const consoleID = "readiness-test"
	fdTable, err := parent.NewInternalConsoleFDTable(parent.SupervisorContext(), consoleID)
	if err != nil {
		t.Fatalf("NewInternalConsoleFDTable: %v", err)
	}
	fdTableReleased := false
	defer func() {
		if !fdTableReleased {
			fdTable.DecRef(parent.SupervisorContext())
		}
		_ = parent.RemoveInternalConsole(parent.SupervisorContext(), consoleID)
	}()
	family := NewSandboxFamily(parent, SandboxFamilyOptions{LiveForkEligible: true})

	var timeoutResult ConsoleWaitResult
	if err := family.ConsoleWait(&ConsoleWaitArgs{
		BranchID: DefaultBranchID, ConsoleID: consoleID,
		Want: kernel.InternalConsoleReadiness{Stdout: true}, Timeout: 5 * time.Millisecond,
	}, &timeoutResult); err != nil {
		t.Fatalf("ConsoleWait timeout: %v", err)
	}
	if !timeoutResult.TimedOut || timeoutResult.Ready.Stdout {
		t.Fatalf("ConsoleWait timeout result: %+v", timeoutResult)
	}

	var stdinResult ConsoleWaitResult
	if err := family.ConsoleWait(&ConsoleWaitArgs{
		BranchID: DefaultBranchID, ConsoleID: consoleID,
		Want: kernel.InternalConsoleReadiness{Stdin: true}, Timeout: 0,
	}, &stdinResult); err != nil {
		t.Fatalf("ConsoleWait stdin: %v", err)
	}
	if stdinResult.TimedOut || !stdinResult.Ready.Stdin {
		t.Fatalf("ConsoleWait stdin result: %+v", stdinResult)
	}

	cancelledWait := make(chan error, 1)
	go func() {
		var result ConsoleWaitResult
		cancelledWait <- family.ConsoleWait(&ConsoleWaitArgs{
			BranchID: DefaultBranchID, ConsoleID: consoleID,
			Want: kernel.InternalConsoleReadiness{Stdout: true}, Timeout: time.Second,
		}, &result)
	}()
	time.Sleep(20 * time.Millisecond)
	branch, err := family.branch(DefaultBranchID)
	if err != nil {
		t.Fatalf("finding default branch: %v", err)
	}
	branch.opMu.Lock()
	family.cancelConsoleWaits(branch)
	branch.opMu.Unlock()
	select {
	case err := <-cancelledWait:
		if err != nil {
			t.Fatalf("cancelled ConsoleWait: %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("console waiter cancellation did not join")
	}

	waitDone := make(chan error, 1)
	go func() {
		var result ConsoleWaitResult
		err := family.ConsoleWait(&ConsoleWaitArgs{
			BranchID: DefaultBranchID, ConsoleID: consoleID,
			Want: kernel.InternalConsoleReadiness{Stdout: true}, Timeout: time.Second,
		}, &result)
		if err == nil && (result.TimedOut || !result.Ready.Stdout) {
			err = fmt.Errorf("unexpected readiness result: %+v", result)
		}
		waitDone <- err
	}()
	time.Sleep(20 * time.Millisecond)
	select {
	case err := <-waitDone:
		t.Fatalf("ConsoleWait returned before readiness: %v", err)
	default:
	}

	pauseDone := make(chan error, 1)
	go func() { pauseDone <- family.Pause(DefaultBranchID) }()
	select {
	case err := <-pauseDone:
		if err != nil {
			t.Fatalf("Pause during ConsoleWait: %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("Pause blocked behind ConsoleWait")
	}
	if err := family.Resume(DefaultBranchID); err != nil {
		t.Fatalf("Resume during ConsoleWait: %v", err)
	}

	stdout, _ := fdTable.Get(1)
	if stdout == nil {
		t.Fatal("internal console stdout FD is missing")
	}
	const output = "readiness-output"
	written, err := stdout.Write(parent.SupervisorContext(), usermem.BytesIOSequence([]byte(output)), vfs.WriteOptions{})
	stdout.DecRef(parent.SupervisorContext())
	if err != nil || written != int64(len(output)) {
		t.Fatalf("writing internal console stdout: wrote %d, err=%v", written, err)
	}
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("ConsoleWait wakeup: %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("ConsoleWait did not wake after guest output became readable")
	}
	data, eof, wouldBlock, err := parent.ReadInternalConsole(parent.SupervisorContext(), consoleID, kernel.InternalConsoleStdout, len(output))
	if err != nil || eof || wouldBlock || string(data) != output {
		t.Fatalf("reading readiness output: data=%q eof=%t wouldBlock=%t err=%v", data, eof, wouldBlock, err)
	}
	fdTable.DecRef(parent.SupervisorContext())
	fdTableReleased = true
	data, eof, wouldBlock, err = parent.ReadInternalConsole(parent.SupervisorContext(), consoleID, kernel.InternalConsoleStdout, 1)
	if err != nil || !eof || wouldBlock || len(data) != 0 {
		t.Fatalf("reading closed internal console: data=%q eof=%t wouldBlock=%t err=%v", data, eof, wouldBlock, err)
	}
}

func TestSandboxFamilyExecLeaseSerializesLifecycle(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	parent.Pause()
	if err := parent.StartPaused(); err != nil {
		t.Fatalf("StartPaused: %v", err)
	}
	parent.Unpause()
	defer func() {
		parent.Pause()
		parent.Release()
	}()

	family := NewSandboxFamily(parent, SandboxFamilyOptions{LiveForkEligible: true})
	child, err := family.Fork(&ForkBranchArgs{ID: "exec-target"})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	holdExec := func() func() {
		_, release, err := family.beginRunningControl(child.ID)
		if err != nil {
			t.Fatalf("beginRunningControl: %v", err)
		}
		return release
	}
	assertBlocked := func(done <-chan error, operation string) {
		t.Helper()
		select {
		case err := <-done:
			t.Fatalf("%s completed while exec lease was held: %v", operation, err)
		case <-time.After(50 * time.Millisecond):
		}
	}
	wait := func(done <-chan error, operation string) error {
		t.Helper()
		select {
		case err := <-done:
			return err
		case <-time.After(5 * time.Second):
			t.Fatalf("%s remained blocked after exec lease release", operation)
			return nil
		}
	}

	release := holdExec()
	resumeDone := make(chan error, 1)
	go func() { resumeDone <- family.Resume(child.ID) }()
	assertBlocked(resumeDone, "resume")
	if err := family.Pause(DefaultBranchID); err != nil {
		t.Fatalf("pausing sibling while exec lease held: %v", err)
	}
	if err := family.Resume(DefaultBranchID); err != nil {
		t.Fatalf("resuming sibling while exec lease held: %v", err)
	}
	release()
	if err := wait(resumeDone, "resume"); err == nil {
		t.Fatal("concurrent resume unexpectedly accepted a running branch")
	}

	release = holdExec()
	pauseDone := make(chan error, 1)
	go func() { pauseDone <- family.Pause(child.ID) }()
	assertBlocked(pauseDone, "pause")
	release()
	if err := wait(pauseDone, "pause"); err != nil {
		t.Fatalf("pause after exec lease: %v", err)
	}
	if err := family.Resume(child.ID); err != nil {
		t.Fatalf("resuming target: %v", err)
	}

	release = holdExec()
	forkDone := make(chan error, 1)
	go func() {
		_, err := family.Fork(&ForkBranchArgs{Source: child.ID, ID: "grandchild", Paused: true})
		forkDone <- err
	}()
	assertBlocked(forkDone, "fork")
	release()
	if err := wait(forkDone, "fork"); err != nil {
		t.Fatalf("fork after exec lease: %v", err)
	}
	if err := family.Destroy("grandchild"); err != nil {
		t.Fatalf("destroying grandchild: %v", err)
	}

	release = holdExec()
	destroyDone := make(chan error, 1)
	go func() { destroyDone <- family.Destroy(child.ID) }()
	assertBlocked(destroyDone, "destroy")
	release()
	if err := wait(destroyDone, "destroy"); err != nil {
		t.Fatalf("destroy after exec lease: %v", err)
	}
}

func TestSandboxFamilyPausedManagementControlPreservesPause(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	parent.Pause()
	if err := parent.StartPaused(); err != nil {
		t.Fatalf("StartPaused: %v", err)
	}
	parent.Unpause()
	defer func() {
		parent.Pause()
		parent.Release()
	}()

	family := NewSandboxFamily(parent, SandboxFamilyOptions{LiveForkEligible: true})
	childInfo, err := family.Fork(&ForkBranchArgs{ID: "paused-copy", Paused: true})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	child, release, err := family.beginPausedManagementControl(childInfo.ID)
	if err != nil {
		t.Fatalf("beginPausedManagementControl: %v", err)
	}
	if child.state != BranchPaused {
		t.Fatalf("branch state during management control = %q, want paused", child.state)
	}
	if child.k.IsPaused() {
		t.Fatal("management execution window retained the TaskSet-wide stop")
	}

	resumeDone := make(chan error, 1)
	go func() {
		resumeDone <- family.Resume(childInfo.ID)
	}()
	select {
	case err := <-resumeDone:
		t.Fatalf("Resume completed during paused management execution: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	release()
	select {
	case err := <-resumeDone:
		if err != nil {
			t.Fatalf("Resume after paused management execution: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Resume remained blocked after paused management execution")
	}
	if child.k.IsPaused() {
		t.Fatal("resumed child remained paused")
	}
	if err := family.Destroy(childInfo.ID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
}

func TestSandboxFamilyBranchLifecycle(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	parent.Pause()
	if err := parent.StartPaused(); err != nil {
		t.Fatalf("StartPaused: %v", err)
	}
	parent.Unpause()

	family := NewSandboxFamily(parent, SandboxFamilyOptions{LiveForkEligible: true})
	initialBranches := family.List()
	if len(initialBranches) != 1 || initialBranches[0].State != BranchRunning || parent.IsPaused() {
		branches := initialBranches
		t.Fatalf("unexpected initial running family: branches=%+v paused=%t", branches, parent.IsPaused())
	}
	mainCreatedAt := initialBranches[0].CreatedAt
	if mainCreatedAt.IsZero() {
		t.Fatal("default branch has a zero creation timestamp")
	}
	child, err := family.Fork(&ForkBranchArgs{ID: "child", Paused: true})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if child.ID != "child" || child.ParentID != DefaultBranchID || child.State != BranchPaused {
		t.Fatalf("Fork returned %+v", child)
	}
	if child.CreatedAt.IsZero() || child.CreatedAt.Before(mainCreatedAt) {
		t.Errorf("child creation time=%v, main=%v", child.CreatedAt, mainCreatedAt)
	}
	if _, _, err := family.beginRunningControl("child"); err == nil {
		t.Fatal("beginRunningControl succeeded for paused child")
	}
	if child.ForkMetrics.Total <= 0 || child.ForkMetrics.Kernel.MetadataStream <= 0 || child.ForkMetrics.ChildStart <= 0 {
		t.Fatalf("incomplete fork metrics: %+v", child.ForkMetrics)
	}
	branches := family.List()
	if len(branches) != 2 || branches[0].ID != "child" || branches[1].ID != DefaultBranchID {
		t.Fatalf("List after fork: %+v", branches)
	}
	if branches[1].State != BranchRunning {
		t.Fatalf("source state after fork: %s", branches[1].State)
	}
	if !branches[0].CreatedAt.Equal(child.CreatedAt) || !branches[1].CreatedAt.Equal(mainCreatedAt) {
		t.Errorf("List creation timestamps=%+v, child=%v main=%v", branches, child.CreatedAt, mainCreatedAt)
	}
	if !branches[0].SharesMemory || !branches[1].SharesMemory {
		t.Errorf("forked branches do not report shared CoW backing: %+v", branches)
	}

	if err := family.Resume("child"); err != nil {
		t.Fatalf("Resume child: %v", err)
	}
	if branch, done, err := family.beginRunningControl("child"); err != nil {
		t.Fatalf("beginRunningControl running child: %v", err)
	} else {
		if branch.opMu.TryLock() {
			branch.opMu.Unlock()
			t.Fatal("beginRunningControl did not retain the branch operation lock")
		}
		done()
		if !branch.opMu.TryLock() {
			t.Fatal("beginRunningControl done did not release the branch operation lock")
		}
		branch.opMu.Unlock()
	}
	if err := family.Pause("child"); err != nil {
		t.Fatalf("Pause child: %v", err)
	}
	grandchild, err := family.Fork(&ForkBranchArgs{Source: "child", ID: "grandchild", Paused: true})
	if err != nil {
		t.Fatalf("Fork grandchild: %v", err)
	}
	if grandchild.ParentID != "child" {
		t.Fatalf("grandchild parent: %q", grandchild.ParentID)
	}
	// Descendants own all references needed by their cloned Kernels. Destroying
	// an ancestor must not constrain their lifetime; display lineage is spliced
	// through the removed node.
	if err := family.Destroy("child"); err != nil {
		t.Fatalf("Destroy child before grandchild: %v", err)
	}
	branches = family.List()
	if len(branches) != 2 || branches[0].ID != "grandchild" || branches[0].ParentID != DefaultBranchID {
		t.Fatalf("List after ancestor destroy: %+v", branches)
	}
	if _, err := family.Fork(&ForkBranchArgs{ID: "child", Paused: true}); err != nil {
		t.Fatalf("reuse deleted branch name: %v", err)
	}
	if err := family.Destroy("child"); err != nil {
		t.Fatalf("destroy reused child: %v", err)
	}
	if err := family.Resume("grandchild"); err != nil {
		t.Fatalf("Resume grandchild after ancestor destroy: %v", err)
	}
	if err := family.Pause("grandchild"); err != nil {
		t.Fatalf("Pause grandchild after ancestor destroy: %v", err)
	}
	greatGrandchild, err := family.Fork(&ForkBranchArgs{Source: "grandchild", ID: "great-grandchild", Paused: true})
	if err != nil {
		t.Fatalf("Fork from grandchild after ancestor destroy: %v", err)
	}
	if greatGrandchild.ParentID != "grandchild" {
		t.Fatalf("great-grandchild parent: %q", greatGrandchild.ParentID)
	}
	if err := family.Destroy("grandchild"); err != nil {
		t.Fatalf("Destroy grandchild before great-grandchild: %v", err)
	}
	if err := family.Resume("great-grandchild"); err != nil {
		t.Fatalf("Resume great-grandchild after ancestor destroy: %v", err)
	}
	if err := family.PrepareStop(DefaultBranchID); err != nil {
		t.Fatalf("PrepareStop loader branch: %v", err)
	}
	if got := family.LoaderBranchID(); got != "great-grandchild" {
		t.Fatalf("loader branch after PrepareStop=%q, want great-grandchild", got)
	}
	if err := family.Destroy(DefaultBranchID); err != nil {
		t.Fatalf("Destroy loader branch while peer remains: %v", err)
	}
	if got := family.LoaderBranchID(); got != "great-grandchild" {
		t.Fatalf("loader branch after deleting main=%q, want great-grandchild", got)
	}
	if got := family.List(); len(got) != 1 || got[0].ID != "great-grandchild" || got[0].ParentID != "" {
		t.Fatalf("List after deleting main: %+v", got)
	}
	if err := family.Destroy("great-grandchild"); err == nil {
		t.Fatal("destroying the final family branch succeeded without family removal")
	}

	family.mu.Lock()
	remaining := family.branches["great-grandchild"].k
	family.mu.Unlock()
	remaining.Kill(linux.WaitStatusTerminationSignal(linux.SIGKILL))
	remaining.WaitExited()
	remaining.Release()
}

func TestSandboxFamilyForkMany(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	parent.Pause()
	if err := parent.StartPaused(); err != nil {
		t.Fatalf("StartPaused: %v", err)
	}
	parent.Unpause()
	family := NewSandboxFamily(parent, SandboxFamilyOptions{LiveForkEligible: true})

	infos, err := family.ForkMany(&ForkBranchesArgs{
		IDs:    []string{"sibling-a", "sibling-b", "sibling-c"},
		Paused: true,
	})
	if err != nil {
		t.Fatalf("ForkMany: %v", err)
	}
	if len(infos) != 3 {
		t.Fatalf("ForkMany returned %d branches, want 3", len(infos))
	}
	wantIDs := []string{"sibling-a", "sibling-b", "sibling-c"}
	for i, info := range infos {
		if info.ID != wantIDs[i] {
			t.Errorf("branch %d ID=%q, want %q", i, info.ID, wantIDs[i])
		}
		if info.ParentID != DefaultBranchID || info.State != BranchPaused {
			t.Errorf("branch %d = %+v", i, info)
		}
		if info.ForkMetrics.Kernel.MetadataStream <= 0 {
			t.Errorf("branch %d has no metadata timing: %+v", i, info.ForkMetrics)
		}
		if i != 0 && (info.ForkMetrics.Kernel.Validation != 0 || info.ForkMetrics.Kernel.ParentPreparation != 0) {
			t.Errorf("sibling %d repeated source work: %+v", i, info.ForkMetrics.Kernel)
		}
		if info.ForkMetrics.SourceDowntime != infos[0].ForkMetrics.SourceDowntime {
			t.Errorf("sibling %d downtime=%v, primary=%v", i, info.ForkMetrics.SourceDowntime, infos[0].ForkMetrics.SourceDowntime)
		}
	}
	if parent.IsPaused() {
		t.Fatal("ForkMany left the running source paused")
	}
	for _, info := range infos {
		if err := family.Destroy(info.ID); err != nil {
			t.Fatalf("Destroy(%q): %v", info.ID, err)
		}
	}
	parent.Pause()
	parent.Release()
}

func TestSandboxFamilyRepeatedBranchTreesDoNotLeakPages(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	parent.Pause()
	if err := parent.StartPaused(); err != nil {
		t.Fatalf("StartPaused: %v", err)
	}
	parent.Unpause()
	family := NewSandboxFamily(parent, SandboxFamilyOptions{LiveForkEligible: true})
	// Exclude one-time source-side lazy initialization from the leak baseline.
	// A retained branch reference would continue growing after this warm-up.
	if _, err := family.Fork(&ForkBranchArgs{ID: "warm-up", Paused: true}); err != nil {
		t.Fatalf("warm-up fork: %v", err)
	}
	if err := family.Destroy("warm-up"); err != nil {
		t.Fatalf("warm-up destroy: %v", err)
	}
	baselineUsage, err := parent.MemoryFile().TotalUsage()
	if err != nil {
		t.Fatalf("initial MemoryFile.TotalUsage: %v", err)
	}

	for round := 0; round < 20; round++ {
		childID := fmt.Sprintf("child-%d", round)
		siblingID := fmt.Sprintf("sibling-%d", round)
		grandchildID := fmt.Sprintf("grandchild-%d", round)
		greatGrandchildID := fmt.Sprintf("great-grandchild-%d", round)
		if _, err := family.Fork(&ForkBranchArgs{ID: childID, Paused: true}); err != nil {
			t.Fatalf("round %d child fork: %v", round, err)
		}
		if _, err := family.Fork(&ForkBranchArgs{ID: siblingID, Paused: true}); err != nil {
			t.Fatalf("round %d sibling fork: %v", round, err)
		}
		if _, err := family.Fork(&ForkBranchArgs{Source: childID, ID: grandchildID, Paused: true}); err != nil {
			t.Fatalf("round %d grandchild fork: %v", round, err)
		}
		if _, err := family.Fork(&ForkBranchArgs{Source: grandchildID, ID: greatGrandchildID, Paused: true}); err != nil {
			t.Fatalf("round %d great-grandchild fork: %v", round, err)
		}

		// Destroy two ancestors while their descendants remain, then remove
		// the surviving leaves in a different lineage order.
		for _, id := range []string{childID, grandchildID, siblingID, greatGrandchildID} {
			if err := family.Destroy(id); err != nil {
				t.Fatalf("round %d destroy %q: %v", round, id, err)
			}
		}
		if got := family.List(); len(got) != 1 || got[0].ID != DefaultBranchID {
			t.Fatalf("round %d branches after destroy: %+v", round, got)
		}
	}

	// Kernel teardown can hand its last pages to MemoryFile's asynchronous
	// releaser. Wait for that handoff rather than sampling a transient page;
	// usage below the warm baseline is valid because parent-side lazy pages may
	// also have become reclaimable.
	deadline := time.Now().Add(time.Second)
	for {
		finalUsage, err := parent.MemoryFile().TotalUsage()
		if err != nil {
			t.Fatalf("final MemoryFile.TotalUsage: %v", err)
		}
		if finalUsage <= baselineUsage {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("live branch page usage leaked: before=%d after=%d", baselineUsage, finalUsage)
		}
		time.Sleep(time.Millisecond)
	}
	parent.Pause()
	parent.Release()
}

func TestSandboxFamilyQuarantineIsIndependentInheritedBranchState(t *testing.T) {
	parent, err := testutil.Boot()
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	parent.Pause()
	if err := parent.StartPaused(); err != nil {
		parent.Release()
		t.Fatalf("StartPaused: %v", err)
	}
	parent.Unpause()
	family := NewSandboxFamily(parent, SandboxFamilyOptions{LiveForkEligible: true})
	defer func() {
		for _, info := range family.List() {
			if info.ID != DefaultBranchID {
				_ = family.Destroy(info.ID)
			}
		}
		parent.Pause()
		parent.Release()
	}()

	if err := family.SetQuarantine(DefaultBranchID, true); err != nil {
		t.Fatalf("quarantine source: %v", err)
	}
	child, err := family.Fork(&ForkBranchArgs{ID: "child", Paused: true})
	if err != nil {
		t.Fatalf("fork quarantined source: %v", err)
	}
	if !child.Quarantined {
		t.Fatalf("child did not inherit quarantine: %+v", child)
	}
	if err := family.SetQuarantine("child", false); err != nil {
		t.Fatalf("unquarantine child: %v", err)
	}
	var sourceSeen, childSeen bool
	for _, info := range family.List() {
		switch info.ID {
		case DefaultBranchID:
			sourceSeen = true
			if !info.Quarantined {
				t.Fatal("unquarantining child changed source")
			}
		case "child":
			childSeen = true
			if info.Quarantined {
				t.Fatal("child remained quarantined")
			}
			if info.State != BranchPaused {
				t.Fatalf("changing quarantine changed child lifecycle to %s", info.State)
			}
		}
	}
	if !sourceSeen || !childSeen {
		t.Fatalf("family branches after quarantine transitions: %+v", family.List())
	}
}

func BenchmarkSandboxFamilyFork(b *testing.B) {
	parent, err := testutil.Boot()
	if err != nil {
		b.Fatalf("Boot: %v", err)
	}
	parent.PipeMount().IncRef()
	parent.SetHostMount(parent.PipeMount())
	parent.Pause()
	if err := parent.StartPaused(); err != nil {
		b.Fatalf("StartPaused: %v", err)
	}
	parent.Unpause()
	family := NewSandboxFamily(parent, SandboxFamilyOptions{LiveForkEligible: true})

	var totals SandboxForkMetrics
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := fmt.Sprintf("bench-%d", i)
		child, err := family.Fork(&ForkBranchArgs{ID: id, Paused: true})
		if err != nil {
			b.Fatalf("Fork %d: %v", i, err)
		}
		b.StopTimer()
		totals.SourcePause += child.ForkMetrics.SourcePause
		totals.SourceDowntime += child.ForkMetrics.SourceDowntime
		totals.Kernel.Validation += child.ForkMetrics.Kernel.Validation
		totals.Kernel.ParentPreparation += child.ForkMetrics.Kernel.ParentPreparation
		totals.Kernel.MetadataStream += child.ForkMetrics.Kernel.MetadataStream
		totals.Kernel.ChildRestore += child.ForkMetrics.Kernel.ChildRestore
		totals.ChildStart += child.ForkMetrics.ChildStart
		totals.SourceResume += child.ForkMetrics.SourceResume
		totals.Total += child.ForkMetrics.Total
		if err := family.Destroy(id); err != nil {
			b.Fatalf("Destroy %d: %v", i, err)
		}
		b.StartTimer()
	}
	b.StopTimer()
	perOp := func(d time.Duration) float64 { return float64(d.Nanoseconds()) / float64(b.N) }
	b.ReportMetric(perOp(totals.SourcePause), "source-pause-ns/op")
	b.ReportMetric(perOp(totals.SourceDowntime), "source-downtime-ns/op")
	b.ReportMetric(perOp(totals.Kernel.Validation), "validation-ns/op")
	b.ReportMetric(perOp(totals.Kernel.ParentPreparation), "preparation-ns/op")
	b.ReportMetric(perOp(totals.Kernel.MetadataStream), "metadata-ns/op")
	b.ReportMetric(perOp(totals.Kernel.ChildRestore), "restore-ns/op")
	b.ReportMetric(perOp(totals.ChildStart), "child-start-ns/op")
	b.ReportMetric(perOp(totals.SourceResume), "source-resume-ns/op")
	b.ReportMetric(perOp(totals.Total), "api-ns/op")

	parent.Pause()
	parent.Release()
}
