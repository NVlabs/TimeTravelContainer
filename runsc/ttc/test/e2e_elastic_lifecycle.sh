#!/bin/sh
# Copyright 2026 The gVisor Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     https://www.apache.org/licenses/LICENSE-2.0

set -eu

TTC=${TTC:-ttc}
TTCD=${TTCD:-ttcd}
IMAGE=${TTC_ELASTIC_TEST_IMAGE:-busybox:1.36}
NAME=${TTC_ELASTIC_TEST_NAME:-ttc-elastic-lifecycle-$$}
ROOT=${TTC_ELASTIC_TEST_ROOT:-$(mktemp -d "${TMPDIR:-/tmp}/ttc-elastic-lifecycle.XXXXXX")}
SHARED=${TTC_ELASTIC_TEST_SHARED_ROOT:-$ROOT/shared}
TOKEN=ttc-elastic-lifecycle-$NAME
PORT_A=${TTC_ELASTIC_TEST_PORT_A:-19883}
PORT_B=${TTC_ELASTIC_TEST_PORT_B:-19893}
PORT_C=${TTC_ELASTIC_TEST_PORT_C:-19903}
KEEP_ROOT=${TTC_ELASTIC_TEST_KEEP_ROOT:-}
COMMAND_TIMEOUT=${TTC_E2E_COMMAND_TIMEOUT:-120s}
PID_A=
PID_B=
PID_C=

socket() { printf '%s/run-%s/ttcd.sock' "$ROOT" "$1"; }
client() { node=$1; shift; TTC_SOCKET=$(socket "$node") timeout "$COMMAND_TIMEOUT" "$TTC" "$@"; }

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
	set +e
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
		test "${executable##*/}" = ttcd || continue
		kill -KILL "$root_pid" >/dev/null 2>&1 || true
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
	tail -100 "$ROOT/run-$node/ttcd.log" >&2 || true
	return 1
}

start_node() {
	node=$1
	port=$2
	shift 2
	mkdir -p "$ROOT/local-$node" "$ROOT/run-$node"
	TTC_ROOT=$SHARED TTC_LOCAL_ROOT=$ROOT/local-$node TTC_CLUSTER_TOKEN=$TOKEN \
		setsid "$TTCD" --socket "$(socket "$node")" --node "node-$node" \
		--listen "127.0.0.1:$port" --advertise "127.0.0.1:$port" \
		--memory-target 64GiB --cluster-lease 15s --unsafe-no-host-isolation "$@" \
		>"$ROOT/run-$node/ttcd.log" 2>&1 &
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
start_node a "$PORT_A" --cluster-init
start_node b "$PORT_B" --cluster-join "127.0.0.1:$PORT_A"
start_node c "$PORT_C" --cluster-join "127.0.0.1:$PORT_A"
# Learner-first admission and promotion are asynchronous. Wait until all three
# nodes are visible before asking placement to use a newly joined node.
attempt=0
while test "$attempt" -lt 300; do
	if test "$(client a node ls --json | jq 'length')" -eq 3; then break; fi
	attempt=$((attempt + 1))
	sleep 0.1
done
test "$(client a node ls --json | jq 'length')" -eq 3

client a pull "$IMAGE" >/dev/null
client a run -d --node node-a --name "$NAME" --shm-size 32m "$IMAGE" sh -c \
	'dd if=/dev/zero of=/dev/shm/resident bs=1M count=16 2>/dev/null; printf ELASTIC-MARK | dd of=/dev/shm/resident conv=notrunc 2>/dev/null; exec sleep 3600' >/dev/null
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
test "$(branch_field c child quarantined)" = true
test "$(client c exec "$NAME@child" sh -c 'dd if=/dev/shm/resident bs=12 count=1 2>/dev/null')" = ELASTIC-MARK
test "$(client c exec "$NAME@child" sha256sum /dev/shm/resident | awk '{print $1}')" = "$source_digest"

# Retiring node-a must move its remaining running branch, preserve its state,
# and stop that daemon only after no authoritative state remains on its local
# root. The destination is deliberately left to the scheduler.
client b node stop node-a --migrate-all >/dev/null
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
client c unquarantine "$NAME@child" >/dev/null
# False boolean fields are omitted from the compact JSON representation.
test "$(branch_field c child quarantined)" != true

client c rm -f "$NAME" >/dev/null
client c node stop node-c --stop-all >/dev/null
wait "$PID_C"
PID_C=
client b node stop node-b --stop-all >/dev/null
wait "$PID_B"
PID_B=

echo "TTC elastic join, migration, quarantine, and retirement lifecycle passed"
