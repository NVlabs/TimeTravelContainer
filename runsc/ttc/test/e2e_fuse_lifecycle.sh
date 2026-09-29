#!/bin/sh

# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -eu

TTC=${TTC:-ttc}
TTCD=${TTCD:-ttcd}
TTC_UNSAFE_NO_HOST_ISOLATION=${TTC_UNSAFE_NO_HOST_ISOLATION:-0}
IMAGE=${TTC_FUSE_TEST_IMAGE:-ubuntu:24.04}
NAME=${TTC_FUSE_TEST_NAME:-ttc-fuse-lifecycle-$$}
# macOS gives TMPDIR a trailing slash; a doubled one would differ from the
# cleaned paths TTC reports.
tmp=${TMPDIR:-/tmp}
ROOT=${TTC_FUSE_TEST_ROOT:-$(mktemp -d "${tmp%/}/ttc-fuse.XXXXXX")}
OWNED_ROOT=1
if test -n "${TTC_FUSE_TEST_ROOT:-}"; then
	OWNED_ROOT=0
fi
BIN_DIR=$ROOT/bin

test -x "$TTC" || { echo "TTC FUSE lifecycle: TTC is not executable: $TTC" >&2; exit 1; }
test -x "$TTCD" || { echo "TTC FUSE lifecycle: TTCD is not executable: $TTCD" >&2; exit 1; }
mkdir -p "$BIN_DIR"
install -m 0755 "$(readlink -f "$TTC")" "$BIN_DIR/ttc"
install -m 0755 "$(readlink -f "$TTCD")" "$BIN_DIR/ttcd"
if [ "$(uname -s)" = Darwin ]; then
  # A copy loses its ad-hoc signature; Hypervisor.framework requires the
  # entitlement on the executable at its final path.
  /usr/bin/codesign --force --sign - \
    --entitlements "$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)/hvf.entitlements" \
    "$BIN_DIR/ttcd" >/dev/null
fi
TTC=$BIN_DIR/ttc
# The client finds the daemon through TTC_ROOT, which a node's TTC_LOCAL_ROOT
# would override.
unset TTC_LOCAL_ROOT
export TTC_ROOT=$ROOT/data

ttc() {
	"$TTC" "$@"
}

cleanup() {
	ttc rm -f "$NAME" >/dev/null 2>&1 || true
	ttc daemon stop --stop-all >/dev/null 2>&1 || true
	if test "$OWNED_ROOT" -eq 1; then
		rm -rf "$ROOT"
	fi
}
trap cleanup EXIT HUP INT TERM

fail() {
	echo "TTC FUSE lifecycle: $*" >&2
	exit 1
}

set -- daemon start
if test "$TTC_UNSAFE_NO_HOST_ISOLATION" = 1; then
	set -- "$@" --unsafe-no-host-isolation
fi
ttc "$@"
ttc run -d --cap-add SYS_ADMIN --network=ttc --nat --name "$NAME" "$IMAGE" sleep infinity >/dev/null
ttc exec "$NAME@main" sh -lc \
	'apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq fuse-overlayfs'
ttc exec "$NAME@main" sh -lc '
	mkdir -p /tmp/fuse/lower /tmp/fuse/upper /tmp/fuse/work /tmp/fuse/merged
	echo base >/tmp/fuse/lower/base
	fuse-overlayfs -f -o lowerdir=/tmp/fuse/lower,upperdir=/tmp/fuse/upper,workdir=/tmp/fuse/work /tmp/fuse/merged >/tmp/fuse/server.log 2>&1 &
	for delay in 1 1 1 1 1; do
		test -f /tmp/fuse/merged/base && break
		sleep "$delay"
	done
	test "$(cat /tmp/fuse/merged/base)" = base
	echo parent >/tmp/fuse/merged/branch
'

ttc fork "$NAME@main" child >/dev/null
child_before=$(ttc exec "$NAME@child" cat /tmp/fuse/merged/branch)
test "$child_before" = parent || fail "child initially read $child_before"
ttc exec "$NAME@child" sh -lc 'echo child >/tmp/fuse/merged/branch'
parent_after=$(ttc exec "$NAME@main" cat /tmp/fuse/merged/branch)
test "$parent_after" = parent || fail "child FUSE write leaked into parent: $parent_after"

ttc dehydrate "$NAME@child" >/dev/null
ttc rehydrate "$NAME@child" >/dev/null
child_after=$(ttc exec "$NAME@child" cat /tmp/fuse/merged/branch)
test "$child_after" = child || fail "rehydrated child read $child_after"
ttc exec "$NAME@child" sh -lc 'echo rehydrated >/tmp/fuse/merged/branch'
parent_final=$(ttc exec "$NAME@main" cat /tmp/fuse/merged/branch)
test "$parent_final" = parent || fail "rehydrated FUSE write leaked into parent: $parent_final"

echo "TTC in-sandbox FUSE fork/dehydrate/rehydrate lifecycle passed"
