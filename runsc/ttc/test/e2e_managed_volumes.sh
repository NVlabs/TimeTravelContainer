#!/bin/sh
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

# Manual end-to-end coverage for TTC's disk-backed, branchable volume driver.
set -eu

TTC=${TTC:-}
RUNSC=${RUNSC:-}
TTC_E2E_ROOT=${TTC_E2E_ROOT:-}
TTC_UNSAFE_NO_HOST_ISOLATION=${TTC_UNSAFE_NO_HOST_ISOLATION:-0}
TTC_ALLOW_SLOW_VOLUME_COPY=${TTC_ALLOW_SLOW_VOLUME_COPY:-0}
BUSYBOX_IMAGE=${BUSYBOX_IMAGE:-busybox:1.36}

fail() {
  echo "ttc managed-volume e2e: $*" >&2
  exit 1
}

[ -x "$TTC" ] || fail "set TTC to the ttc binary"
[ -x "$RUNSC" ] || fail "set RUNSC to the TimeTravel-enabled runsc binary"

owned_root=false
if [ -z "$TTC_E2E_ROOT" ]; then
  TTC_E2E_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/ttc-volume-e2e.XXXXXX")
  owned_root=true
else
  mkdir -p "$TTC_E2E_ROOT"
fi
DATA_ROOT="$TTC_E2E_ROOT/data"
RUNSC_ROOT="$TTC_E2E_ROOT/runsc"
DAEMON_SOCKET="$TTC_E2E_ROOT/ttcd.sock"
PREFIX="volume-$$"
MAIN="$PREFIX-main"
SECOND="$PREFIX-second"
VOLUME="$PREFIX-data"
SEEDED_VOLUME="$PREFIX-seeded"
EMPTY_VOLUME="$PREFIX-empty"

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

set -- daemon start
if [ "$TTC_UNSAFE_NO_HOST_ISOLATION" = 1 ]; then
  set -- "$@" --unsafe-no-host-isolation
fi
if [ "$TTC_ALLOW_SLOW_VOLUME_COPY" = 1 ]; then
  set -- "$@" --allow-slow-volume-copy
fi
ttc "$@"

# A new volume copies the image destination directly into its repository once;
# nocopy creates the same mount with an empty root.
ttc run --name "$PREFIX-seed-writer" -v "$SEEDED_VOLUME:/etc" "$BUSYBOX_IMAGE" \
  sh -c 'test -s /etc/passwd; echo ttc-seeded >>/etc/passwd' >/dev/null
ttc rm "$PREFIX-seed-writer" >/dev/null
ttc run --rm -v "$SEEDED_VOLUME:/etc:ro" "$BUSYBOX_IMAGE" \
  grep -q ttc-seeded /etc/passwd
ttc run --rm -v "$EMPTY_VOLUME:/etc:nocopy" "$BUSYBOX_IMAGE" \
  sh -c 'test ! -e /etc/passwd'
ttc volume rm "$SEEDED_VOLUME" "$EMPTY_VOLUME" >/dev/null

ttc volume create "$VOLUME" >/dev/null
ttc run -d --name "$MAIN" -v "$VOLUME:/data" "$BUSYBOX_IMAGE" \
  sh -c 'while :; do sleep 60; done' >/dev/null
ttc exec "$MAIN" sh -c '
  printf "base\n" >/data/value
  mkdir /data/sub
  ln /data/value /data/sub/hard
  ln -s ../value /data/sub/sym
  fallocate -l 8192 /data/allocated
  chmod 0640 /data/value
  printf base >/data/race
  sync
'

# The canonical bytes and namespace are already disk objects while running;
# no tmpfs/tar export is involved.
find "$DATA_ROOT/volumes-v2" -path '*/objects/*' -type f -size +0c | grep . >/dev/null ||
  fail "running volume did not create a non-empty disk object"
if find "$DATA_ROOT/volumes-v2" -type f -name '*.tar' | grep . >/dev/null; then
  fail "managed volume unexpectedly created a tar persistence image"
fi

ttc stop "$MAIN" >/dev/null
ttc start "$MAIN" >/dev/null
ttc exec "$MAIN" sh -c '
  test "$(cat /data/value)" = base
  test "$(cat /data/sub/hard)" = base
  test "$(readlink /data/sub/sym)" = ../value
  test "$(stat -c %s /data/allocated)" = 8192
  test "$(stat -c %a /data/value)" = 640
'

# Writes before and between nested forks must produce the expected tree.
ttc fork "$MAIN" f1 >/dev/null

