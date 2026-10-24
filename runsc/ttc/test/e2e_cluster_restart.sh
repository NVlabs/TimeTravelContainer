#!/bin/sh

# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Qualifies recovery of a whole cluster whose daemons crashed together. Every
# node starts alike: both restart at once, as the jobs of a batch system would.
set -eu

TTC=${TTC:-ttc}
IMAGE=${TTC_CLUSTER_RESTART_IMAGE:-busybox:1.36}
NAME=${TTC_CLUSTER_RESTART_NAME:-ttc-cluster-restart-$$}
ROOT=${TTC_CLUSTER_RESTART_ROOT:-$(mktemp -d "${TMPDIR:-/tmp}/ttc-cluster-restart.XXXXXX")}
PORT_A=${TTC_CLUSTER_RESTART_PORT_A:-23583}
PORT_B=${TTC_CLUSTER_RESTART_PORT_B:-23593}
KEEP_ROOT=${TTC_CLUSTER_RESTART_KEEP_ROOT:-}

socket() { printf '%s/local-%s/ttcd.sock' "$ROOT" "$1"; }
client() { node=$1; shift; TTC_LOCAL_ROOT=$ROOT/local-$node timeout 120s "$TTC" "$@"; }
read_counter() {
	client a exec "$NAME@main" sh -c \
		'while :; do value=$(cat /counter); case "$value" in ""|*[!0-9]*) sleep 0.01 ;; *) printf "%s\n" "$value"; break ;; esac; done'
}

start_node() {
	node=$1
	port=$2
	shift 2
	TTC_ROOT=$ROOT/shared TTC_LOCAL_ROOT=$ROOT/local-$node \
		timeout 60s "$TTC" daemon start \
			--node "node-$node" --listen "127.0.0.1:$port" \
			--advertise "127.0.0.1:$port" --memory-target 8GiB \
			--unsafe-no-host-isolation "$@"
}

stop_pid() {
	pid=$1
	test -n "$pid" || return 0
	kill -KILL "$pid" >/dev/null 2>&1 || true
	attempt=0
	while kill -0 "$pid" >/dev/null 2>&1 && test "$attempt" -lt 200; do
		attempt=$((attempt + 1))
		sleep 0.02
	done
}

wait_ready() {
	attempt=0
	while test "$attempt" -lt 600; do
		if client a node ls --json 2>/dev/null | jq -e \
			'length == 2 and all(.[]; .state == "ready" or .state == "pressured")' >/dev/null; then
			return 0
		fi
		attempt=$((attempt + 1))
		sleep 0.1
	done
	client a node ls --json | jq -e \
		'length == 2 and all(.[]; .state == "ready" or .state == "pressured")' >/dev/null
}

restart_cluster() {
	# A restarting daemon admits itself only once it reaches every node whose
	# lease is still active, the other crashed one among them: start both at
	# once. One takes the authority over, once the crashed holder's counter in
	# the shared root has stood still.
	start_node a "$PORT_A" >"$ROOT/start-a.out" 2>&1 &
	start_a=$!
	start_node b "$PORT_B" >"$ROOT/start-b.out" 2>&1 &
	start_b=$!
	status=0
	wait "$start_a" || status=$?
	wait "$start_b" || status=$?
	if test "$status" -ne 0; then
		cat "$ROOT/start-a.out" "$ROOT/start-b.out" >&2
		return "$status"
	fi
	wait_ready
}

cleanup() {
	set +e
	if test -S "$(socket a)"; then client a rm -f "$NAME" >/dev/null 2>&1; fi
	if test -S "$(socket b)"; then client b node stop node-b --stop-all >/dev/null 2>&1; fi
	if test -S "$(socket a)"; then client a node stop node-a --stop-all >/dev/null 2>&1; fi
	for pid in $(pgrep -f "[/]${ROOT#/}" 2>/dev/null || true); do
		executable=$(readlink "/proc/$pid/exe" 2>/dev/null || true)
		case "${executable##*/}" in ttcd|runsc|runsc-gofer) kill -KILL "$pid" >/dev/null 2>&1 || true ;; esac
	done
	if test -z "$KEEP_ROOT" && test -z "${TTC_CLUSTER_RESTART_ROOT:-}"; then
		rm -rf "$ROOT"
	else
		echo "TTC cluster-restart root retained at $ROOT" >&2
	fi
}
trap cleanup EXIT HUP INT TERM

