#!/bin/bash

# Copyright 2018 The gVisor Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# The STABLE_ prefix will trigger a re-link if it changes. `git describe
# --dirty` only distinguishes clean from dirty: two binaries built from
# different uncommitted source trees would both report the same `<commit>-dirty`
# identity and TTC's exact client/daemon and daemon/daemon compatibility fence
# could accept them. Include a deterministic digest of every tracked change
# and every non-ignored untracked file instead.
if base=$(git describe --always --tags --exclude 'release-*-staging' --abbrev=12 2>/dev/null); then
  changed=false
  if ! git diff --quiet HEAD -- || ! git diff --cached --quiet HEAD --; then
    changed=true
  elif test -n "$(git ls-files --others --exclude-standard | head -n 1)"; then
    changed=true
  fi
  if "$changed"; then
    digest=$(
      {
        git diff --binary HEAD --
        while IFS= read -r -d '' path; do
          printf '\0%s\0' "$path"
          sha256sum -- "$path"
        done < <(git ls-files --others --exclude-standard -z)
      } | sha256sum | cut -c1-16
    )
    base="${base}-dirty.${digest}"
  fi
else
  base=0.0.0
fi
echo STABLE_VERSION "$base"
