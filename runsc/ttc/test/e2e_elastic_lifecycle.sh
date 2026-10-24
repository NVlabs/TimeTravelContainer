#!/bin/sh

# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -eu

TTC=${TTC:-ttc}
TTCD=${TTCD:-ttcd}
IMAGE=${TTC_ELASTIC_TEST_IMAGE:-busybox:1.36}
NAME=${TTC_ELASTIC_TEST_NAME:-ttc-elastic-lifecycle-$$}
FOREGROUND_NAME=$NAME-foreground
ROOT=${TTC_ELASTIC_TEST_ROOT:-$(mktemp -d "${TMPDIR:-/tmp}/ttc-elastic-lifecycle.XXXXXX")}
SHARED=${TTC_ELASTIC_TEST_SHARED_ROOT:-$ROOT/shared}
PORT_A=${TTC_ELASTIC_TEST_PORT_A:-19883}
PORT_B=${TTC_ELASTIC_TEST_PORT_B:-19893}
PORT_C=${TTC_ELASTIC_TEST_PORT_C:-19903}
KEEP_ROOT=${TTC_ELASTIC_TEST_KEEP_ROOT:-}
KEEP_ON_FAILURE=${TTC_ELASTIC_TEST_KEEP_ON_FAILURE:-0}
COMMAND_TIMEOUT=${TTC_E2E_COMMAND_TIMEOUT:-120s}
SUCCESS=0
PID_A=
PID_B=
PID_C=

socket() { printf '%s/local-%s/ttcd.sock' "$ROOT" "$1"; }
client() { node=$1; shift; TTC_LOCAL_ROOT=$ROOT/local-$node timeout "$COMMAND_TIMEOUT" "$TTC" "$@"; }

stop_session() {
	pid=$1
	test -n "$pid" || return 0
	sid=$(ps -o sid= -p "$pid" 2>/dev/null | tr -d ' ')
	if test -n "$sid"; then
		pkill -TERM -s "$sid" >/dev/null 2>&1 || true
		sleep 0.2
		pkill -KILL -s "$sid" >/dev/null 2>&1 || true
	else
		kill "$pid" >/dev/null 2>&1 || true
	fi
	wait "$pid" >/dev/null 2>&1 || true
}

cleanup() {
	if test "$KEEP_ON_FAILURE" -ne 0 && test "$SUCCESS" -eq 0; then
		echo "TTC elastic lifecycle failure retained live at $ROOT" >&2
		return
	fi
	set +e
	for node in c b a; do
		if test -S "$(socket "$node")"; then client "$node" rm -f "$FOREGROUND_NAME" >/dev/null 2>&1; fi
	done
	if test -S "$(socket c)"; then client c rm -f "$NAME" >/dev/null 2>&1; fi
	if test -S "$(socket b)"; then client b rm -f "$NAME" >/dev/null 2>&1; fi
	if test -S "$(socket c)"; then client c node stop node-c --stop-all >/dev/null 2>&1; fi
	if test -S "$(socket b)"; then client b node stop node-b --stop-all >/dev/null 2>&1; fi
	if test -S "$(socket a)"; then client a node stop node-a --stop-all >/dev/null 2>&1; fi
	stop_session "$PID_C"
	stop_session "$PID_B"
	stop_session "$PID_A"
	root_pattern="[/]${ROOT#/}"
	for root_pid in $(pgrep -f "$root_pattern" 2>/dev/null || true); do
		executable=$(readlink "/proc/$root_pid/exe" 2>/dev/null || true)
		case "${executable##*/}" in
			ttcd|runsc|runsc-gofer) kill -KILL "$root_pid" >/dev/null 2>&1 || true ;;
		esac
	done
	if test -z "$KEEP_ROOT" && test -z "${TTC_ELASTIC_TEST_ROOT:-}"; then
		rm -rf "$ROOT"
	else
		echo "TTC elastic lifecycle root retained at $ROOT" >&2
	fi
}
trap cleanup EXIT HUP INT TERM

