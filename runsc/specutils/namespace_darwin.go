// Copyright 2018 Google Inc.
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

package specutils

import (
	"os/exec"

	"github.com/moby/sys/capability"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

// GetNS returns the requested OCI namespace entry. OCI specs retain their
// Linux shape on every runsc host even when host namespace operations are not
// available.
func GetNS(nst specs.LinuxNamespaceType, s *specs.Spec) (specs.LinuxNamespace, bool) {
	if s.Linux == nil {
		return specs.LinuxNamespace{}, false
	}
	for _, ns := range s.Linux.Namespaces {
		if ns.Type == nst {
			return ns, true
		}
	}
	return specs.LinuxNamespace{}, false
}

func ApplyNS(specs.LinuxNamespace) (func() error, error) { return nil, unix.EOPNOTSUPP }

// StartInNS intentionally ignores Linux namespace requests on Darwin. This is
// reachable only after runsc's mandatory unsafe trusted-mode validation.
func StartInNS(cmd *exec.Cmd, nss []specs.LinuxNamespace) error { return cmd.Start() }

func SetUIDGIDMappings(cmd *exec.Cmd, s *specs.Spec) {}

func HasCapabilities(cs ...capability.Cap) bool { return false }

// SetgroupsAllowed reports that a Darwin gofer does not take an application's
// supplementary groups: Darwin has no per-thread groups.
func SetgroupsAllowed() bool { return false }

// Darwin has no Linux user namespace in which to acquire capabilities.
func MaybeRunAsRoot() error { return nil }

const NullNetNSFilename = "null-netns"

func UnmountNullNetNS(sharedRootDir string) {}

func KnownNamespaces() []string { return nil }
