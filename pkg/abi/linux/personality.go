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

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package linux

// Personality flags, used by personality(2),
// from include/uapi/linux/personality.h.
const (
	ADDR_NO_RANDOMIZE = 0x0040000
	SHORT_INODE       = 0x1000000
	WHOLE_SECONDS     = 0x2000000
	PER_LINUX         = 0x0000
	PER_BSD           = 0x0006
	PER_HPUX          = 0x0010

	// PER_CLEAR_ON_SETID contains security-sensitive personality flags that
	// Linux clears during privileged execve(2). Only supported flags are
	// included here.
	PER_CLEAR_ON_SETID = ADDR_NO_RANDOMIZE
)

// NOTE: Personality flags are copied from parent task to child task and across
// ordinary execs. Security-sensitive flags must also be included in
// PER_CLEAR_ON_SETID so that privileged execs clear them.
