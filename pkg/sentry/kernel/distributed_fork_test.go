// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kernel

import (
	"bytes"
	"strings"
	"testing"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	"gvisor.dev/gvisor/pkg/sentry/ttcchunk"
)

// A graph handoff raced a remote load's last read: the loader exited, the
// switch closed the graph's reader, and the handoff closed it again, which
// panicked the Sentry.
func TestRemotePageReaderSwitchAfterTheLoadStopped(t *testing.T) {
	newReader := func() *stateio.IOReader {
		return stateio.NewIOReader(bytes.NewReader(make([]byte, hostarch.PageSize)), hostarch.PageSize, 1, 1)
	}
	finished, err := pgalloc.StartAsyncPagesFileLoadWithOpts(newReader(), nil, nil, pgalloc.AsyncPagesFileLoadOpts{DemandOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	finished.MemoryFilesDone()
	if err := finished.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := switchRemotePageReader(finished, newReader()); err != nil {
		t.Errorf("switching the reader of a finished load: %v", err)
	}

	cancelled, err := pgalloc.StartAsyncPagesFileLoadWithOpts(newReader(), nil, nil, pgalloc.AsyncPagesFileLoadOpts{DemandOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	cancelled.Cancel()
	if err := cancelled.Wait(); err == nil {
		t.Fatal("a cancelled load reported no error")
	}
	if err := switchRemotePageReader(cancelled, newReader()); err == nil {
		t.Error("switching the reader of a cancelled load succeeded")
	}
}

// An import whose pages were all of units that earlier imports installed
// gives its branch no loader of its own, only theirs: its graph handoff waits
// for them, as the daemon retries it, and then has nothing of its own to
// switch. It failed instead, and a family's removal after a node had migrated
// its branches into their source's Sentry never ended.
func TestGraphHandoffOfABranchWithOnlyInheritedLoads(t *testing.T) {
	reader := stateio.NewIOReader(bytes.NewReader(make([]byte, hostarch.PageSize)), hostarch.PageSize, 1, 1)
	inherited, err := pgalloc.StartAsyncPagesFileLoadWithOpts(reader, nil, nil, pgalloc.AsyncPagesFileLoadOpts{DemandOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		inherited.Cancel()
		_ = inherited.Wait()
	}()
	k := &Kernel{auxiliaryRemotePageLoads: []*pgalloc.AsyncPagesFileLoad{inherited}}
	if err := k.HandoffDistributedForkMemoryGraph(nil, nil, ttcchunk.Manifest{}, 1); err == nil || !strings.Contains(err.Error(), "inherited distributed memory load is not durable yet") {
		t.Fatalf("handoff while an inherited load loads: %v, want it to wait for the load", err)
	}
	inherited.MemoryFilesDone()
	if err := inherited.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := k.HandoffDistributedForkMemoryGraph(nil, nil, ttcchunk.Manifest{}, 1); err != nil {
		t.Fatalf("handoff once the inherited load is done: %v", err)
	}
	if k.remoteHandoffSnapshot != 1 {
		t.Errorf("handed off to snapshot %d, want 1", k.remoteHandoffSnapshot)
	}
	// A Kernel that loads nothing is no remote fork's target.
	if err := (&Kernel{}).HandoffDistributedForkMemoryGraph(nil, nil, ttcchunk.Manifest{}, 1); err == nil {
		t.Error("handoff of a Kernel that loads nothing succeeded")
	}
}
