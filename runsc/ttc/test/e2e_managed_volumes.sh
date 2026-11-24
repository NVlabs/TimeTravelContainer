#!/bin/sh

# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Manual end-to-end coverage for TTC's disk-backed, branchable rootfs upper and
# managed-volume driver.
set -eu

TTC=${TTC:-}
TTCD=${TTCD:-}
TTC_E2E_ROOT=${TTC_E2E_ROOT:-}
TTC_UNSAFE_NO_HOST_ISOLATION=${TTC_UNSAFE_NO_HOST_ISOLATION:-0}
BUSYBOX_IMAGE=${BUSYBOX_IMAGE:-busybox:1.36}

fail() {
  echo "ttc managed-volume e2e: $*" >&2
  exit 1
}

[ -x "$TTC" ] || fail "set TTC to the ttc binary"
[ -x "$TTCD" ] || fail "set TTCD to the matching ttcd binary"

owned_root=false
if [ -z "$TTC_E2E_ROOT" ]; then
  # macOS gives TMPDIR a trailing slash; a doubled one would differ from
  # the cleaned paths TTC reports.
  tmp=${TMPDIR:-/tmp}
  TTC_E2E_ROOT=$(mktemp -d "${tmp%/}/ttc-volume.XXXXXX")
  owned_root=true
else
  mkdir -p "$TTC_E2E_ROOT"
fi
DATA_ROOT="$TTC_E2E_ROOT/data"
BIN_DIR="$TTC_E2E_ROOT/bin"
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
TTC="$BIN_DIR/ttc"
# The client finds the daemon through TTC_ROOT, which a node's TTC_LOCAL_ROOT
# would override.
unset TTC_LOCAL_ROOT
export TTC_ROOT="$DATA_ROOT"
PREFIX="volume-$$"
MAIN="$PREFIX-main"
SECOND="$PREFIX-second"
VOLUME="$PREFIX-data"
SEEDED_VOLUME="$PREFIX-seeded"
EMPTY_VOLUME="$PREFIX-empty"
ROOTFS_CONTAINER="$PREFIX-rootfs"
CREATED_CONTAINER="$PREFIX-created"
HOST_COW_CONTAINER="$PREFIX-host-cow"
HOST_COPY_CONTAINER="$PREFIX-host-copy"
HOST_COW_RESTORED="$PREFIX-host-cow-restored"
HOST_COPY_RESTORED="$PREFIX-host-copy-restored"

ttc() {
  "$TTC" "$@"
}

