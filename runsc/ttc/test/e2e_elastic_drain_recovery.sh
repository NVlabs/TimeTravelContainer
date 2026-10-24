#!/bin/sh

# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -eu

TTC=${TTC:-ttc}
TTCD=${TTCD:-ttcd}
IMAGE=${TTC_ELASTIC_DRAIN_IMAGE:-busybox:1.36}
ROOT=${TTC_ELASTIC_DRAIN_ROOT:-$(mktemp -d "${TMPDIR:-/tmp}/ttc-elastic-drain.XXXXXX")}
SHARED=${TTC_ELASTIC_DRAIN_SHARED_ROOT:-$ROOT/shared}
NAME=drain-recovery-$$
COMMAND_TIMEOUT=${TTC_E2E_COMMAND_TIMEOUT:-120s}
PORT_A=${TTC_ELASTIC_DRAIN_PORT_A:-20583}
PORT_B=${TTC_ELASTIC_DRAIN_PORT_B:-20593}
PORT_C=${TTC_ELASTIC_DRAIN_PORT_C:-20603}
PID_A=
PID_B=
PID_C=
STOP_CLIENT=

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
	test -z "$STOP_CLIENT" || kill -KILL "$STOP_CLIENT" >/dev/null 2>&1 || true
	for node in b c a; do
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
	if test -z "${TTC_ELASTIC_DRAIN_ROOT:-}"; then rm -rf "$ROOT"; else echo "TTC elastic drain root retained at $ROOT" >&2; fi
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

wait_ready() {
	node=$1
	for attempt in $(seq 1 600); do
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
		--memory-target 64GiB --cluster-lease 6s --unsafe-no-host-isolation "$@" \
		>"$ROOT/local-$node/ttcd.log" 2>&1 &
	eval "PID_$(printf '%s' "$node" | tr '[:lower:]' '[:upper:]')=$!"
	wait_ready "$node"
}

mkdir -p "$ROOT" "$SHARED"
start_node a "$PORT_A"
start_node b "$PORT_B"
start_node c "$PORT_C"

client a pull "$IMAGE" >/dev/null
client a run -d --node node-a --name "$NAME" "$IMAGE" sh -c 'printf RECOVERED >/tmp/marker; exec sleep 3600' >/dev/null
client a fork --node node-a "$NAME@main" b1 b2 b3 b4 b5 b6 b7 >/dev/null

# Begin a scheduler-selected retirement, then kill only ttcd after its local
# durable intent is visible. The lease guardian host-stops (does not kill) the
# Sentries. The command client is expected to fail because its transport died.
client b node stop node-a --migrate-all >"$ROOT/node-stop.out" 2>"$ROOT/node-stop.err" &
STOP_CLIENT=$!
intent=$SHARED/cluster/drains/node-a.json
for attempt in $(seq 1 2000); do
	test -s "$intent" && break
	kill -0 "$PID_A" 2>/dev/null || break
	sleep 0.001
done
test -s "$intent"
kill -KILL "$PID_A"
wait "$PID_A" >/dev/null 2>&1 || true
PID_A=
wait "$STOP_CLIENT" >/dev/null 2>&1 || true
STOP_CLIENT=

# The new incarnation waits out the old lease, adopts its fenced placements,
# resumes the exact Sentries, and continues the durable drain intent. Recovery
# must stop this daemon automatically when no authoritative local state remains.
start_node a "$PORT_A"
for attempt in $(seq 1 1200); do
	if ! kill -0 "$PID_A" 2>/dev/null; then break; fi
	sleep 0.05
done
if kill -0 "$PID_A" 2>/dev/null; then
	echo "recovered draining daemon did not stop automatically" >&2
	exit 1
fi
wait "$PID_A" >/dev/null 2>&1 || true
PID_A=

# Node-local state is no longer authoritative after a successful drain.
client b ls --json >"$ROOT/after-drain.json"
rm -rf "$ROOT/local-a"
for branch in main b1 b2 b3 b4 b5 b6 b7; do
	owner=$(jq -r --arg name "$NAME" --arg branch "$branch" \
		'.[] | select(.name == $name) | .branches[] | select(.id == $branch) | .node_id' "$ROOT/after-drain.json")
	case "$owner" in node-b) node=b ;; node-c) node=c ;; *) echo "unexpected owner $owner for $branch" >&2; cat "$ROOT/after-drain.json" >&2; exit 1 ;; esac
	test "$(client "$node" exec "$NAME@$branch" cat /tmp/marker)" = RECOVERED
done

client b rm -f "$NAME" >/dev/null
client c node stop node-c --stop-all >/dev/null
wait "$PID_C"
PID_C=
client b node stop node-b --stop-all >/dev/null
wait "$PID_B"
PID_B=

echo "TTC interrupted node retirement recovered and evacuated all branches"
