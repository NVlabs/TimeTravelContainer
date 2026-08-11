#!/bin/sh
# Copyright 2026 The gVisor Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Manual end-to-end coverage for branch-aware host/container copying.
set -eu

TTC=${TTC:-}
RUNSC=${RUNSC:-}
TTC_E2E_ROOT=${TTC_E2E_ROOT:-}
TTC_UNSAFE_NO_HOST_ISOLATION=${TTC_UNSAFE_NO_HOST_ISOLATION:-0}
TTC_ALLOW_SLOW_VOLUME_COPY=${TTC_ALLOW_SLOW_VOLUME_COPY:-0}
BUSYBOX_IMAGE=${BUSYBOX_IMAGE:-busybox:1.36}

fail() {
  echo "ttc cp e2e: $*" >&2
  exit 1
}

[ -x "$TTC" ] || fail "set TTC to the ttc binary"
[ -x "$RUNSC" ] || fail "set RUNSC to the TimeTravel-enabled runsc binary"

owned_root=false
if [ -z "$TTC_E2E_ROOT" ]; then
  TTC_E2E_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/ttc-cp-e2e.XXXXXX")
  owned_root=true
else
  mkdir -p "$TTC_E2E_ROOT"
fi
DATA_ROOT="$TTC_E2E_ROOT/data"
RUNSC_ROOT="$TTC_E2E_ROOT/runsc"
DAEMON_SOCKET="$TTC_E2E_ROOT/ttcd.sock"
CONTAINER="cp-$$"
VOLUME="cp-volume-$$"
HOST_SOURCE="$TTC_E2E_ROOT/host-source"
HOST_EXPORT="$TTC_E2E_ROOT/host-export"
HOST_CONTENTS="$TTC_E2E_ROOT/host-contents"
ARCHIVE="$TTC_E2E_ROOT/export.tar"

ttc() {
  "$TTC" \
    --data-root "$DATA_ROOT" \
    --daemon-socket "$DAEMON_SOCKET" \
    --root "$RUNSC_ROOT" \
    --runsc "$RUNSC" \
    "$@"
}

cleanup() {
  ttc daemon stop >/dev/null 2>&1 || true
  if [ "$owned_root" = true ]; then
    rm -rf "$TTC_E2E_ROOT"
  fi
}
trap cleanup EXIT HUP INT TERM

rm -rf "$HOST_SOURCE" "$HOST_EXPORT" "$HOST_CONTENTS" "$ARCHIVE" \
  "$TTC_E2E_ROOT/parent" "$TTC_E2E_ROOT/child" \
  "$TTC_E2E_ROOT/paused-copy" "$TTC_E2E_ROOT/paused-input"
mkdir -p "$HOST_SOURCE/nested" "$HOST_EXPORT" "$HOST_CONTENTS"
printf 'binary\000payload\n' >"$HOST_SOURCE/nested/value"
ln "$HOST_SOURCE/nested/value" "$HOST_SOURCE/hard"
ln -s nested/value "$HOST_SOURCE/symbolic"
chmod 0640 "$HOST_SOURCE/nested/value"

set -- daemon start
if [ "$TTC_UNSAFE_NO_HOST_ISOLATION" = 1 ]; then
  set -- "$@" --unsafe-no-host-isolation
fi
if [ "$TTC_ALLOW_SLOW_VOLUME_COPY" = 1 ]; then
  set -- "$@" --allow-slow-volume-copy
fi
ttc "$@"

ttc volume create "$VOLUME" >/dev/null
ttc run -d --name "$CONTAINER" -v "$VOLUME:/data" "$BUSYBOX_IMAGE" \
  sh -c 'while :; do sleep 60; done' >/dev/null

# A directory copied to a new destination uses that path exactly. The helper is
# donated as an executable FD, so this operation does not depend on guest tar.
ttc cp "$HOST_SOURCE" "$CONTAINER@main:/tmp/import"
ttc exec "$CONTAINER@main" sh -c '
  test "$(cat /tmp/import/nested/value)" = binary
  test "$(readlink /tmp/import/symbolic)" = nested/value
  test /tmp/import/nested/value -ef /tmp/import/hard
  test "$(stat -c %a /tmp/import/nested/value)" = 640
'

ttc fork "$CONTAINER@main" child >/dev/null
printf 'parent\n' >"$TTC_E2E_ROOT/parent"
printf 'child\n' >"$TTC_E2E_ROOT/child"
ttc cp "$TTC_E2E_ROOT/parent" "$CONTAINER@main:/data/branch"
ttc cp "$TTC_E2E_ROOT/child" "$CONTAINER@child:/data/branch"
ttc exec "$CONTAINER@main" sh -c 'test "$(cat /data/branch)" = parent'
ttc exec "$CONTAINER@child" sh -c 'test "$(cat /data/branch)" = child'

# A trusted management helper copies in both directions while all existing
# guest tasks remain paused.
ttc pause "$CONTAINER@child" >/dev/null
ttc cp "$CONTAINER@child:/data/branch" "$TTC_E2E_ROOT/paused-copy"
[ "$(cat "$TTC_E2E_ROOT/paused-copy")" = child ] ||
  fail "copy from paused branch returned the wrong data"
printf 'paused-import\n' >"$TTC_E2E_ROOT/paused-input"
ttc cp "$TTC_E2E_ROOT/paused-input" "$CONTAINER@child:/data/paused-import"
if ttc exec "$CONTAINER@child" true >/dev/null 2>&1; then
  fail "paused-branch copy resumed ordinary guest execution"
fi
ttc resume "$CONTAINER@child" >/dev/null
ttc exec "$CONTAINER@child" sh -c 'test "$(cat /data/paused-import)" = paused-import'

# Container-to-host copying preserves nested regular files, hard links, and
# symlinks. A trailing '/.' copies only the source directory's contents.
ttc cp "$CONTAINER@child:/tmp/import" "$HOST_EXPORT"
cmp "$HOST_SOURCE/nested/value" "$HOST_EXPORT/import/nested/value"
[ "$(readlink "$HOST_EXPORT/import/symbolic")" = nested/value ] ||
  fail "container-to-host symlink target changed"
[ "$HOST_EXPORT/import/nested/value" -ef "$HOST_EXPORT/import/hard" ] ||
  fail "container-to-host hard link was materialized as a distinct file"

ttc cp "$CONTAINER@child:/tmp/import/." "$HOST_CONTENTS"
[ -f "$HOST_CONTENTS/nested/value" ] ||
  fail "trailing '/.' did not copy directory contents"
[ ! -e "$HOST_CONTENTS/import" ] ||
  fail "trailing '/.' unexpectedly nested the source directory"

# '-' is an unmodified tar stream in either direction; daemon framing must not
# corrupt NUL bytes or archive padding.
ttc cp "$CONTAINER@child:/tmp/import" - >"$ARCHIVE"
ttc exec "$CONTAINER@main" mkdir /tmp/from-archive
ttc cp - "$CONTAINER@main:/tmp/from-archive" <"$ARCHIVE"
ttc exec "$CONTAINER@main" sh -c '
  test "$(cat /data/branch)" = parent
  test "$(readlink /tmp/from-archive/import/symbolic)" = nested/value
  cmp /tmp/from-archive/import/nested/value /tmp/import/nested/value
'

echo "ttc cp e2e: PASS"
