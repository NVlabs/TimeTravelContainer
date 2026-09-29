#!/bin/bash

# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

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
