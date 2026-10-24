#!/bin/sh

# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Exercises distributed TTC with a continuously mutating real workload: a
# page-checked Python heap and a synchronous SQLite WAL database. This covers
# process/session preservation, memory CoW, filesystem CoW, cold branching,
# migration routing, and reclamation in one lifecycle.
set -eu

TTC=${TTC:-ttc}
TTCD=${TTCD:-ttcd}
IMAGE=${TTC_DISTRIBUTED_WORKLOAD_IMAGE:-python:3.12-slim}
NAME=${TTC_DISTRIBUTED_WORKLOAD_NAME:-ttc-distributed-workload-$$}
ROOT=${TTC_DISTRIBUTED_WORKLOAD_ROOT:-$(mktemp -d "${TMPDIR:-/tmp}/ttc-distributed-workload.XXXXXX")}
SHARED=${TTC_DISTRIBUTED_WORKLOAD_SHARED_ROOT:-$ROOT/shared}
PORT_A=${TTC_DISTRIBUTED_WORKLOAD_PORT_A:-25883}
PORT_B=${TTC_DISTRIBUTED_WORKLOAD_PORT_B:-25893}
COMMAND_TIMEOUT=${TTC_E2E_COMMAND_TIMEOUT:-300s}
MEMORY_MIB=${TTC_DISTRIBUTED_WORKLOAD_MEMORY_MIB:-128}
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
PID_A=
PID_B=
SUCCESS=0

socket() { printf '%s/local-%s/ttcd.sock' "$ROOT" "$1"; }
client() { node=$1; shift; TTC_LOCAL_ROOT=$ROOT/local-$node timeout --kill-after=5s "$COMMAND_TIMEOUT" "$TTC" "$@"; }

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
	if test "${TTC_E2E_KEEP_FAILED_STATE:-0}" = 1 && test "$SUCCESS" = 0; then
		echo "TTC distributed-workload failure state retained at $ROOT" >&2
		return
	fi
	for node in b a; do
		if test -S "$(socket "$node")"; then client "$node" rm -f "$NAME" >/dev/null 2>&1; fi
	done
	for node in b a; do
		if test -S "$(socket "$node")"; then client "$node" node stop "node-$node" --stop-all >/dev/null 2>&1; fi
	done
	stop_session "$PID_B"
	stop_session "$PID_A"
	for root_pid in $(pgrep -f "[/]${ROOT#/}" 2>/dev/null || true); do
		executable=$(readlink "/proc/$root_pid/exe" 2>/dev/null || true)
		case "${executable##*/}" in ttcd|runsc|runsc-gofer) kill -KILL "$root_pid" >/dev/null 2>&1 || true ;; esac
	done
	if test -z "${TTC_DISTRIBUTED_WORKLOAD_ROOT:-}"; then
		find "$ROOT" -depth -mindepth 1 -delete 2>/dev/null || true
		rmdir "$ROOT" 2>/dev/null || true
	else
		echo "TTC distributed-workload root retained at $ROOT" >&2
	fi
}
trap cleanup EXIT HUP INT TERM

wait_ready() {
	node=$1
	for attempt in $(seq 1 400); do
		if client "$node" node ls >/dev/null 2>&1; then return 0; fi
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
		--memory-target 8GiB --cluster-lease 15s --unsafe-no-host-isolation "$@" \
		>"$ROOT/local-$node/ttcd.log" 2>&1 &
	eval "PID_$(printf '%s' "$node" | tr '[:lower:]' '[:upper:]')=$!"
	wait_ready "$node"
}

heartbeat() {
	client a exec "$1" cat /state/heartbeat 2>/dev/null || true
}

wait_heartbeat() {
	branch=$1
	minimum=${2:-25}
	for attempt in $(seq 1 600); do
		value=$(heartbeat "$branch")
		case "$value" in ''|*[!0-9]*) ;; *) test "$value" -ge "$minimum" && return 0 ;; esac
		sleep 0.1
	done
	echo "$branch did not advance its workload heartbeat to $minimum" >&2
	return 1
}

