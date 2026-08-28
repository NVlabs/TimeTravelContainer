#!/bin/sh
# Copyright 2026 The gVisor Authors.
# SPDX-License-Identifier: Apache-2.0

set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/../.." && pwd)
destination=${1:-"$repo_root/../bin"}
bazel_command=${BAZEL:-bazel}

case "$bazel_command" in
/*) ;;
*)
	if ! command -v "$bazel_command" >/dev/null 2>&1; then
		echo "install_macos.sh: cannot find Bazel executable $bazel_command" >&2
		exit 1
	fi
	;;
esac

"$bazel_command" build //runsc/ttc:ttc //runsc/ttcd:ttcd
mkdir -p "$destination"
/usr/bin/install -m 0755 "$repo_root/bazel-bin/runsc/ttc/ttc_/ttc" "$destination/ttc"
/usr/bin/install -m 0755 "$repo_root/bazel-bin/runsc/ttcd/ttcd_/ttcd" "$destination/ttcd"
/usr/bin/codesign --force --sign - \
	--entitlements "$repo_root/runsc/hvf.entitlements" \
	"$destination/ttcd"
/usr/bin/codesign --verify --strict --verbose "$destination/ttcd"

echo "installed $destination/ttc"
echo "installed and signed $destination/ttcd"
