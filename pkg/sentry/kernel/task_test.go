// Copyright 2018 The gVisor Authors.
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

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kernel

import (
	"testing"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/kernel/sched"
)

func TestTaskLimitReached(t *testing.T) {
	for _, test := range []struct {
		name      string
		max, live uint64
		want      bool
	}{
		{name: "unlimited", max: 0, live: 100, want: false},
		{name: "below limit", max: 3, live: 2, want: false},
		{name: "at limit", max: 3, live: 3, want: true},
		{name: "above limit", max: 3, live: 4, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ts := TaskSet{maxLiveTasks: test.max, liveTasks: uint32(test.live)}
			if got := ts.taskLimitReachedLocked(); got != test.want {
				t.Errorf("taskLimitReachedLocked()=%t, want %t", got, test.want)
			}
		})
	}
}

func TestTaskCPU(t *testing.T) {
	for _, test := range []struct {
		mask sched.CPUSet
		tid  ThreadID
		cpu  int32
	}{
		{
			mask: []byte{0xff},
			tid:  1,
			cpu:  1,
		},
		{
			mask: []byte{0xff},
			tid:  10,
			cpu:  2,
		},
		{
			// more than 8 cpus.
			mask: []byte{0xff, 0xff},
			tid:  10,
			cpu:  10,
		},
		{
			// missing the first cpu.
			mask: []byte{0xfe},
			tid:  1,
			cpu:  2,
		},
		{
			mask: []byte{0xfe},
			tid:  10,
			cpu:  4,
		},
		{
			// missing the fifth cpu.
			mask: []byte{0xef},
			tid:  10,
			cpu:  3,
		},
		{
			// only the fifth cpu.
			mask: []byte{0x10},
			tid:  10,
			cpu:  4,
		},
	} {
		assigned := assignCPU(test.mask, test.tid)
		if test.cpu != assigned {
			t.Errorf("assignCPU(%v, %v) got %v, want %v", test.mask, test.tid, assigned, test.cpu)
		}
	}
}

func TestFSCheckpointedMemoryFilesFromContext(t *testing.T) {
	if got := FSCheckpointedMemoryFilesFromContext(nil); got != nil {
		t.Errorf("FSCheckpointedMemoryFilesFromContext(nil) = %v, want nil", got)
	}
	ctx := context.Background()
	if got := FSCheckpointedMemoryFilesFromContext(ctx); got != nil {
		t.Errorf("FSCheckpointedMemoryFilesFromContext(ctx) = %v, want nil", got)
	}
	mfs := make(map[checkpoint.ResourceID]struct{})
	ctx = WithFSRestore(ctx, mfs)
	if got := FSCheckpointedMemoryFilesFromContext(ctx); got == nil {
		t.Errorf("FSCheckpointedMemoryFilesFromContext(ctx) = nil, want non-nil")
	}
}

func TestMatchesPaths(t *testing.T) {
	pathsMap := map[checkpoint.ResourceID]struct{}{
		{ContainerName: "c1", Path: "/data"}: {},
	}
	rid := checkpoint.ResourceID{ContainerName: "c1", Path: "/data"}
	if !matchesPaths(rid, pathsMap, true /* isTmpfs */) {
		t.Errorf("matchesPaths(%+v, isTmpfs=true) = false, want true", rid)
	}
	if matchesPaths(rid, pathsMap, false /* isTmpfs */) {
		t.Errorf("matchesPaths(%+v, isTmpfs=false) = true, want false", rid)
	}
}

func TestMatchesPathsDefaultRoot(t *testing.T) {
	pathsMap := map[checkpoint.ResourceID]struct{}{
		{Path: "/"}: {},
	}
	rid := checkpoint.ResourceID{Path: "/"}
	if !matchesPaths(rid, pathsMap, true /* isTmpfs */) {
		t.Errorf("matchesPaths(%+v, isTmpfs=true) = false, want true", rid)
	}
}

// A container's lowest thread group is found among the thread groups the
// namespace holds, with the IDs it holds them by, so one reaped meanwhile is
// neither found nor taken for the lowest.
func TestLowestThreadGroupOfContainer(t *testing.T) {
	ns := &PIDNamespace{owner: &TaskSet{}, tgids: make(map[*ThreadGroup]ThreadID)}
	add := func(cid string, id ThreadID) *ThreadGroup {
		tg := &ThreadGroup{threadGroupNode: threadGroupNode{leader: &Task{containerID: cid}}}
		ns.tgids[tg] = id
		return tg
	}
	add("other", 1)
	init := add("c", 2)
	add("c", 9)
	reaped := add("c", 5)
	delete(ns.tgids, reaped)
	if got := ns.LowestThreadGroupOfContainer("c"); got != init {
		t.Errorf("LowestThreadGroupOfContainer(c) = %p, want the init %p", got, init)
	}
	if got := ns.LowestThreadGroupOfContainer("none"); got != nil {
		t.Errorf("LowestThreadGroupOfContainer(none) = %p, want nil", got)
	}
}
