#!/bin/sh

# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -eu

TTC=${TTC:-ttc}
TTCD=${TTCD:-ttcd}
IMAGE=${TTC_DISTRIBUTED_FANOUT_IMAGE:-busybox:1.36}
COUNT=${TTC_DISTRIBUTED_FANOUT_BRANCHES:-64}
NAME=${TTC_DISTRIBUTED_FANOUT_NAME:-ttc-distributed-fanout-$$}
ROOT=${TTC_DISTRIBUTED_FANOUT_ROOT:-$(mktemp -d "${TMPDIR:-/tmp}/ttc-distributed-fanout.XXXXXX")}
SHARED=${TTC_DISTRIBUTED_FANOUT_SHARED_ROOT:-$ROOT/shared}
LOCAL_BASE=${TTC_DISTRIBUTED_FANOUT_LOCAL_ROOT:-$ROOT}
PORT_A=${TTC_DISTRIBUTED_FANOUT_PORT_A:-22483}
PORT_B=${TTC_DISTRIBUTED_FANOUT_PORT_B:-22493}
PORT_C=${TTC_DISTRIBUTED_FANOUT_PORT_C:-22503}
COMMAND_TIMEOUT=${TTC_E2E_COMMAND_TIMEOUT:-900s}
PAUSE_BEFORE_DIRTY=${TTC_DISTRIBUTED_FANOUT_PAUSE_BEFORE_DIRTY:-0}
KEEP_ON_FAILURE=${TTC_DISTRIBUTED_FANOUT_KEEP_ON_FAILURE:-0}
SUCCESS=0
PID_A=
PID_B=
PID_C=

test "$COUNT" -ge 2

socket() { printf '%s/local-%s/ttcd.sock' "$LOCAL_BASE" "$1"; }
client() { node=$1; shift; TTC_LOCAL_ROOT=$LOCAL_BASE/local-$node timeout "$COMMAND_TIMEOUT" "$TTC" "$@"; }

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
		echo "TTC distributed fanout failure retained live at $ROOT" >&2
		return
	fi
	set +e
	for node in a c b; do
		if test -S "$(socket "$node")"; then client "$node" rm -f "$NAME" >/dev/null 2>&1; fi
	done
	for node in c b a; do
		if test -S "$(socket "$node")"; then client "$node" node stop "node-$node" --stop-all >/dev/null 2>&1; fi
	done
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
	if test -z "${TTC_DISTRIBUTED_FANOUT_ROOT:-}"; then
		rm -rf "$ROOT"
	else
		echo "TTC distributed fanout root retained at $ROOT" >&2
	fi
}
trap cleanup EXIT HUP INT TERM

wait_ready() {
	node=$1
	for attempt in $(seq 1 400); do
		if client "$node" node ls >/dev/null 2>&1; then return 0; fi
		sleep 0.05
	done
	tail -100 "$LOCAL_BASE/local-$node/ttcd.log" >&2 || true
	return 1
}

start_node() {
	node=$1
	port=$2
	shift 2
	# A daemon's root holds its socket, and must be private.
	mkdir -p -m 700 "$LOCAL_BASE/local-$node"
	TTC_ROOT=$SHARED TTC_LOCAL_ROOT=$LOCAL_BASE/local-$node \
		setsid "$TTCD" --node "node-$node" \
		--listen "127.0.0.1:$port" --advertise "127.0.0.1:$port" \
		--memory-target 64GiB --cluster-lease 15s --unsafe-no-host-isolation "$@" \
		>"$LOCAL_BASE/local-$node/ttcd.log" 2>&1 &
	eval "PID_$(printf '%s' "$node" | tr '[:lower:]' '[:upper:]')=$!"
	wait_ready "$node"
}

branch_count() {
	client a ls --json | jq --arg name "$NAME" \
		'[.[] | select(.name == $name) | .branches[]] | length'
}

node_branch_count() {
	node=$1
	client a ls --json | jq --arg name "$NAME" --arg node "$node" \
		'[.[] | select(.name == $name) | .branches[] | select(.node_id == $node)] | length'
}

# The cluster keeps its authority's log in its shared root, as the snapshot
# and log segments of its latest epoch, and no node keeps one in its local
# root.
authority_log_retained() {
	authority=$SHARED/cluster/authority
	latest=$(ls "$authority/epochs" | sort | tail -n 1)
	test -n "$latest"
	test -f "$authority/$latest/prepared"
	test -f "$authority/$latest/snapshot"
	test -n "$(find "$authority/$latest" -name 'log-*' -print -quit)"
	test -z "$(find "$LOCAL_BASE"/local-? -path '*/cluster/authority/*' -print -quit)"
}

mkdir -p "$ROOT" "$SHARED"
start_node a "$PORT_A"
start_node b "$PORT_B"
start_node c "$PORT_C"

for attempt in $(seq 1 300); do
	test "$(client a node ls --json | jq 'length')" -eq 3 && break
	sleep 0.1
done
test "$(client a node ls --json | jq 'length')" -eq 3

