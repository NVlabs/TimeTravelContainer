#!/bin/sh

# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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
# ttcd builds with cgo, which the build's options turn off, in a configuration
# of its own, whose outputs are not in bazel-bin: Bazel says where they are.
ttc=$("$bazel_command" cquery --output=files //runsc/ttc:ttc 2>/dev/null)
ttcd=$("$bazel_command" cquery --output=files //runsc/ttcd:ttcd 2>/dev/null)
mkdir -p "$destination"
/usr/bin/install -m 0755 "$repo_root/$ttc" "$destination/ttc"
/usr/bin/install -m 0755 "$repo_root/$ttcd" "$destination/ttcd"
/usr/bin/codesign --force --sign - \
	--entitlements "$repo_root/runsc/hvf.entitlements" \
	"$destination/ttcd"
/usr/bin/codesign --verify --strict --verbose "$destination/ttcd"

echo "installed $destination/ttc"
echo "installed and signed $destination/ttcd"