wait_ready() {
	node=$1
	attempt=0
	while test "$attempt" -lt 300; do
		if client "$node" node ls >/dev/null 2>&1; then return 0; fi
		attempt=$((attempt + 1))
		sleep 0.05
	done
	tail -100 "$ROOT/local-$node/ttcd.log" >&2 || true
	return 1
}

start_node() {
	node=$1
	port=$2
	shift 2
	# A daemon's root holds its socket, and must be private.
	mkdir -p -m 700 "$ROOT/local-$node"
	TTC_ROOT=$SHARED TTC_LOCAL_ROOT=$ROOT/local-$node \
		setsid "$TTCD" --node "node-$node" \
		--listen "127.0.0.1:$port" --advertise "127.0.0.1:$port" \
		--memory-target 64GiB --cluster-lease 15s --unsafe-no-host-isolation "$@" \
		>"$ROOT/local-$node/ttcd.log" 2>&1 &
	eval "PID_$(printf '%s' "$node" | tr '[:lower:]' '[:upper:]')=$!"
	wait_ready "$node"
}

branch_field() {
	node=$1
	branch=$2
	field=$3
	client "$node" ls --json | jq -r --arg name "$NAME" --arg branch "$branch" --arg field "$field" \
		'.[] | select(.name == $name or .id == $name) | .branches[] | select(.id == $branch) | .[$field]'
}

mkdir -p "$ROOT" "$SHARED"
start_node a "$PORT_A"
start_node b "$PORT_B"
start_node c "$PORT_C"
# Admission is asynchronous. Wait until all three nodes are visible before
# asking placement to use a newly joined node.
attempt=0
while test "$attempt" -lt 300; do
	if test "$(client a node ls --json | jq 'length')" -eq 3; then break; fi
	attempt=$((attempt + 1))
	sleep 0.1
done
test "$(client a node ls --json | jq 'length')" -eq 3

client a pull "$IMAGE" >/dev/null
# Foreground execution must reserve Raft ownership before starting just like a
# detached container. Once init exits, TTC preserves the real status, publishes
# the filesystem asynchronously, and makes the branch durable and unplaced.
foreground_status=0
client a run --node node-a --name "$FOREGROUND_NAME" "$IMAGE" sh -c \
	'printf FOREGROUND-MARK >/foreground-marker; exit 7' >/dev/null || foreground_status=$?
test "$foreground_status" -eq 7
client a wait --condition durable "$FOREGROUND_NAME@main" >/dev/null
foreground=$(client b ls --json | jq -c --arg name "$FOREGROUND_NAME" '.[] | select(.name == $name) | .branches[] | select(.id == "main")')
test "$(printf '%s' "$foreground" | jq -r '.state')" = exited
test "$(printf '%s' "$foreground" | jq -r '.storage_state')" = shared
test "$(printf '%s' "$foreground" | jq -r '.node_id // ""')" = ""
client c cp "$FOREGROUND_NAME@main:/foreground-marker" "$ROOT/copied-foreground-marker"
test "$(cat "$ROOT/copied-foreground-marker")" = FOREGROUND-MARK
client b rm -f "$FOREGROUND_NAME" >/dev/null

client a run -d --node node-a --name "$NAME" --shm-size 32m -v /work "$IMAGE" sh -c \
	'dd if=/dev/zero of=/dev/shm/resident bs=1M count=16 2>/dev/null; printf ELASTIC-MARK | dd of=/dev/shm/resident conv=notrunc 2>/dev/null; printf VOLUME-MARK >/work/marker; exec sleep 3600' >/dev/null
# Detached run returns once the runtime starts, before the guest entrypoint is
# required to finish its setup. Wait for the fork point explicitly so the
# reference digest cannot race the initial zero fill and marker write.
attempt=0
while test "$attempt" -lt 300; do
	if test "$(client a exec "$NAME@main" sh -c 'dd if=/dev/shm/resident bs=12 count=1 2>/dev/null' 2>/dev/null || true)" = ELASTIC-MARK; then break; fi
	attempt=$((attempt + 1))
	sleep 0.05
