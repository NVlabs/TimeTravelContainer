#!/bin/sh

# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Exercises branch lifecycle transitions and daemon retirement policies through
# the distributed command router. The other elastic tests emphasize migration
# data integrity; this one deliberately composes paused, dehydrated, exited,
# restarted, unplaced, and re-placed branches in one family.
set -eu

TTC=${TTC:-ttc}
TTCD=${TTCD:-ttcd}
IMAGE=${TTC_LIFECYCLE_MATRIX_IMAGE:-busybox:1.36}
NAME=${TTC_LIFECYCLE_MATRIX_NAME:-ttc-lifecycle-matrix-$$}
ROOT=${TTC_LIFECYCLE_MATRIX_ROOT:-$(mktemp -d "${TMPDIR:-/tmp}/ttc-lifecycle-matrix.XXXXXX")}
PORT_A=${TTC_LIFECYCLE_MATRIX_PORT_A:-24783}
PORT_B=${TTC_LIFECYCLE_MATRIX_PORT_B:-24793}
PORT_C=${TTC_LIFECYCLE_MATRIX_PORT_C:-24803}
COMMAND_TIMEOUT=${TTC_E2E_COMMAND_TIMEOUT:-120s}
KEEP_ROOT=${TTC_LIFECYCLE_MATRIX_KEEP_ROOT:-}
KEEP_LIVE_ON_FAILURE=${TTC_LIFECYCLE_MATRIX_KEEP_LIVE_ON_FAILURE:-}
SUCCESS=
PID_A=
PID_B=
PID_C=

socket() { printf '%s/local-%s/ttcd.sock' "$ROOT" "$1"; }
client() { node=$1; shift; TTC_LOCAL_ROOT=$ROOT/local-$node timeout "$COMMAND_TIMEOUT" "$TTC" "$@"; }

fail() {
	echo "TTC distributed lifecycle matrix: $*" >&2
	exit 1
}

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
	if test -n "$KEEP_LIVE_ON_FAILURE" && test -z "$SUCCESS"; then
		echo "TTC lifecycle-matrix failure retained live at $ROOT" >&2
		return
	fi
	for node in c b a; do
		if test -S "$(socket "$node")"; then client "$node" rm -f "$NAME" >/dev/null 2>&1; fi
	done
	for node in c b a; do
		if test -S "$(socket "$node")"; then client "$node" node stop "node-$node" --stop-all >/dev/null 2>&1; fi
	done
	stop_session "$PID_C"
	stop_session "$PID_B"
	stop_session "$PID_A"
	if test -z "$KEEP_ROOT" && test -z "${TTC_LIFECYCLE_MATRIX_ROOT:-}"; then
		rm -rf "$ROOT"
	else
		echo "TTC lifecycle-matrix root retained at $ROOT" >&2
	fi
}
trap cleanup EXIT HUP INT TERM

wait_ready() {
	node=$1
	attempt=0
	while test "$attempt" -lt 600; do
		if client "$node" node ls --json >/dev/null 2>&1; then return 0; fi
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
	TTC_ROOT=$ROOT/shared TTC_LOCAL_ROOT=$ROOT/local-$node \
		setsid "$TTCD" --node "node-$node" \
		--listen "127.0.0.1:$port" --advertise "127.0.0.1:$port" \
		--memory-target 8GiB --unsafe-no-host-isolation "$@" \
		>"$ROOT/local-$node/ttcd.log" 2>&1 &
	eval "PID_$(printf '%s' "$node" | tr '[:lower:]' '[:upper:]')=$!"
	wait_ready "$node"
}

branch_json() {
	node=$1
	branch=$2
	client "$node" ls --json | jq -c --arg name "$NAME" --arg branch "$branch" \
		'.[] | select(.name == $name) | .branches[] | select(.id == $branch)'
}

assert_state() {
	node=$1
	branch=$2
	want=$3
	got=$(branch_json "$node" "$branch" | jq -r .state)
	test "$got" = "$want" || fail "$NAME@$branch state is $got, want $want"
}

assert_owner() {
	node=$1
	branch=$2
	want=$3
	got=$(branch_json "$node" "$branch" | jq -r '.node_id // ""')
	test "$got" = "$want" || fail "$NAME@$branch owner is $got, want $want"
}

assert_marker() {
	node=$1
	branch=$2
	test "$(client "$node" exec "$NAME@$branch" cat /state/marker)" = MATRIX-MARK || \
		fail "$NAME@$branch lost its writable filesystem marker"
}

assert_no_processes_for_root() {
	root=$1
	for cmdline in /proc/[0-9]*/cmdline; do
		test -r "$cmdline" || continue
		command=$(tr '\000' ' ' <"$cmdline" 2>/dev/null || true)
		case "$command" in
		*"$root/"*) fail "retired node left host process alive: $command" ;;
		esac
	done
}

