#!/bin/bash

# Copyright 2026 The gVisor Authors.
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

# Runs a Go test binary that uses Hypervisor.framework. Bazel links test
# binaries without the hypervisor entitlement, so on their own they can't
# create a VM and their tests skip; run a copy signed with the entitlement,
# and make those tests fail rather than skip if a VM still can't be created.
#
# Usage: run_entitled_test.sh BINARY ENTITLEMENTS [TEST FLAGS...]

set -euo pipefail

readonly binary="$1"
readonly entitlements="$2"
shift 2

readonly signed="${TEST_TMPDIR:?}/$(basename "${binary}")"
cp "${binary}" "${signed}"
chmod u+w "${signed}"
/usr/bin/codesign --force --sign - --entitlements "${entitlements}" "${signed}"
export TTC_REQUIRE_HVF=1
exec "${signed}" "$@"