done
test "$(client a exec "$NAME@main" sh -c 'dd if=/dev/shm/resident bs=12 count=1 2>/dev/null')" = ELASTIC-MARK
source_digest=$(client a exec "$NAME@main" sha256sum /dev/shm/resident | awk '{print $1}')
client a fork --node node-b "$NAME@main" child >/dev/null
test "$(client b exec "$NAME@child" sha256sum /dev/shm/resident | awk '{print $1}')" = "$source_digest"
client b quarantine "$NAME@child" >/dev/null
client b migrate --node node-c "$NAME@child" >/dev/null
test "$(branch_field c child node_id)" = node-c
quarantined=$(branch_field c child quarantined)
if test "$quarantined" != true; then
	echo "migrated quarantine state is $quarantined, want true" >&2
	client c ls --json >&2 || true
	find "$ROOT/local-c/containers" -name container.json -type f -exec sh -c 'echo "=== $1"; sed -n "1,240p" "$1"' sh {} \; >&2 || true
	exit 1
fi
test "$(client c exec "$NAME@child" sh -c 'dd if=/dev/shm/resident bs=12 count=1 2>/dev/null')" = ELASTIC-MARK
test "$(client c exec "$NAME@child" sha256sum /dev/shm/resident | awk '{print $1}')" = "$source_digest"
test "$(client c exec "$NAME@child" cat /work/marker)" = VOLUME-MARK
test "$(client a exec "$NAME@main" sh -c 'dd if=/dev/shm/resident bs=12 count=1 2>/dev/null')" = ELASTIC-MARK

# Retiring node-a must move its remaining running branch, preserve its state,
# and stop that daemon only after no authoritative state remains on its local
# root. The destination is deliberately left to the scheduler.
if ! client b node stop node-a --migrate-all >/dev/null; then
	echo "node-a retirement failed; cluster inventory follows" >&2
	client b ls --json >&2 || true
	find "$SHARED/containers" -name container.json -type f -exec sh -c 'echo "=== $1"; sed -n "1,240p" "$1"' sh {} \; >&2 || true
	exit 1
fi
wait "$PID_A"
PID_A=
owner=$(branch_field b main node_id)
case "$owner" in
	node-b) owner_client=b ;;
	node-c) owner_client=c ;;
	*) echo "retired node-a left main on unexpected owner $owner" >&2; exit 1 ;;
esac
test "$(client "$owner_client" exec "$NAME@main" sh -c 'dd if=/dev/shm/resident bs=12 count=1 2>/dev/null')" = ELASTIC-MARK
test "$(client "$owner_client" exec "$NAME@main" sha256sum /dev/shm/resident | awk '{print $1}')" = "$source_digest"
test "$(client "$owner_client" exec "$NAME@main" cat /work/marker)" = VOLUME-MARK
client c unquarantine "$NAME@child" >/dev/null
# False boolean fields are omitted from the compact JSON representation.
test "$(branch_field c child quarantined)" != true

client c rm -fv "$NAME" >/dev/null
client c node stop node-c --stop-all >/dev/null
wait "$PID_C"
PID_C=
client b node stop node-b --stop-all >/dev/null
wait "$PID_B"
PID_B=

# Private distributed-fork and migration containers are implementation
# details, but they still own image leases and graph roots. A successful
# family removal must reclaim them rather than merely hiding them from ls.
test "$(find "$SHARED/containers" -mindepth 1 -maxdepth 1 -type d 2>/dev/null | wc -l)" -eq 0
test "$(find "$SHARED/removed-containers" -mindepth 1 2>/dev/null | wc -l)" -eq 0

SUCCESS=1
echo "TTC elastic join, migration, quarantine, and retirement lifecycle passed"
