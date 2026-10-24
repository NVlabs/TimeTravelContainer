#!/bin/sh

# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -eu

TTC=${TTC:-ttc}
TTCD=${TTCD:-ttcd}
IMAGE=${TTC_ELASTIC_PRESSURE_IMAGE:-busybox:1.36}
ROOT=${TTC_ELASTIC_PRESSURE_ROOT:-$(mktemp -d "${TMPDIR:-/tmp}/ttc-elastic-pressure.XXXXXX")}
SHARED=${TTC_ELASTIC_PRESSURE_SHARED_ROOT:-$ROOT/shared}
COMMAND_TIMEOUT=${TTC_E2E_COMMAND_TIMEOUT:-120s}
PORT_A=${TTC_ELASTIC_PRESSURE_PORT_A:-20383}
PORT_B=${TTC_ELASTIC_PRESSURE_PORT_B:-20393}
PORT_C=${TTC_ELASTIC_PRESSURE_PORT_C:-20403}
PID_A=
PID_B=
PID_C=
CLEANED=
FAMILIES="pressure-a-$$ pressure-b-$$"
SOURCE_TARGET=${TTC_ELASTIC_PRESSURE_SOURCE_TARGET:-512MiB}
BRANCH_BYTES=${TTC_ELASTIC_PRESSURE_BRANCH_BYTES:-67108864}

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
	if test -z "$CLEANED"; then
		for family in $FAMILIES; do
			for node in a b c; do
				if test -S "$(socket "$node")"; then client "$node" rm -f "$family" >/dev/null 2>&1 && break; fi
			done
		done
		for node in c b a; do
			if test -S "$(socket "$node")"; then client "$node" node stop "node-$node" --stop-all >/dev/null 2>&1; fi
		done
	fi
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
	# A daemon's root holds its socket, and must be private.
	mkdir -p -m 700 "$ROOT/local-$node"
	TTC_ROOT=$SHARED TTC_LOCAL_ROOT=$ROOT/local-$node \
		setsid "$TTCD" --node "node-$node" \
		--listen "127.0.0.1:$port" --advertise "127.0.0.1:$port" \
		--memory-target "$target" --cluster-lease 15s --unsafe-no-host-isolation "$@" \
		>"$ROOT/local-$node/ttcd.log" 2>&1 &
	eval "PID_$(printf '%s' "$node" | tr '[:lower:]' '[:upper:]')=$!"
	wait_ready "$node"
}

# Node accounting deliberately scans only the daemon and processes belonging to
# its local root. Do not derive this threshold from the caller's cgroup: build
# workers and unrelated jobs can disappear after this test starts and make the
# supposedly pressured node fall below a stale, inflated target. Eight branches
# independently dirty BRANCH_BYTES below, as much as this fixed node-local
# target, which the node's daemon and runtimes cross.
# Planner estimates include conservative shared-memory exposure, so peers keep
# ample accepting capacity to exercise placement rather than provisioning.
source_target=$SOURCE_TARGET
peer_target=64GiB
mkdir -p "$ROOT" "$SHARED"
start_node a "$PORT_A" "$source_target"
start_node b "$PORT_B" "$peer_target"
start_node c "$PORT_C" "$peer_target"

client a pull "$IMAGE" >/dev/null
for family in $FAMILIES; do
	client a run -d --node node-a --name "$family" "$IMAGE" sh -c \
		"while test ! -e /tmp/fill; do sleep 0.05; done; yes 0123456789abcdef | head -c $BRANCH_BYTES >/dev/shm/private && printf PRESSURE >/tmp/ready; sleep 3600" >/dev/null
	client a fork --node node-a "$family@main" b1 b2 b3 >/dev/null
done

# Admit the complete test topology before dirtying memory. Admission and
# pressure transitions are concurrent in production, but the property under
# test here is multi-destination rebalance after a known accepted workload.
# A separate lifecycle regression verifies that a late admission failure never
# leaves an executable branch outside Raft placement authority.
fill_pids=
for family in $FAMILIES; do
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
expected_accounted=$((8 * BRANCH_BYTES))
if test "$accounted" -lt "$expected_accounted"; then
	echo "planner accounted only $accounted bytes for $expected_accounted bytes of private tmpfs data" >&2
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

# Cleanup is part of the regression, not merely a best-effort EXIT trap. A
# high-fanout migration used to leave retired physical branches and hundreds
# of MiB of authoritative memory chunks after every visible family was gone.
for family in $FAMILIES; do
	client a rm -f "$family" >/dev/null
done
# A node keeps the chunks of origins it does not hold as its own as a cache,
# until it finds them retired, which it asks again a minute after it last
# found them alive: the running nodes release the families' chunks within two
# minutes.
for attempt in $(seq 1 240); do
	find "$ROOT" -path '*/objects/memory-chunks/*' -type f -print -quit | grep -q . || break
	sleep 0.5
done
for node in c b a; do
	client "$node" node stop "node-$node" --stop-all >/dev/null
done
wait "$PID_C"
wait "$PID_B"
wait "$PID_A"
PID_A=
PID_B=
PID_C=
CLEANED=yes
if find "$ROOT" -path '*/objects/memory-chunks/*' -type f -print -quit | grep -q .; then
	echo "quiescent cluster retained authoritative local memory chunks" >&2
	exit 1
fi
if find "$SHARED/snapshot-packs/snapshots" -type f -print -quit 2>/dev/null | grep -q .; then
	echo "quiescent cluster retained unreachable shared snapshot packs" >&2
	exit 1
fi