mkdir -p "$ROOT/shared"
# A daemon's root holds its socket, and must be private.
mkdir -p -m 700 "$ROOT/local-a" "$ROOT/local-b"
start_node a "$PORT_A" >/dev/null
start_node b "$PORT_B" >/dev/null

client a pull "$IMAGE" >/dev/null
client a run -d --node node-a --name "$NAME" --shm-size 64m "$IMAGE" sh -c \
	'dd if=/dev/zero of=/dev/shm/resident bs=1M count=32 2>/dev/null; printf RESTART-MARK | dd of=/dev/shm/resident conv=notrunc 2>/dev/null; printf TAIL-MARK | dd of=/dev/shm/resident bs=1M seek=31 conv=notrunc 2>/dev/null; n=0; while :; do n=$((n+1)); echo "$n" >/counter; sleep 0.05; done' >/dev/null
attempt=0
while test "$attempt" -lt 300; do
	if test "$(client a exec "$NAME@main" sh -c 'dd if=/dev/shm/resident bs=12 count=1 2>/dev/null' 2>/dev/null || true)" = RESTART-MARK; then break; fi
	attempt=$((attempt + 1))
	sleep 0.05
done
test "$(client a exec "$NAME@main" sh -c 'dd if=/dev/shm/resident bs=12 count=1 2>/dev/null')" = RESTART-MARK
client a fork --node node-b "$NAME@main" remote >/dev/null
# Deliberately leave the target tail unfaulted and crash promptly, before its
# background transfer can make the target independent. Recovery must establish
# immutable graph backing and a daemon-independent relay.
before=$(read_counter)
pid_a=$(client a daemon status | jq -r .pid)
pid_b=$(client b daemon status | jq -r .pid)
stop_pid "$pid_a"
stop_pid "$pid_b"
restart_cluster
attempt=0
while test "$attempt" -lt 600; do
	if pgrep -f "[/]ttcd _shared-memory-relay $ROOT/shared/snapshot-packs" >/dev/null; then break; fi
	attempt=$((attempt + 1))
	sleep 0.1
done
pgrep -f "[/]ttcd _shared-memory-relay $ROOT/shared/snapshot-packs" >/dev/null

# A branch name is a cluster-wide family identity even when its physical
# runtime is on another node. Reject the conflict before creating a throwaway
# local CoW branch.
if client a fork --node node-a "$NAME@main" remote >"$ROOT/duplicate.out" 2>"$ROOT/duplicate.err"; then
	echo "duplicate distributed fork unexpectedly succeeded" >&2
	exit 1
fi
grep -q 'branch "remote" already exists' "$ROOT/duplicate.err"

# Create a second remote import solely to exercise a locally forked descendant
# whose intermediate parent is removed. Keep "remote" intact so the second
# restart below still covers the first branch's lazy cross-node relay.
client a fork --node node-b "$NAME@main" intermediate >/dev/null
client b fork --node node-b "$NAME@intermediate" child >/dev/null
if ! client a rm -f "$NAME@intermediate" >/dev/null; then
	client a ls --json >&2 || true
	exit 1
fi
test "$(client a ls --json | jq --arg family "$NAME" \
	'[.[] | select(.name == $family) | .branches[]] | length')" -eq 3

# A second daemon crash must not kill the now-authoritative lazy relay.
pid_a=$(client a daemon status | jq -r .pid)
pid_b=$(client b daemon status | jq -r .pid)
stop_pid "$pid_a"
stop_pid "$pid_b"
pgrep -f "[/]ttcd _shared-memory-relay $ROOT/shared/snapshot-packs" >/dev/null
restart_cluster

test "$(client a exec "$NAME@main" sh -c 'dd if=/dev/shm/resident bs=12 count=1 2>/dev/null')" = RESTART-MARK
test "$(client b exec "$NAME@remote" sh -c 'dd if=/dev/shm/resident bs=1M skip=31 count=1 2>/dev/null | dd bs=9 count=1 2>/dev/null')" = TAIL-MARK
test "$(client b exec "$NAME@child" sh -c 'dd if=/dev/shm/resident bs=1M skip=31 count=1 2>/dev/null | dd bs=9 count=1 2>/dev/null')" = TAIL-MARK
after=$(read_counter)
test "$after" -ge "$before"

client a rm -f "$NAME" >/dev/null
client b node stop node-b --stop-all >/dev/null
test "$(client a node ls --json | jq 'length')" -eq 1
client a node stop node-a --stop-all >/dev/null

echo "TTC sequential whole-cluster restart passed (counter $before -> $after)"