mkdir -p "$ROOT/shared"
start_node a "$PORT_A"
start_node b "$PORT_B"
start_node c "$PORT_C"

attempt=0
while test "$attempt" -lt 600; do
	if test "$(client a node ls --json | jq 'length')" -eq 3; then break; fi
	attempt=$((attempt + 1))
	sleep 0.05
done
test "$(client a node ls --json | jq 'length')" -eq 3

client a pull "$IMAGE" >/dev/null
client a run -d --node node-a --name "$NAME" "$IMAGE" sh -c \
	'mkdir -p /state; printf MATRIX-MARK >/state/marker; exec sleep 3600' >/dev/null
attempt=0
while test "$attempt" -lt 300; do
	if test "$(client a exec "$NAME@main" cat /state/marker 2>/dev/null || true)" = MATRIX-MARK; then break; fi
	attempt=$((attempt + 1))
	sleep 0.05
done
assert_marker a main

# Paused stop must discard execution without first resuming guest code. The
# next start is a fresh init process on the requested node, with disk state
# intact.
client a fork --node node-b "$NAME@main" paused-stop >/dev/null
client b pause "$NAME@paused-stop" >/dev/null
assert_state a paused-stop paused
client a stop "$NAME@paused-stop" >/dev/null
client a wait --condition durable "$NAME@paused-stop" >/dev/null
assert_state c paused-stop exited
client c start --node node-c "$NAME@paused-stop" >/dev/null
assert_state a paused-stop running
assert_owner a paused-stop node-c
assert_marker c paused-stop

# A dehydrated branch can be explicitly stopped without rehydration, then
# started elsewhere as an ordinary exited branch.
client a fork --node node-b "$NAME@main" cold-stop >/dev/null
client c dehydrate "$NAME@cold-stop" >/dev/null
client a wait --condition durable "$NAME@cold-stop" >/dev/null
assert_state b cold-stop dehydrated
client b stop "$NAME@cold-stop" >/dev/null
client c wait --condition durable "$NAME@cold-stop" >/dev/null
assert_state a cold-stop exited
client a start --node node-b "$NAME@cold-stop" >/dev/null
assert_owner c cold-stop node-b
assert_marker b cold-stop

# Restart is stop+start for non-running states too. It must not implicitly run
# a paused branch before discarding it, and a cold unplaced branch must be
# placed by the scheduler.
client a fork --node node-a "$NAME@main" paused-restart >/dev/null
client c pause "$NAME@paused-restart" >/dev/null
client b restart "$NAME@paused-restart" >/dev/null
assert_state a paused-restart running
assert_marker a paused-restart

client a fork --node node-b "$NAME@main" cold-restart >/dev/null
client a dehydrate "$NAME@cold-restart" >/dev/null
client c wait --condition durable "$NAME@cold-restart" >/dev/null
client c restart "$NAME@cold-restart" >/dev/null
assert_state a cold-restart running
owner=$(branch_json a cold-restart | jq -r .node_id)
case "$owner" in
	node-a) assert_marker a cold-restart ;;
	node-b) assert_marker b cold-restart ;;
	node-c) assert_marker c cold-restart ;;
	*) fail "$NAME@cold-restart has invalid owner $owner" ;;
esac

# A daemon with authoritative execution cannot silently choose a destructive
# default. Retirement with --dehydrate-all publishes complete state and leaves
# those branches unplaced for an explicit later rehydration.
if client a daemon stop >"$ROOT/no-policy.out" 2>"$ROOT/no-policy.err"; then
	fail "daemon stop without a policy accepted live branches"
fi
grep -Eq 'policy|running|paused|branch' "$ROOT/no-policy.err"
client b node stop node-a --dehydrate-all >/dev/null
wait "$PID_A"
PID_A=
test "$(client b node ls --json | jq 'length')" -eq 2
assert_state b main dehydrated
assert_owner b main ""
client c rehydrate --node node-b "$NAME@main" >/dev/null
assert_owner c main node-b
assert_marker b main

# The local daemon form uses the same drain state machine. All remaining
# node-b execution must move before its socket and process disappear.
client b daemon stop --migrate-all >/dev/null
wait "$PID_B"
PID_B=
assert_no_processes_for_root "$ROOT/local-b"
test "$(client c node ls --json | jq 'length')" -eq 1
client c ls --json | jq -e --arg name "$NAME" \
	'all(.[] | select(.name == $name).branches[]; (.node_id // "") != "node-b")' >/dev/null
assert_marker c main

client c rm -f "$NAME" >/dev/null
client c daemon stop >/dev/null
wait "$PID_C"
PID_C=

SUCCESS=1
echo "TTC distributed branch-state and daemon-policy matrix passed"