client a pull "$IMAGE" >/dev/null
client a run -d --node node-a --name "$NAME" --shm-size 4m "$IMAGE" sh -c \
	'dd if=/dev/zero of=/dev/shm/state bs=1M count=2 2>/dev/null; touch /ready; exec sleep 3600' >/dev/null
for attempt in $(seq 1 300); do
	client a exec "$NAME@main" test -e /ready >/dev/null 2>&1 && break
	sleep 0.05
done
client a exec "$NAME@main" test -e /ready

half=$((COUNT / 2))
branches_b=
branches_c=
index=1
while test "$index" -le "$COUNT"; do
	if test "$index" -le "$half"; then
		branches_b="$branches_b b$index"
	else
		branches_c="$branches_c b$index"
	fi
	index=$((index + 1))
done
# shellcheck disable=SC2086 # Each word is a generated, validated branch name.
client a fork --node node-b "$NAME@main" $branches_b >/dev/null
# shellcheck disable=SC2086 # Each word is a generated, validated branch name.
client a fork --node node-c "$NAME@main" $branches_c >/dev/null
test "$(branch_count)" -eq $((COUNT + 1))
test "$(node_branch_count node-b)" -eq "$half"
test "$(node_branch_count node-c)" -eq $((COUNT - half))

if test "$PAUSE_BEFORE_DIRTY" -gt 0; then
	echo "TTC distributed fanout pausing ${PAUSE_BEFORE_DIRTY}s before branch writes" >&2
	sleep "$PAUSE_BEFORE_DIRTY"
fi

# Dirty one private page in every remote branch. Bound the number of concurrent
# client/RPC processes while still exercising interleaved CoW faults.
pending=
pending_count=0
index=1
while test "$index" -le "$COUNT"; do
	digit=$((index % 10))
	offset=$((index * 4096))
	client a exec "$NAME@b$index" sh -c \
		"printf '%s' '$digit' | dd of=/dev/shm/state bs=1 seek=$offset conv=notrunc 2>/dev/null" >/dev/null &
	pending="$pending $!"
	pending_count=$((pending_count + 1))
	if test "$pending_count" -eq 32; then
		for pid in $pending; do wait "$pid"; done
		pending=
		pending_count=0
	fi
	index=$((index + 1))
done
for pid in $pending; do wait "$pid"; done

# Retire a node that owns half of the high-fanout family. Every branch must be
# migrated before the daemon exits, and none may retain node-b as its owner.
client a node stop node-b --migrate-all >/dev/null
wait "$PID_B"
PID_B=
test "$(node_branch_count node-b)" -eq 0
test "$(branch_count)" -eq $((COUNT + 1))
# A retired node's scratch is disposable. In particular, evacuation must not
# retain the migrated family's rootfs repository or console cache after every
# sparse dependency has crossed the independence fence.
test -z "$(find "$LOCAL_BASE/local-b/containers" -mindepth 1 -maxdepth 1 -type d -print -quit 2>/dev/null)"

# Bound verification concurrency just like the dirtying phase. At hundreds of
# branches a serial loop mostly measures command startup; interleaved reads are
# both faster and a stronger exercise of post-migration routing.
pending=
pending_count=0
index=1
while test "$index" -le "$COUNT"; do
	digit=$((index % 10))
	offset=$((index * 4096))
	(
		got=$(client a exec "$NAME@b$index" sh -c \
			"dd if=/dev/shm/state bs=1 skip=$offset count=1 2>/dev/null")
		test "$got" = "$digit" || {
			echo "TTC distributed fanout: branch b$index read '$got', want '$digit'" >&2
			exit 1
		}
	) &
	pending="$pending $!"
	pending_count=$((pending_count + 1))
	if test "$pending_count" -eq 32; then
		for pid in $pending; do wait "$pid"; done
		pending=
		pending_count=0
	fi
	index=$((index + 1))
done
for pid in $pending; do wait "$pid"; done

client a rm -f "$NAME" >/dev/null
test "$(client a ls --json | jq --arg name "$NAME" '[.[] | select(.name == $name)] | length')" -eq 0
client c node stop node-c --stop-all >/dev/null
wait "$PID_C"
PID_C=
client a node stop node-a --stop-all >/dev/null
wait "$PID_A"
PID_A=

# Cleanly stopping the whole cluster must retain its authority's log to
# restart from.
# Restart a node, which takes the authority; startup also exercises a fresh
# shared-reference-ledger reconstruction.
authority_log_retained
start_node a "$PORT_A"
test "$(client a ls --json | jq 'length')" -eq 0
for attempt in $(seq 1 600); do
	test -z "$(find "$SHARED/snapshot-packs/snapshots" -mindepth 1 -maxdepth 1 -type d -print -quit 2>/dev/null)" && break
	sleep 0.1
done
test -z "$(find "$SHARED/snapshot-packs/snapshots" -mindepth 1 -maxdepth 1 -type d -print -quit 2>/dev/null)"
client a node stop node-a --stop-all >/dev/null
wait "$PID_A"
PID_A=
authority_log_retained

SUCCESS=1
echo "TTC $COUNT-branch distributed fanout and retirement lifecycle passed"
