#!/bin/sh

# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Exercises a login-node-style control-only daemon against two compute nodes.
# All user operations enter through the outbound-only gateway.
set -eu

TTC=${TTC:-ttc}
TTCD=${TTCD:-ttcd}
IMAGE=${TTC_CONTROL_ONLY_IMAGE:-busybox:1.36}
NAME=${TTC_CONTROL_ONLY_NAME:-ttc-control-only-$$}
ROOT=${TTC_CONTROL_ONLY_ROOT:-$(mktemp -d "${TMPDIR:-/tmp}/ttc-control-only.XXXXXX")}
PORT_A=${TTC_CONTROL_ONLY_PORT_A:-28383}
PORT_B=${TTC_CONTROL_ONLY_PORT_B:-28393}
HOST_PORT=${TTC_CONTROL_ONLY_HOST_PORT:-28403}
SUCCESS=
PID_A=
PID_B=
PID_CONTROL=

fail() {
	echo "TTC control-only e2e: $*" >&2
	exit 1
}

socket() { printf '%s/local-%s/ttcd.sock' "$ROOT" "$1"; }
client() { node=$1; shift; TTC_LOCAL_ROOT=$ROOT/local-$node timeout 120s "$TTC" "$@"; }

stop_session() {
	pid=$1
	test -n "$pid" || return 0
	sid=$(ps -o sid= -p "$pid" 2>/dev/null | tr -d ' ')
	if test -n "$sid"; then
		pkill -TERM -s "$sid" >/dev/null 2>&1 || true
		sleep 0.2
		pkill -KILL -s "$sid" >/dev/null 2>&1 || true
	else
		kill -KILL "$pid" >/dev/null 2>&1 || true
	fi
	wait "$pid" >/dev/null 2>&1 || true
}

cleanup() {
	set +e
	if test -S "$(socket control)"; then
		client control rm -f "$NAME" >/dev/null 2>&1
		client control daemon stop >/dev/null 2>&1
	fi
	if test -S "$(socket a)"; then
		client a node stop node-b --stop-all >/dev/null 2>&1
		client a node stop node-a --stop-all >/dev/null 2>&1
	fi
	stop_session "$PID_CONTROL"
	stop_session "$PID_B"
	stop_session "$PID_A"
	if test -z "${TTC_CONTROL_ONLY_ROOT:-}" && test -n "$SUCCESS"; then
		find "$ROOT" -depth -mindepth 1 -delete 2>/dev/null || true
		rmdir "$ROOT" 2>/dev/null || true
	else
		echo "TTC control-only root retained at $ROOT" >&2
	fi
}
trap cleanup EXIT HUP INT TERM

wait_ready() {
	node=$1
	for _ in $(seq 1 300); do
		if client "$node" daemon status >/dev/null 2>&1; then return 0; fi
		sleep 0.05
	done
	return 1
}

start_compute() {
	node=$1
	port=$2
	shift 2
	mkdir -p "$ROOT/shared"
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

branch_owner() {
	branch=$1
	client control ls --json | jq -r --arg family "$NAME" --arg branch "$branch" \
		'.[] | select(.name == $family) | .branches[] | select(.id == $branch) | .node_id'
}

test -x "$TTC" || fail "TTC is not executable: $TTC"
test -x "$TTCD" || fail "TTCD is not executable: $TTCD"
mkdir -p -m 700 "$ROOT/local-control"

start_compute a "$PORT_A"
start_compute b "$PORT_B"
for _ in $(seq 1 300); do
	test "$(client a node ls --json 2>/dev/null | jq 'length' 2>/dev/null || echo 0)" -eq 2 && break
	sleep 0.05
done
test "$(client a node ls --json | jq 'length')" -eq 2 || fail "compute nodes did not converge"

TTC_ROOT=$ROOT/shared TTC_LOCAL_ROOT=$ROOT/local-control \
	setsid "$TTCD" --control-only \
	>"$ROOT/local-control/ttcd.log" 2>&1 &
PID_CONTROL=$!
wait_ready control
client control daemon status | jq -e '.control_only == true' >/dev/null
test "$(client control node ls --json | jq 'length')" -eq 2 || fail "control-only daemon entered node inventory"

client control pull "$IMAGE" >/dev/null
client control run -d --node node-b --network ttc --nat --name "$NAME" "$IMAGE" sh -c \
	'mkdir -p /www; echo control-route >/www/index.html; exec httpd -f -p 8080 -h /www' >/dev/null
test "$(branch_owner main)" = node-b
client control exec "$NAME@main" test -f /www/index.html

client control fork --node node-a "$NAME@main" child >/dev/null
test "$(branch_owner child)" = node-a
client control migrate --node node-b "$NAME@child" >/dev/null
test "$(branch_owner child)" = node-b
test "$(client control ls --json | jq --arg family "$NAME" \
	'[.[] | select(.name == $family) | .branches[]] | length')" -eq 2 ||
	fail "private migration branch leaked into inventory"

# The listener is created on the compute node the gateway selected. It must
# route to the remote owner and continue routing after ownership moves.
binding=127.0.0.1:$HOST_PORT:8080
client control port publish "$NAME@main" "$binding" >/dev/null
response=
for _ in $(seq 1 100); do
	response=$(curl -fsS --max-time 2 "http://127.0.0.1:$HOST_PORT/" 2>/dev/null || true)
	test "$response" = control-route && break
	sleep 0.05
done
test "$response" = control-route || fail "published port did not reach remote owner"
client control migrate --node node-a "$NAME@main" >/dev/null
test "$(branch_owner main)" = node-a
test "$(curl -fsS --max-time 5 "http://127.0.0.1:$HOST_PORT/")" = control-route ||
	fail "published port did not follow migration"
client control port unpublish "$NAME@main" "$binding" >/dev/null
if curl -fsS --max-time 1 "http://127.0.0.1:$HOST_PORT/" >/dev/null 2>&1; then
	fail "unpublished endpoint remained reachable"
fi

client control rm -f "$NAME" >/dev/null
client control daemon stop >/dev/null
wait "$PID_CONTROL"
PID_CONTROL=
client a node stop node-b --stop-all >/dev/null
wait "$PID_B"
PID_B=
client a node stop node-a --stop-all >/dev/null
wait "$PID_A"
PID_A=
SUCCESS=1
echo "TTC control-only gateway and transparent port-routing lifecycle passed"
