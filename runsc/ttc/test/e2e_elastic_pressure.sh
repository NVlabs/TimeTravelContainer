#!/bin/sh
# Copyright 2026 The gVisor Authors.
# Licensed under the Apache License, Version 2.0 (the "License");

set -eu

TTC=${TTC:-ttc}
TTCD=${TTCD:-ttcd}
IMAGE=${TTC_ELASTIC_PRESSURE_IMAGE:-busybox:1.36}
ROOT=${TTC_ELASTIC_PRESSURE_ROOT:-$(mktemp -d "${TMPDIR:-/tmp}/ttc-elastic-pressure.XXXXXX")}
SHARED=$ROOT/shared
TOKEN=ttc-elastic-pressure-$$
PORT_A=${TTC_ELASTIC_PRESSURE_PORT_A:-20383}
PORT_B=${TTC_ELASTIC_PRESSURE_PORT_B:-20393}
PORT_C=${TTC_ELASTIC_PRESSURE_PORT_C:-20403}
PID_A=
PID_B=
PID_C=
FAMILIES="pressure-a-$$ pressure-b-$$"

socket() { printf '%s/run-%s/ttcd.sock' "$ROOT" "$1"; }
client() { node=$1; shift; TTC_SOCKET=$(socket "$node") "$TTC" "$@"; }

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
	for family in $FAMILIES; do
		for node in a b c; do
			if test -S "$(socket "$node")"; then client "$node" rm -f "$family" >/dev/null 2>&1; fi
		done
	done
	for node in c b a; do
		if test -S "$(socket "$node")"; then client "$node" node stop "node-$node" --stop-all >/dev/null 2>&1; fi
	done
	stop_session "$PID_C"
	stop_session "$PID_B"
	stop_session "$PID_A"
	root_pattern="[/]${ROOT#/}"
	root_pids=$(pgrep -f "$root_pattern" 2>/dev/null || true)
	test -z "$root_pids" || kill -KILL $root_pids >/dev/null 2>&1 || true
	if test -z "${TTC_ELASTIC_PRESSURE_ROOT:-}"; then rm -rf "$ROOT"; else echo "TTC elastic pressure root retained at $ROOT" >&2; fi
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

wait_ready() {
	node=$1
	for attempt in $(seq 1 400); do
		if client "$node" node ls >/dev/null 2>&1; then return 0; fi
		sleep 0.05
	done
	return 1
}

start_node() {
	node=$1
	port=$2
	target=$3
	shift 3
	mkdir -p "$ROOT/local-$node" "$ROOT/run-$node"
	TTC_ROOT=$SHARED TTC_LOCAL_ROOT=$ROOT/local-$node TTC_CLUSTER_TOKEN=$TOKEN \
		setsid "$TTCD" --socket "$(socket "$node")" --node "node-$node" \
		--listen "127.0.0.1:$port" --advertise "127.0.0.1:$port" \
		--memory-target "$target" --cluster-lease 15s --unsafe-no-host-isolation "$@" \
		>"$ROOT/run-$node/ttcd.log" 2>&1 &
	eval "PID_$(printf '%s' "$node" | tr '[:lower:]' '[:upper:]')=$!"
	wait_ready "$node"
}

memory_current() {
	cgroup=$(awk -F: '$1 == 0 { print $3 }' /proc/self/cgroup)
	if test -r "/sys/fs/cgroup/${cgroup#/}/memory.current"; then
		cat "/sys/fs/cgroup/${cgroup#/}/memory.current"
	else
		awk -v page="$(getconf PAGESIZE)" '{ print $2 * page }' /proc/self/statm
	fi
}

