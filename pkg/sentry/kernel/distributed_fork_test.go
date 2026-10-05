// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kernel

import (
	"bytes"
	"testing"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
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
