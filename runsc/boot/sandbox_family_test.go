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

package boot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/testutil"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/mm"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	sentryplatform "gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/ttcchunk"
	"gvisor.dev/gvisor/pkg/sentry/ttcremote"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/ttcvolume"
	"gvisor.dev/gvisor/pkg/urpc"
	"gvisor.dev/gvisor/pkg/usermem"
)

func TestResidentMemorySharingEdgesExactAndBounded(t *testing.T) {
	page := func(sequence uint64) mm.ResidentPageVersion {
		return mm.ResidentPageVersion{Version: pgalloc.PageVersionID{Sequence: sequence}}
	}
	set := func(sequences ...uint64) map[mm.ResidentPageVersion]struct{} {
		pages := make(map[mm.ResidentPageVersion]struct{}, len(sequences))
		for _, sequence := range sequences {
			pages[page(sequence)] = struct{}{}
		}
		return pages
	}
	edges, truncated, omitted, counts := residentMemorySharingEdges(
		[]string{"a", "b", "c"},
		[]map[mm.ResidentPageVersion]struct{}{set(1, 2, 3), set(2, 3, 4), set(3, 5)},
		maxResidentMemorySharingEdges,
	)
	if truncated || omitted != 0 || len(edges) != 2 {
		t.Fatalf("edges=%+v truncated=%t omitted=%d", edges, truncated, omitted)
	}
	if got := counts[page(1)]; got != 1 {
		t.Errorf("private page references=%d, want 1", got)
	}
	if got := counts[page(3)]; got != 3 {
		t.Errorf("three-way page references=%d, want 3", got)
	}
	if got := edges[0]; got.Bytes != uint64(hostarch.PageSize) || strings.Join(got.Branches, ",") != "a,b" {
		t.Errorf("first edge=%+v, want one a,b page", got)
	}
	if got := edges[1]; got.Bytes != uint64(hostarch.PageSize) || strings.Join(got.Branches, ",") != "a,b,c" {
		t.Errorf("second edge=%+v, want one a,b,c page", got)
	}
	bounded, truncated, omitted, _ := residentMemorySharingEdges(
		[]string{"a", "b", "c"},
		[]map[mm.ResidentPageVersion]struct{}{set(1, 2, 3), set(2, 3, 4), set(3, 5)},
		1,
	)
	if !truncated || len(bounded) != 1 || omitted != uint64(hostarch.PageSize) {
		t.Fatalf("bounded edges=%+v truncated=%t omitted=%d", bounded, truncated, omitted)
	}
	ids := make([]string, 65)
	pages := make([]map[mm.ResidentPageVersion]struct{}, len(ids))
	for index := range ids {
		ids[index] = fmt.Sprintf("b%02d", index)
		pages[index] = set(uint64(1000 + index))
	}
	pages[0][page(2000)] = struct{}{}
	pages[64][page(2000)] = struct{}{}
	largeEdges, truncated, omitted, _ := residentMemorySharingEdges(ids, pages, maxResidentMemorySharingEdges)
	if truncated || omitted != 0 || len(largeEdges) != 1 || strings.Join(largeEdges[0].Branches, ",") != "b00,b64" {
		t.Fatalf("large-family edges=%+v truncated=%t omitted=%d", largeEdges, truncated, omitted)
	}
}

func TestResidentMemoryVersionRunsCompressAndDeduplicate(t *testing.T) {
	allocator := pgalloc.LogicalAllocatorID{1, 2, 3}
	file1 := &pgalloc.MemoryFile{}
	file2 := &pgalloc.MemoryFile{}
	pages := map[mm.ResidentPageVersion]struct{}{
		{File: file1, Version: pgalloc.PageVersionID{Allocator: allocator, Sequence: 7}}:  {},
		{File: file2, Version: pgalloc.PageVersionID{Allocator: allocator, Sequence: 7}}:  {},
		{File: file1, Version: pgalloc.PageVersionID{Allocator: allocator, Sequence: 8}}:  {},
		{File: file1, Version: pgalloc.PageVersionID{Allocator: allocator, Sequence: 10}}: {},
	}
	runs := residentMemoryVersionRuns([]string{"main"}, []map[mm.ResidentPageVersion]struct{}{pages})
	if len(runs) != 2 || runs[0].Branch != "main" || runs[0].FirstSequence != 7 || runs[0].Pages != 2 || runs[1].FirstSequence != 10 || runs[1].Pages != 1 {
		t.Fatalf("runs=%+v", runs)
	}
}

type testDistributedBranchResource struct {
	removed []string
}

type testMemoryCompactingPlatform struct {
	sentryplatform.Platform
	compactCalls int
}

func (p *testMemoryCompactingPlatform) CompactMemory(context.Context) (sentryplatform.MemoryCompaction, error) {
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

	family := NewSandboxFamily(parent, SandboxFamilyOptions{})
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
	repositoryRoot := t.TempDir()
	repository, err := ttcchunk.OpenMemoryRepository(repositoryRoot)
	if err != nil {
		t.Fatal(err)
	}
	repositoryDir, err := os.Open(repositoryRoot)
	if err != nil {
		t.Fatal(err)
	}
	exported, err := family.ExportDistributedForkSnapshot(&DistributedForkExportArgs{
		FilePayload: urpc.FilePayload{Files: []*os.File{repositoryDir}}, SessionID: "tx-1", Sparse: true,
	})
	repositoryDir.Close()
	if err != nil {
		t.Fatalf("ExportDistributedForkSnapshot sparse: %v", err)
	}
	if exported.Manifest == (ttcchunk.ManifestID{}) || exported.FilesystemBranch != "remote-1" {
		t.Fatalf("incomplete sparse export: %+v", exported)
	}
	if _, err := repository.LoadManifest(exported.Manifest); err != nil {
		t.Fatalf("loading exported sparse manifest: %v", err)
	}
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

	family := NewSandboxFamily(parent, SandboxFamilyOptions{})
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
	family := NewSandboxFamily(parent, SandboxFamilyOptions{})
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
	family.mu.Unlock()
	if restored == nil || restored == original {
		t.Fatalf("rehydrated Kernel=%p, original=%p", restored, original)
	}
	if !restored.IsPaused() {
		t.Fatal("previously paused branch resumed during rehydration")
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
	family := NewSandboxFamily(parent, SandboxFamilyOptions{})
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
	family := NewSandboxFamily(parent, SandboxFamilyOptions{})
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
	family := NewSandboxFamily(parent, SandboxFamilyOptions{})

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

	family := NewSandboxFamily(parent, SandboxFamilyOptions{})
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

	family := NewSandboxFamily(parent, SandboxFamilyOptions{})
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

	family := NewSandboxFamily(parent, SandboxFamilyOptions{})
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
	family := NewSandboxFamily(parent, SandboxFamilyOptions{})

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
	family := NewSandboxFamily(parent, SandboxFamilyOptions{})
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
	family := NewSandboxFamily(parent, SandboxFamilyOptions{})

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
