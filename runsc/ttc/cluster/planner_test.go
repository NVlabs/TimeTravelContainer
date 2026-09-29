// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"reflect"
	"testing"
)

func TestPlanPressureReliefMovesSharingGroupWithoutCut(t *testing.T) {
	const mib = uint64(1 << 20)
	plan, err := PlanPressureRelief(
		NodeResource{ID: "node-a", CurrentBytes: 2400 * mib, TargetBytes: 1500 * mib},
		NodeResource{ID: "node-b", CurrentBytes: 200 * mib, TargetBytes: 3 << 30},
		[]BranchResource{
			{ID: "family@a", NodeID: "node-a", PrivateBytes: 100 * mib, Movable: true},
			{ID: "family@b", NodeID: "node-a", PrivateBytes: 100 * mib, Movable: true},
		},
		[]SharedResource{{ID: "warm-runtime", Bytes: 1024 * mib, Branches: []string{"family@a", "family@b"}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Branches, []string{"family@a", "family@b"}) || plan.EstimatedCut != 0 || plan.EstimatedFreed != 1224*mib {
		t.Fatalf("plan=%+v", plan)
	}
}

func TestPlanPressureReliefRejectsOverfullTarget(t *testing.T) {
	_, err := PlanPressureRelief(
		NodeResource{ID: "a", CurrentBytes: 200, TargetBytes: 100},
		NodeResource{ID: "b", CurrentBytes: 95, TargetBytes: 100},
		[]BranchResource{{ID: "f@b", NodeID: "a", PrivateBytes: 10, Movable: true}}, nil,
	)
	if err == nil {
		t.Fatal("overfull destination accepted a move")
	}
}
