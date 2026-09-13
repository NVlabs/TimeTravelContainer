#!/bin/sh
# Copyright 2026 The gVisor Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     https://www.apache.org/licenses/LICENSE-2.0

# Commit/save/load coverage for a live snapshot-graph rootfs. In particular,
# this catches manager-side snapshots that race ahead of the Sentry's bounded
# metadata batch and silently omit recent creates or writes.
set -eu

TTC=${TTC:-}
TTCD=${TTCD:-}
TTC_E2E_ROOT=${TTC_E2E_ROOT:-}
TTC_UNSAFE_NO_HOST_ISOLATION=${TTC_UNSAFE_NO_HOST_ISOLATION:-0}
IMAGE=${TTC_IMAGE_ROUNDTRIP_IMAGE:-busybox:1.36}

fail() {
	echo "ttc image round-trip e2e: $*" >&2
	exit 1
}

[ -x "$TTC" ] || fail "set TTC to the ttc binary"
[ -x "$TTCD" ] || fail "set TTCD to the matching ttcd binary"

owned_root=false
if [ -z "$TTC_E2E_ROOT" ]; then
	TTC_E2E_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/ttc-image-roundtrip.XXXXXX")
	owned_root=true
else
	mkdir -p "$TTC_E2E_ROOT"
fi
BIN_DIR="$TTC_E2E_ROOT/bin"
mkdir -p "$BIN_DIR"
install -m 0755 "$(readlink -f "$TTC")" "$BIN_DIR/ttc"
install -m 0755 "$(readlink -f "$TTCD")" "$BIN_DIR/ttcd"
TTC="$BIN_DIR/ttc"
export TTC_ROOT="$TTC_E2E_ROOT/data"
SOCKET="$TTC_E2E_ROOT/ttcd.sock"
NAME="image-roundtrip-$$"
LIVE_IMAGE="local/ttc-image-live-$$:test"
PAUSED_IMAGE="local/ttc-image-paused-$$:test"
EXITED_IMAGE="local/ttc-image-exited-$$:test"
ARCHIVE="$TTC_E2E_ROOT/image.tar"

ttc() {
	"$TTC" --daemon-socket "$SOCKET" "$@"
}

cleanup() {
	ttc rm -f "$NAME" >/dev/null 2>&1 || true
	for image in "$LIVE_IMAGE" "$PAUSED_IMAGE" "$EXITED_IMAGE"; do
		ttc image rm -f "$image" >/dev/null 2>&1 || true
	done
	ttc daemon stop >/dev/null 2>&1 || true
	if [ "$owned_root" = true ]; then
		rm -rf "$TTC_E2E_ROOT"
	fi
}
trap cleanup EXIT HUP INT TERM

set -- daemon start
if [ "$TTC_UNSAFE_NO_HOST_ISOLATION" = 1 ]; then
	set -- "$@" --unsafe-no-host-isolation
fi
ttc "$@"

ttc run -d --name "$NAME" "$IMAGE" sh -c \
	'echo original >/keep; echo delete >/gone; exec sleep 3600' >/dev/null
ttc exec "$NAME@main" sh -c 'rm /gone; echo changed >/keep; echo new >/new'

ttc commit --change 'LABEL timetravel.usage=qualification' \
	"$NAME@main" "$LIVE_IMAGE" >/dev/null
ttc image inspect "$LIVE_IMAGE" | grep -q 'timetravel.usage' ||
	fail "OCI label did not survive commit"

ttc pause "$NAME@main" >/dev/null
ttc commit "$NAME@main" "$PAUSED_IMAGE" >/dev/null
ttc stop "$NAME@main" >/dev/null
ttc commit "$NAME@main" "$EXITED_IMAGE" >/dev/null

ttc save -o "$ARCHIVE" "$EXITED_IMAGE"
[ -s "$ARCHIVE" ] || fail "save produced an empty archive"
ttc image rm -f "$EXITED_IMAGE" >/dev/null
ttc load -i "$ARCHIVE" >/dev/null

output=$(ttc run --rm "$EXITED_IMAGE" sh -c \
	'test ! -e /gone && cat /keep /new')
[ "$output" = "$(printf 'changed\nnew')" ] ||
	fail "committed files did not survive save/load: $output"

echo "TTC live/paused/exited commit and save/load round-trip passed"
