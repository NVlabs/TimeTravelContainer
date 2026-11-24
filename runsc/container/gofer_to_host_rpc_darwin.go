// Copyright 2024 The gVisor Authors.
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

//go:build darwin

package container

import (
	"fmt"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/urpc"
)

// OpenMountArgs represents a host mount request from a gofer.
type OpenMountArgs struct {
	Mount *specs.Mount
	Flags uint32
}

// OpenMountResult contains a donated host descriptor.
type OpenMountResult struct{ urpc.FilePayload }

// goferToHostRPC retains the RPC ABI, but trusted-mode Darwin gofers always
// open their configured source paths directly and must never call OpenMount.
type goferToHostRPC struct{ goferPID int }

func (*goferToHostRPC) OpenMount(*OpenMountArgs, *OpenMountResult) error {
	return fmt.Errorf("host mount RPC is unavailable on native macOS: %w", unix.EOPNOTSUPP)
}

// goferWillNeedHostRPC returns false: a Darwin gofer opens its mounts itself.
func goferWillNeedHostRPC(*specs.Spec) (bool, error) {
	return false, nil
}
