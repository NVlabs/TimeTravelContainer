#!/bin/sh

# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -eu

TTC=${TTC:-ttc}
TTCD=${TTCD:-ttcd}
IMAGE=${TTC_SHARED_GC_TEST_IMAGE:-busybox:1.36}
NAME=${TTC_SHARED_GC_TEST_NAME:-ttc-shared-gc-turnover-$$}
ROOT=${TTC_SHARED_GC_TEST_ROOT:-$(mktemp -d "${TMPDIR:-/tmp}/ttc-shared-gc-turnover.XXXXXX")}
SHARED=${TTC_SHARED_GC_TEST_SHARED_ROOT:-$ROOT/shared}
PORT_A=${TTC_SHARED_GC_TEST_PORT_A:-20183}
PORT_B=${TTC_SHARED_GC_TEST_PORT_B:-20193}
PORT_C=${TTC_SHARED_GC_TEST_PORT_C:-20203}
KEEP_ROOT=${TTC_SHARED_GC_TEST_KEEP_ROOT:-}
COMMAND_TIMEOUT=${TTC_E2E_COMMAND_TIMEOUT:-120s}
CLUSTER_LEASE=${TTC_SHARED_GC_TEST_CLUSTER_LEASE:-30s}
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
	if test -z "$KEEP_ROOT"; then
		for node in a b c; do
			if test -S "$(socket "$node")"; then client "$node" rm -f "$NAME" >/dev/null 2>&1; fi
		done
	fi
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
	if test -z "$KEEP_ROOT" && test -z "${TTC_SHARED_GC_TEST_ROOT:-}"; then
		rm -rf "$ROOT"
	else
		echo "TTC shared-GC turnover root retained at $ROOT" >&2
	fi
}
trap cleanup EXIT HUP INT TERM

wait_ready() {
	node=$1
	attempt=0
	while test "$attempt" -lt 400; do
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
		--memory-target 64GiB --cluster-lease "$CLUSTER_LEASE" --unsafe-no-host-isolation "$@" \
		>"$ROOT/local-$node/ttcd.log" 2>&1 &
	eval "PID_$(printf '%s' "$node" | tr '[:lower:]' '[:upper:]')=$!"
	wait_ready "$node"
}

mkdir -p "$ROOT" "$SHARED"
start_node a "$PORT_A"
start_node b "$PORT_B"
start_node c "$PORT_C"

# Find the actual coordinator instead of assuming the first node won its
# independent lease-acquisition race. A daemon logs when it starts
# coordinating, once it has rebuilt its ledger.
coordinator=
attempt=0
while test "$attempt" -lt 300 && test -z "$coordinator"; do
	for node in a b c; do
		if grep -q 'coordinating shared garbage collection' "$ROOT/local-$node/ttcd.log" 2>/dev/null; then
			coordinator=$node
			break
		fi
	done
	attempt=$((attempt + 1))
	test -n "$coordinator" || sleep 0.05
done
test -n "$coordinator"

case "$coordinator" in
	a) source=b; target=c ;;
	b) source=a; target=c ;;
	c) source=a; target=b ;;
esac
client "$source" pull "$IMAGE" >/dev/null
client "$source" run -d --node "node-$source" --name "$NAME" --shm-size 24m "$IMAGE" sh -c \
	'dd if=/dev/zero of=/dev/shm/resident bs=1M count=16 2>/dev/null; printf GC-TURNOVER | dd of=/dev/shm/resident conv=notrunc 2>/dev/null; exec sleep 3600' >/dev/null
client "$source" dehydrate "$NAME@main" >/dev/null
client "$source" wait --condition durable "$NAME@main" >/dev/null

# Abruptly lose the coordinator, then immediately perform an operation whose
# shared snapshot retain requires its successor. TTC must wait/retry across
# the fenced lease expiry and ledger rebuild without exposing a dynamic fork
# rejection.
if test -z "${TTC_SHARED_GC_TEST_SKIP_CRASH:-}"; then
	eval "coordinator_pid=\$PID_$(printf '%s' "$coordinator" | tr '[:lower:]' '[:upper:]')"
	stop_session "$coordinator_pid"
	eval "PID_$(printf '%s' "$coordinator" | tr '[:lower:]' '[:upper:]')="
fi
started=$(date +%s)
client "$source" fork --node "node-$target" "$NAME@main" child >/dev/null
elapsed=$(( $(date +%s) - started ))
test "$elapsed" -lt 45
test "$(client "$target" exec "$NAME@child" sh -c 'dd if=/dev/shm/resident bs=11 count=1 2>/dev/null')" = GC-TURNOVER

# Publish a second-generation branch whose filesystem graph is lazily backed
# by the first shared origin, then fork it back across the cluster. This covers
# publication-time path compression and cross-origin reference rebuilding
# after the original shared-GC coordinator has disappeared.
client "$target" exec "$NAME@child" sh -c 'printf SECOND-GENERATION >/tmp/shared-graph-child'
client "$target" dehydrate "$NAME@child" >/dev/null
client "$target" wait --condition durable "$NAME@child" >/dev/null
client "$target" fork --node "node-$source" "$NAME@child" grandchild >/dev/null
test "$(client "$source" exec "$NAME@grandchild" cat /tmp/shared-graph-child)" = SECOND-GENERATION
test "$(client "$source" exec "$NAME@grandchild" sh -c 'dd if=/dev/shm/resident bs=11 count=1 2>/dev/null')" = GC-TURNOVER

client "$target" rm -f "$NAME" >/dev/null

# Stop both survivors through the protocol rather than relying on the trap's
# best-effort process reaping. The final daemon must wait for the crashed
# coordinator's lease to be authoritatively fenced, assume collection
# ownership, and leave neither graph origins nor cold publication residue.
client "$target" node stop "node-$target" --stop-all >/dev/null
eval "target_pid=\$PID_$(printf '%s' "$target" | tr '[:lower:]' '[:upper:]')"
wait "$target_pid"
eval "PID_$(printf '%s' "$target" | tr '[:lower:]' '[:upper:]')="
client "$source" node stop "node-$source" --stop-all >/dev/null
eval "source_pid=\$PID_$(printf '%s' "$source" | tr '[:lower:]' '[:upper:]')"
wait "$source_pid"
eval "PID_$(printf '%s' "$source" | tr '[:lower:]' '[:upper:]')="
test "$(find "$SHARED/snapshot-packs/snapshots" -mindepth 1 -maxdepth 1 -type d 2>/dev/null | wc -l)" -eq 0
test "$(find "$SHARED/cold/manifests" -type f 2>/dev/null | wc -l)" -eq 0
test ! -e "$SHARED/cold/packs"
test "$(find "$SHARED/containers" -mindepth 1 -maxdepth 1 -type d 2>/dev/null | wc -l)" -eq 0
test "$(find "$SHARED/removed-containers" -mindepth 1 2>/dev/null | wc -l)" -eq 0
echo "TTC shared-GC coordinator crash and retain retry passed in ${elapsed}s"
