#!/bin/bash

# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Runs a Go test binary that uses Hypervisor.framework. Bazel links test
# binaries without the hypervisor entitlement, so on their own they can't
# create a VM and their tests skip; run a copy signed with the entitlement,
# and make those tests fail rather than skip if a VM still can't be created.
# On a host without hypervisor support, such as a virtual machine without
# nested virtualization, no binary can create a VM, and the tests skip. Such
# a host's kern.hv_support is 0, or the host has no such variable.
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
if [[ "$(/usr/sbin/sysctl -n kern.hv_support 2>/dev/null)" == 1 ]]; then
  export TTC_REQUIRE_HVF=1
else
  echo "kern.hv_support is not 1: this host has no hypervisor support, and the tests that create a VM skip" >&2
fi
exec "${signed}" "$@"