baseline=$(memory_current)
# Same-host test daemons share the caller's cgroup. Give node-a enough startup
# headroom, then create 8 independently dirtied branches to cross its target.
source_target=$((baseline + 256 * 1024 * 1024))
# Planner estimates deliberately include conservative shared-memory exposure;
# leave ample accepting capacity so this test exercises placement rather than
# capacity provisioning.
peer_target=$((baseline + 64 * 1024 * 1024 * 1024))
mkdir -p "$ROOT" "$SHARED"
start_node a "$PORT_A" "$source_target" --cluster-init
start_node b "$PORT_B" "$peer_target" --cluster-join "127.0.0.1:$PORT_A"
start_node c "$PORT_C" "$peer_target" --cluster-join "127.0.0.1:$PORT_A"

client a pull "$IMAGE" >/dev/null
fill_pids=
for family in $FAMILIES; do
	client a run -d --node node-a --name "$family" "$IMAGE" sh -c \
		'while test ! -e /tmp/fill; do sleep 0.05; done; yes 0123456789abcdef | head -c 50331648 >/dev/shm/private && printf PRESSURE >/tmp/ready; sleep 3600' >/dev/null
	client a fork --node node-a "$family@main" b1 b2 b3 >/dev/null
	for branch in main b1 b2 b3; do
		client a exec "$family@$branch" touch /tmp/fill &
		fill_pids="$fill_pids $!"
	done
done
for pid in $fill_pids; do wait "$pid"; done

# The execs above only release the guest writers; wait until every branch has
# completed its private allocation before asking the planner for a snapshot.
for family in $FAMILIES; do
	for branch in main b1 b2 b3; do
		ready=
		for attempt in $(seq 1 240); do
			if test "$(client a exec "$family@$branch" cat /tmp/ready 2>/dev/null || true)" = PRESSURE; then
				ready=yes
				break
			fi
			sleep 0.25
		done
		test "$ready" = yes
	done
done

for attempt in $(seq 1 120); do
	state=$(client b node ls --json 2>/dev/null | jq -r '.[] | select(.node_id == "node-a") | .state' || true)
	test "$state" = pressured && break
	sleep 0.25
done
if test "$state" != pressured; then
	echo "node-a did not become pressured (state=${state:-missing})" >&2
	client b node ls --json >&2 || true
	exit 1
fi

client a node _planner-resources --_local node-a >"$ROOT/planner-node-a.json"
planner_bytes=$(wc -c <"$ROOT/planner-node-a.json")
if test "$planner_bytes" -gt $((512 * 1024)); then
	echo "planner snapshot expanded to $planner_bytes bytes for a 384 MiB fragmented CoW workload" >&2
	exit 1
fi
accounted=$(jq '[.branches[].total_bytes] | add // 0' "$ROOT/planner-node-a.json")
if test "$accounted" -lt $((384 * 1024 * 1024)); then
	echo "planner accounted only $accounted bytes for eight 48 MiB tmpfs files" >&2
	cat "$ROOT/planner-node-a.json" >&2
	exit 1
fi
if ! plan=$(client b node rebalance node-a); then
	echo "node-a planner snapshot:" >&2
	cat "$ROOT/planner-node-a.json" >&2
	exit 1
fi
test "$(printf '%s' "$plan" | jq '.moves | length')" -gt 0
# Validate every branch after the multi-destination planner has executed all
# moves. The unified view supplies the current physical owner.
for family in $FAMILIES; do
	for branch in main b1 b2 b3; do
		owner=$(client b ls --json | jq -r --arg f "$family" --arg b "$branch" '.[] | select(.name == $f) | .branches[] | select(.id == $b) | .node_id')
		case "$owner" in node-a) node=a ;; node-b) node=b ;; node-c) node=c ;; *) exit 1 ;; esac
		test "$(client "$node" exec "$family@$branch" cat /tmp/ready)" = PRESSURE
		test "$(client "$node" exec "$family@$branch" head -c 8 /dev/shm/private)" = 01234567
	done
done

echo "TTC high-fanout pressure rebalance passed with $(printf '%s' "$plan" | jq '.moves | length') migrations"
