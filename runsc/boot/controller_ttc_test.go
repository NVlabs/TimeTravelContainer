// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package boot

import (
	"testing"
	"time"
)

func TestDistributedFilesystemDrainIsBranchScoped(t *testing.T) {
	alpha := &distributedForkFilesystemDrain{done: make(chan struct{})}
	cm := &containerManager{
		l:                &Loader{},
		filesystemDrains: map[string]*distributedForkFilesystemDrain{"alpha": alpha},
	}
	betaArgs := &DistributedForkDependencyArgs{BranchID: "beta"}
	if err := cm.DrainDistributedForkFilesystems(betaArgs, nil); err != nil {
		t.Fatalf("DrainDistributedForkFilesystems(beta): %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for {
		var status DistributedForkFilesystemDrainStatus
		if err := cm.DistributedForkFilesystemDrainStatus(betaArgs, &status); err != nil {
			t.Fatalf("DistributedForkFilesystemDrainStatus(beta): %v", err)
		}
		if status.Done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("beta drain did not finish")
		}
		time.Sleep(time.Millisecond)
	}

	var alphaStatus DistributedForkFilesystemDrainStatus
	if err := cm.DistributedForkFilesystemDrainStatus(&DistributedForkDependencyArgs{BranchID: "alpha"}, &alphaStatus); err != nil {
		t.Fatalf("DistributedForkFilesystemDrainStatus(alpha): %v", err)
	}
	if alphaStatus.Done {
		t.Fatal("beta drain incorrectly completed alpha barrier")
	}

	cm.filesystemDrainMu.Lock()
	firstBeta := cm.filesystemDrains["beta"]
	cm.filesystemDrainMu.Unlock()
	if err := cm.DrainDistributedForkFilesystems(betaArgs, nil); err != nil {
		t.Fatalf("repeated DrainDistributedForkFilesystems(beta): %v", err)
	}
	cm.filesystemDrainMu.Lock()
	secondBeta := cm.filesystemDrains["beta"]
	cm.filesystemDrainMu.Unlock()
	if firstBeta == secondBeta {
		t.Fatal("completed branch drain was reused instead of establishing a fresh barrier")
	}
}