verify_branch() {
	branch=$1
	client a exec "$branch" python3 -c \
		'import sqlite3; c=sqlite3.connect("/state/workload.db"); assert c.execute("pragma integrity_check").fetchone()[0] == "ok"; assert c.execute("select count(*) from events").fetchone()[0] > 0'
	client a exec ls --json "$branch" | jq -e '.[] | select(.id == "workload" and .state == "running")' >/dev/null
}

mkdir -p "$ROOT" "$SHARED"
start_node a "$PORT_A"
start_node b "$PORT_B"

client a pull "$IMAGE" >/dev/null
client a run -d --node node-a --name "$NAME" "$IMAGE" sleep 3600 >/dev/null
client a cp "$SCRIPT_DIR/workload_fixture.py" "$NAME@main:/workload_fixture.py"
client a exec -d --session workload -e "TTC_STRESS_MEMORY_MIB=$MEMORY_MIB" \
	"$NAME@main" python3 /workload_fixture.py >/dev/null
wait_heartbeat "$NAME@main" 100

# A remote fork must preserve the live named exec, resident heap, open SQLite
# handles, and WAL state. Both branches then advance independently.
client a fork --node node-b "$NAME@main" worker >/dev/null
main_before=$(heartbeat "$NAME@main")
worker_before=$(heartbeat "$NAME@worker")
wait_heartbeat "$NAME@main" $((main_before + 100))
wait_heartbeat "$NAME@worker" $((worker_before + 100))
verify_branch "$NAME@main"
verify_branch "$NAME@worker"

# Exercise transparent command routing after both branches cross the network.
client a migrate --node node-b "$NAME@main" >/dev/null
client a migrate --node node-a "$NAME@worker" >/dev/null
verify_branch "$NAME@main"
verify_branch "$NAME@worker"

# Cold state remains inspectable, copyable, and forkable. The child becomes a
# normal independent branch while the source remains dehydrated.
client a dehydrate "$NAME@worker" >/dev/null
client a wait --condition durable "$NAME@worker" >/dev/null
client a cp "$NAME@worker:/state/heartbeat" "$ROOT/worker-heartbeat"
grep -Eq '^[0-9]+$' "$ROOT/worker-heartbeat"
client a diff --json "$NAME@worker" | jq -e 'length > 0' >/dev/null
client a fork --node node-b "$NAME@worker" coldchild >/dev/null
wait_heartbeat "$NAME@coldchild" 25
verify_branch "$NAME@coldchild"
client a rehydrate --node node-a "$NAME@worker" >/dev/null
wait_heartbeat "$NAME@worker" 25
verify_branch "$NAME@worker"

# Resource accounting must expose every public branch and never leak physical
# migration helpers into the public family view.
for branch in main worker coldchild; do
	client a stats --json "$NAME@$branch" | jq -e --arg name "$NAME" --arg branch "$branch" \
		'.branches | length == 3 and any(.[]; .target == ($name + "@" + $branch))' >/dev/null
done
test "$(client a ls --json | jq --arg name "$NAME" '[.[] | select(.name == $name) | .branches[]] | length')" -eq 3
test -z "$(client a ls --json | jq -r --arg name "$NAME" '.[] | select(.name == $name) | .branches[].id' | grep '^fork-' || true)"

client a rm -f "$NAME" >/dev/null
client b node stop node-b --stop-all >/dev/null
wait "$PID_B"
PID_B=
client a node stop node-a --stop-all >/dev/null
wait "$PID_A"
PID_A=

test -z "$(find "$ROOT/local-a/containers" "$ROOT/local-b/containers" -mindepth 1 -type d -print -quit 2>/dev/null)"
test -z "$(find "$SHARED/snapshot-packs/snapshots" -mindepth 1 -type d -print -quit 2>/dev/null)"
SUCCESS=1
echo "TTC distributed Python/SQLite/memory workload lifecycle passed"