# Start both branches' first writers before either writer exits. Detached exec
# creation is serialized by ttcd, but the guest writers overlap after sleep.
RACE_MAIN=$(ttc exec -d "$MAIN@main" sh -c '
  sleep 1
  i=0
  while [ "$i" -lt 10 ]; do printf "round1-main-%03d" "$i" >/data/race; i=$((i + 1)); done
')
RACE_F1=$(ttc exec -d "$MAIN@f1" sh -c '
  sleep 1
  i=0
  while [ "$i" -lt 10 ]; do printf "round1-f1-%03d" "$i" >/data/race; i=$((i + 1)); done
')
[ "$(ttc exec wait "$RACE_MAIN")" = 0 ] || fail "concurrent main writer failed"
[ "$(ttc exec wait "$RACE_F1")" = 0 ] || fail "concurrent f1 writer failed"
ttc exec "$MAIN@main" sh -c 'test "$(cat /data/race)" = round1-main-009'
ttc exec "$MAIN@f1" sh -c 'test "$(cat /data/race)" = round1-f1-009'

ttc exec "$MAIN@main" sh -c 'echo main-1 >>/data/value; echo main >/data/main-only'
ttc exec "$MAIN@f1" sh -c 'echo f1-1 >>/data/value; echo f1 >/data/f1-only; mv /data/sub /data/f1-sub'
ttc fork "$MAIN@main" f2 >/dev/null
ttc fork "$MAIN@f1" g1 >/dev/null

# Every side of both second-generation forks now writes the same inherited
# pathname concurrently. This exercises simultaneous first-writer object
# splitting in both sources and both children, plus same-name namespace CoW.
RACE_MAIN=$(ttc exec -d "$MAIN@main" sh -c '
  sleep 1
  i=0
  while [ "$i" -lt 10 ]; do printf "round2-main-%03d" "$i" >/data/race; i=$((i + 1)); done
  printf main >/data/same-name
')
RACE_F1=$(ttc exec -d "$MAIN@f1" sh -c '
  sleep 1
  i=0
  while [ "$i" -lt 10 ]; do printf "round2-f1-%03d" "$i" >/data/race; i=$((i + 1)); done
  printf f1 >/data/same-name
')
RACE_F2=$(ttc exec -d "$MAIN@f2" sh -c '
  sleep 1
  i=0
  while [ "$i" -lt 10 ]; do printf "round2-f2-%03d" "$i" >/data/race; i=$((i + 1)); done
  printf f2 >/data/same-name
')
RACE_G1=$(ttc exec -d "$MAIN@g1" sh -c '
  sleep 1
  i=0
  while [ "$i" -lt 10 ]; do printf "round2-g1-%03d" "$i" >/data/race; i=$((i + 1)); done
  printf g1 >/data/same-name
')
for session in "$RACE_MAIN" "$RACE_F1" "$RACE_F2" "$RACE_G1"; do
  [ "$(ttc exec wait "$session")" = 0 ] || fail "four-branch concurrent writer failed: $session"
done
for branch_and_value in main:main f1:f1 f2:f2 g1:g1; do
  branch=${branch_and_value%%:*}
  value=${branch_and_value#*:}
  ttc exec "$MAIN@$branch" sh -c \
    "test \"\$(cat /data/race)\" = round2-$value-009 && test \"\$(cat /data/same-name)\" = $value"
done

ttc exec "$MAIN@main" sh -c 'echo main-2 >>/data/value; echo main2 >/data/who-main'
ttc exec "$MAIN@f1" sh -c 'echo f1-2 >>/data/value; echo f12 >/data/who-f1'
ttc exec "$MAIN@f2" sh -c 'echo f2 >>/data/value; echo f2 >/data/who-f2; rm /data/main-only'
ttc exec "$MAIN@g1" sh -c 'echo g1 >>/data/value; echo g1 >/data/who-g1; mkdir /data/g1-dir'
ttc exec "$MAIN@main" sh -c 'test "$(tail -n 1 /data/value)" = main-2; test -e /data/who-main; test ! -e /data/who-f1'
ttc exec "$MAIN@f1" sh -c 'test "$(tail -n 1 /data/value)" = f1-2; test -e /data/who-f1; test ! -e /data/who-main'
ttc exec "$MAIN@f2" sh -c 'test "$(tail -n 1 /data/value)" = f2; test -e /data/who-f2; test ! -e /data/main-only'
ttc exec "$MAIN@g1" sh -c 'test "$(tail -n 1 /data/value)" = g1; test -e /data/who-g1; test -d /data/g1-dir'

# A writable file description held by a running process remains branch-local
# after fork.
ttc exec -d "$MAIN@main" sh -c '
  : >/data/openfd
  exec 3>>/data/openfd
  echo pre-fork >&3
  touch /tmp/fd-ready
  while [ ! -e /tmp/fd-go ]; do sleep 0.1; done
  cat /tmp/fd-content >&3
  touch /tmp/fd-done
  sleep 30
' >/dev/null
for unused in $(seq 1 50); do
  ttc exec "$MAIN@main" test -e /tmp/fd-ready && break
done
ttc fork "$MAIN@main" fdchild >/dev/null
ttc exec "$MAIN@main" sh -c 'echo parent-fd >/tmp/fd-content; touch /tmp/fd-go'
ttc exec "$MAIN@fdchild" sh -c 'echo child-fd >/tmp/fd-content; touch /tmp/fd-go'
for branch in main fdchild; do
  for unused in $(seq 1 50); do
    ttc exec "$MAIN@$branch" test -e /tmp/fd-done && break
  done
done
ttc exec "$MAIN@main" sh -c 'test "$(tail -n 1 /data/openfd)" = parent-fd'
ttc exec "$MAIN@fdchild" sh -c 'test "$(tail -n 1 /data/openfd)" = child-fd'

# Identical visible branch names in two sandboxes sharing a named volume must
# map to distinct repository branch keys.
ttc run -d --name "$SECOND" -v "$VOLUME:/data:ro" "$BUSYBOX_IMAGE" \
  sh -c 'while :; do sleep 60; done' >/dev/null
ttc fork "$SECOND" f1 >/dev/null

ttc exec "$MAIN@main" sh -c 'echo daemon-restart >>/data/value; sync'
ttc daemon stop >/dev/null
ttc "$@"
ttc start "$MAIN" >/dev/null
ttc exec "$MAIN" sh -c 'test "$(tail -n 1 /data/value)" = daemon-restart'

echo "ttc managed-volume e2e: PASS"
