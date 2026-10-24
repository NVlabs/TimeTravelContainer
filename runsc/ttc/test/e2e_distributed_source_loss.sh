#!/bin/sh

# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -eu

TTC=${TTC:-ttc}
TTCD=${TTCD:-ttcd}
IMAGE=${TTC_DISTRIBUTED_TEST_IMAGE:-busybox:1.36}
NAME=${TTC_DISTRIBUTED_TEST_NAME:-ttc-distributed-source-loss-$$}
SAFE_NAME=$NAME-independent
ROOT=${TTC_DISTRIBUTED_TEST_ROOT:-$(mktemp -d "${TMPDIR:-/tmp}/ttc-distributed-source-loss.XXXXXX")}
SHARED=${TTC_DISTRIBUTED_TEST_SHARED_ROOT:-$ROOT/shared}
PORT_A=${TTC_DISTRIBUTED_TEST_PORT_A:-17783}
PORT_B=${TTC_DISTRIBUTED_TEST_PORT_B:-17793}
PORT_C=${TTC_DISTRIBUTED_TEST_PORT_C:-17803}
KEEP_ROOT=${TTC_DISTRIBUTED_TEST_KEEP_ROOT:-}
COMMAND_TIMEOUT=${TTC_E2E_COMMAND_TIMEOUT:-120s}
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
	set +e
	if test -S "$(socket c)"; then client c rm -f "$SAFE_NAME" >/dev/null 2>&1; fi
	if test -S "$(socket b)"; then client b rm -f "$NAME" >/dev/null 2>&1; fi
	if test -S "$(socket c)"; then client c node stop node-c --stop-all >/dev/null 2>&1; fi
	if test -S "$(socket b)"; then client b node stop node-b --stop-all >/dev/null 2>&1; fi
	stop_session "$PID_C"
	stop_session "$PID_B"
	stop_session "$PID_A"
	# runsc intentionally starts Sentries, gofers, and console followers in
	# separate sessions. A simulated host loss cannot ask them to exit, so reap
	# only processes whose argv names this test's private root.
	root_pattern="[/]${ROOT#/}"
	for root_pid in $(pgrep -f "$root_pattern" 2>/dev/null || true); do
		executable=$(readlink "/proc/$root_pid/exe" 2>/dev/null || true)
		case "${executable##*/}" in
			ttcd|runsc|runsc-gofer) kill -KILL "$root_pid" >/dev/null 2>&1 || true ;;
		esac
	done
	if test -z "$KEEP_ROOT" && test -z "${TTC_DISTRIBUTED_TEST_ROOT:-}"; then
		rm -rf "$ROOT"
	else
		echo "TTC distributed source-loss root retained at $ROOT" >&2
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
		--memory-target 64GiB --cluster-lease 15s --cluster-prefetch none \
		--cluster-push demand-only --unsafe-no-host-isolation "$@" \
		>"$ROOT/local-$node/ttcd.log" 2>&1 &
	eval "PID_$(printf '%s' "$node" | tr '[:lower:]' '[:upper:]')=$!"
	wait_ready "$node"
}

mkdir -p "$ROOT" "$SHARED"
start_node a "$PORT_A"
start_node b "$PORT_B"
start_node c "$PORT_C"
# Node A, the first to start, holds the cluster's authority. Let both joined
# nodes settle before deliberately removing it.
sleep 8

client a pull "$IMAGE" >/dev/null
client a run -d --node node-a --name "$NAME" --shm-size 320m "$IMAGE" sh -c \
	'dd if=/dev/zero of=/dev/shm/resident bs=1M count=256 2>/dev/null; exec sleep 3600' >/dev/null
client c run -d --node node-c --name "$SAFE_NAME" "$IMAGE" sleep 3600 >/dev/null
client a fork --node node-b "$NAME@main" dependent >/dev/null

# Kill the complete source-node session before background publication can make
# the target independent. A surviving node must take the authority over.
stop_session "$PID_A"
PID_A=

attempt=0
while test "$attempt" -lt 500; do
	if test ! -S "$(socket b)"; then
		echo "surviving node-b self-fenced after source loss" >&2
		tail -100 "$ROOT/local-b/ttcd.log" >&2 || true
		exit 1
	fi
	state=$(client b ls --json | jq -r --arg name "$NAME" '.[] | select(.name == $name or .id == $name) | .branches[] | select(.id == "dependent") | .state')
	if test "$state" = corrupted; then break; fi
	attempt=$((attempt + 1))
	sleep 0.1
done
test "$state" = corrupted || {
	echo "dependent target state=$state, want corrupted" >&2
	client b ls --json >&2 || true
	exit 1
}
if client b exec "$NAME@dependent" true >/dev/null 2>&1; then
	echo "corrupted target remained executable" >&2
	exit 1
fi
client c exec "$SAFE_NAME@main" true

# Corruption is terminal and only removal remains legal.
client b rm -f "$NAME@dependent" >/dev/null
client c rm -f "$SAFE_NAME" >/dev/null
echo "TTC distributed volatile source-loss fencing passed"