cleanup() {
  ttc daemon stop --stop-all >/dev/null 2>&1 || true
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

# The ordinary writable root uses the same direct-to-disk branch machinery.
# Stopping never exports a tar, and cp reconstructs exited branches from
# their persistent layers without starting the image entrypoint.
HOST_IMPORT="$TTC_E2E_ROOT/rootfs-import"
CHILD_EXPORT="$TTC_E2E_ROOT/rootfs-child-export"
CREATED_EXPORT="$TTC_E2E_ROOT/rootfs-created-export"
printf 'offline-import\n' >"$HOST_IMPORT"
ttc run -d --name "$ROOTFS_CONTAINER" "$BUSYBOX_IMAGE" \
  sh -c 'echo base >/rootfs-base; while :; do sleep 60; done' >/dev/null
ttc fork "$ROOTFS_CONTAINER@main" child >/dev/null
ttc exec "$ROOTFS_CONTAINER@child" sh -c 'echo child >/rootfs-child'
ttc exec "$ROOTFS_CONTAINER@main" test ! -e /rootfs-child
# An existing VMA does not receive another AddMapping call when it refaults
# after a fork. Execute an upper-layer file after that boundary and require the
# next dehydration to invalidate its host-backed PMAs and retain the process.
ttc exec "$ROOTFS_CONTAINER@main" mkdir -p /tmp/upper
ttc exec "$ROOTFS_CONTAINER@main" cp /bin/busybox /tmp/upper/busybox
ttc fork "$ROOTFS_CONTAINER@main" mapped-child >/dev/null
ttc exec -d --session upper-mmap "$ROOTFS_CONTAINER@main" \
  /tmp/upper/busybox sh -c 'while :; do sleep 60; done'
ttc exec ls "$ROOTFS_CONTAINER@main" | grep -E \
  '^upper-mmap[[:space:]]+[0-9]+[[:space:]]+running' >/dev/null ||
  fail "upper-layer executable session did not remain running"
ttc dehydrate "$ROOTFS_CONTAINER@main" >/dev/null
ttc rehydrate "$ROOTFS_CONTAINER@main" >/dev/null
ttc exec ls "$ROOTFS_CONTAINER@main" | grep -E \
  '^upper-mmap[[:space:]]+[0-9]+[[:space:]]+running' >/dev/null ||
  fail "upper-layer executable session did not survive dehydration"
ROOTFS_ID=$(ttc inspect -f '{{.Id}}' "$ROOTFS_CONTAINER")
find "$DATA_ROOT/containers/$ROOTFS_ID/rootfs-upper/data/files" -type f -size +0c | grep . >/dev/null ||
  fail "running rootfs did not create a non-empty disk object"
if find "$DATA_ROOT/containers/$ROOTFS_ID" -type f -name '*.tar' | grep . >/dev/null; then
  fail "writable rootfs unexpectedly created a tar persistence image"
fi
ttc stop "$ROOTFS_CONTAINER@child" >/dev/null
ttc stop "$ROOTFS_CONTAINER@mapped-child" >/dev/null
ttc stop "$ROOTFS_CONTAINER@main" >/dev/null
ttc cp "$ROOTFS_CONTAINER@child:/rootfs-child" "$CHILD_EXPORT"
[ "$(cat "$CHILD_EXPORT")" = child ] ||
  fail "stopped child cp selected the wrong persistent rootfs branch"
ttc cp "$HOST_IMPORT" "$ROOTFS_CONTAINER@main:/rootfs-offline-import"
ttc start "$ROOTFS_CONTAINER@main" >/dev/null
ttc exec "$ROOTFS_CONTAINER@main" sh -c '
  test "$(cat /rootfs-base)" = base
  test "$(cat /rootfs-offline-import)" = offline-import
  test ! -e /rootfs-child
'
ttc stop "$ROOTFS_CONTAINER@main" >/dev/null
ttc rm --force "$ROOTFS_CONTAINER" >/dev/null

# A created-but-never-started container also has a complete persistent VFS
# view for offline cp.
ttc create --name "$CREATED_CONTAINER" "$BUSYBOX_IMAGE" sleep 60 >/dev/null
ttc cp "$HOST_IMPORT" "$CREATED_CONTAINER@main:/created-import"
ttc cp "$CREATED_CONTAINER@main:/created-import" "$CREATED_EXPORT"
[ "$(cat "$CREATED_EXPORT")" = offline-import ] ||
  fail "created-container offline cp did not round-trip"
ttc start "$CREATED_CONTAINER@main" >/dev/null
ttc exec "$CREATED_CONTAINER@main" test -f /created-import
ttc stop "$CREATED_CONTAINER@main" >/dev/null
ttc rm "$CREATED_CONTAINER" >/dev/null

# A lazy_copy host path is a live read-only lower with a private persistent
# ttcvol upper. Guest writes never reach the host, survive restart, and split
# with the rest of the family during live fork.
HOST_COW_SOURCE="$TTC_E2E_ROOT/host-cow-source"
mkdir -p "$HOST_COW_SOURCE"
printf 'host-lower\n' >"$HOST_COW_SOURCE/value"
ttc run -d --name "$HOST_COW_CONTAINER" -v "$HOST_COW_SOURCE:/data:lazy_copy" "$BUSYBOX_IMAGE" \
  sh -c 'while :; do sleep 60; done' >/dev/null
[ "$(ttc exec "$HOST_COW_CONTAINER@main" cat /data/value)" = host-lower ] ||
  fail "writable host mount did not expose its lower"
ttc exec "$HOST_COW_CONTAINER@main" sh -c 'echo parent >/data/value; echo parent-created >/data/created'
[ "$(cat "$HOST_COW_SOURCE/value")" = host-lower ] ||
  fail "writable host mount modified its host lower"
test ! -e "$HOST_COW_SOURCE/created" ||
  fail "writable host mount created a host file"
HOST_COW_ID=$(ttc inspect -f '{{.Id}}' "$HOST_COW_CONTAINER")
find "$DATA_ROOT/containers/$HOST_COW_ID/host-mounts" -path '*/data/files/*' -type f -size +0c | grep . >/dev/null ||
  fail "writable host mount did not write directly to a private repository"
ttc fork "$HOST_COW_CONTAINER@main" child >/dev/null
ttc exec "$HOST_COW_CONTAINER@child" sh -c 'test "$(cat /data/value)" = parent; echo child >/data/value; echo child >/data/child'
ttc exec "$HOST_COW_CONTAINER@main" sh -c 'test "$(cat /data/value)" = parent; test ! -e /data/child'
printf 'host-added\n' >"$HOST_COW_SOURCE/added"
for branch in main child; do
  ttc exec "$HOST_COW_CONTAINER@$branch" sh -c 'test "$(cat /data/added)" = host-added'
done
HOST_COW_CHECKPOINT="$TTC_E2E_ROOT/host-cow-checkpoint.ttc"
ttc checkpoint "$HOST_COW_CONTAINER@main" "$HOST_COW_CHECKPOINT" >/dev/null
printf 'host-after-checkpoint\n' >"$HOST_COW_SOURCE/added"
ttc restore --name "$HOST_COW_RESTORED" "$HOST_COW_CHECKPOINT" >/dev/null
ttc exec "$HOST_COW_RESTORED@main" sh -c '
  test "$(cat /data/value)" = parent
  test "$(cat /data/added)" = host-added
  echo restored >/data/added
'
[ "$(cat "$HOST_COW_SOURCE/added")" = host-after-checkpoint ] ||
  fail "restored writable host mount wrote through to the original host"
ttc stop "$HOST_COW_RESTORED@main" >/dev/null
ttc rm "$HOST_COW_RESTORED" >/dev/null
ttc stop "$HOST_COW_CONTAINER@child" >/dev/null
ttc stop "$HOST_COW_CONTAINER@main" >/dev/null
HOST_COW_EXPORT="$TTC_E2E_ROOT/host-cow-export"
HOST_COW_IMPORT="$TTC_E2E_ROOT/host-cow-import"
printf 'offline-host-mount\n' >"$HOST_COW_IMPORT"
ttc cp "$HOST_COW_CONTAINER@main:/data/value" "$HOST_COW_EXPORT"
[ "$(cat "$HOST_COW_EXPORT")" = parent ] ||
  fail "stopped-container cp did not read the writable host mount upper"
ttc cp "$HOST_COW_IMPORT" "$HOST_COW_CONTAINER@main:/data/offline"
ttc start "$HOST_COW_CONTAINER@main" >/dev/null
ttc exec "$HOST_COW_CONTAINER@main" sh -c '
  test "$(cat /data/value)" = parent
  test "$(cat /data/created)" = parent-created
  test "$(cat /data/offline)" = offline-host-mount
'
ttc stop "$HOST_COW_CONTAINER@main" >/dev/null
ttc rm "$HOST_COW_CONTAINER" >/dev/null

# :copy explicitly imports the complete source and then severs the runtime
# dependency on it. Directory and single-file roots are both supported.
HOST_COPY_SOURCE="$TTC_E2E_ROOT/host-copy-source"
HOST_COPY_FILE="$TTC_E2E_ROOT/host-copy-file"
mkdir -p "$HOST_COPY_SOURCE/sub"
printf 'copied-directory\n' >"$HOST_COPY_SOURCE/sub/value"
printf 'copied-file\n' >"$HOST_COPY_FILE"
ttc run -d --name "$HOST_COPY_CONTAINER" \
  -v "$HOST_COPY_SOURCE:/copied:copy" \
  -v "$HOST_COPY_FILE:/single:copy" \
  "$BUSYBOX_IMAGE" sh -c 'while :; do sleep 60; done' >/dev/null
printf 'host-mutated\n' >"$HOST_COPY_SOURCE/sub/value"
printf 'host-mutated\n' >"$HOST_COPY_FILE"
ttc exec "$HOST_COPY_CONTAINER@main" sh -c '
  test "$(cat /copied/sub/value)" = copied-directory
  test "$(cat /single)" = copied-file
  echo container >/copied/sub/value
  echo container-file >/single
'
[ "$(cat "$HOST_COPY_SOURCE/sub/value")" = host-mutated ] ||
  fail ":copy directory wrote through to the host"
[ "$(cat "$HOST_COPY_FILE")" = host-mutated ] ||
  fail ":copy file wrote through to the host"
HOST_COPY_CHECKPOINT="$TTC_E2E_ROOT/host-copy-checkpoint.ttc"
ttc checkpoint "$HOST_COPY_CONTAINER@main" "$HOST_COPY_CHECKPOINT" >/dev/null
rm -rf "$HOST_COPY_SOURCE"
rm -f "$HOST_COPY_FILE"
ttc restore --name "$HOST_COPY_RESTORED" "$HOST_COPY_CHECKPOINT" >/dev/null
ttc exec "$HOST_COPY_RESTORED@main" sh -c '
  test "$(cat /copied/sub/value)" = container
  test "$(cat /single)" = container-file
'
ttc stop "$HOST_COPY_RESTORED@main" >/dev/null
ttc rm "$HOST_COPY_RESTORED" >/dev/null
ttc fork "$HOST_COPY_CONTAINER@main" child >/dev/null
ttc exec "$HOST_COPY_CONTAINER@child" sh -c 'echo child >/copied/sub/value; echo child-file >/single'
ttc exec "$HOST_COPY_CONTAINER@main" sh -c '
  test "$(cat /copied/sub/value)" = container
  test "$(cat /single)" = container-file
'
[ "$(ttc inspect -f '{{(index .Mounts 0).Mode}}|{{(index .Mounts 1).Mode}}|{{len .TTC.ExternalViews}}' "$HOST_COPY_CONTAINER")" = \
  "rw,copy|rw,copy|0" ] || fail ":copy inspection did not report detached mounts"
ttc stop "$HOST_COPY_CONTAINER@child" >/dev/null
ttc stop "$HOST_COPY_CONTAINER@main" >/dev/null
HOST_COPY_EXPORT="$TTC_E2E_ROOT/host-copy-export"
ttc cp "$HOST_COPY_CONTAINER@main:/single" "$HOST_COPY_EXPORT"
[ "$(cat "$HOST_COPY_EXPORT")" = container-file ] ||
  fail "stopped-container cp did not read a copied single-file mount"
ttc rm "$HOST_COPY_CONTAINER" >/dev/null

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
ttc exec "$MAIN@main" sh -c '
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
find "$DATA_ROOT/filesystems/volumes" -path '*/data/files/*' -type f -size +0c | grep . >/dev/null ||
  fail "running volume did not create a non-empty disk object"
if find "$DATA_ROOT/filesystems/volumes" -type f -name '*.tar' | grep . >/dev/null; then
  fail "managed volume unexpectedly created a tar persistence image"
fi

ttc stop "$MAIN@main" >/dev/null
ttc start "$MAIN@main" >/dev/null
ttc exec "$MAIN@main" sh -c '
  test "$(cat /data/value)" = base
  test "$(cat /data/sub/hard)" = base
  test "$(readlink /data/sub/sym)" = ../value
  test "$(stat -c %s /data/allocated)" = 8192
  test "$(stat -c %a /data/value)" = 640
'

# Writes before and between nested forks must produce the expected tree.
ttc fork "$MAIN@main" f1 >/dev/null

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
ttc fork "$SECOND@main" f1 >/dev/null
ttc exec "$SECOND@main" sh -c 'test "$(tail -n 1 /data/value)" = main-2'
ttc exec "$SECOND@f1" sh -c 'test "$(tail -n 1 /data/value)" = main-2'

ttc exec "$MAIN@main" sh -c 'echo daemon-restart >>/data/value; sync'
ttc exec "$MAIN@main" sh -c 'test "$(tail -n 1 /data/value)" = daemon-restart'
ttc exec "$SECOND@main" sh -c 'test "$(tail -n 1 /data/value)" = main-2'
ttc exec "$SECOND@f1" sh -c 'test "$(tail -n 1 /data/value)" = main-2'
ttc daemon stop --stop-all >/dev/null
ttc "$@"
ttc start "$MAIN@main" >/dev/null
ttc exec "$MAIN@main" sh -c 'test "$(tail -n 1 /data/value)" = daemon-restart'
ttc start "$SECOND@main" >/dev/null
ttc exec "$SECOND@main" sh -c 'test "$(tail -n 1 /data/value)" = main-2'

echo "ttc managed-volume e2e: PASS"
