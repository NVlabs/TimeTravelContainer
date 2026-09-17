#!/bin/sh
# Copyright 2026 The gVisor Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     https://www.apache.org/licenses/LICENSE-2.0

# Qualifies sequential recovery of a fully stopped multi-voter cluster. The
# first persistent member must remain alive in an explicit quorum-wait state so
# an operator can start the second member from the same shell.
set -eu

TTC=${TTC:-ttc}
IMAGE=${TTC_CLUSTER_RESTART_IMAGE:-busybox:1.36}
NAME=${TTC_CLUSTER_RESTART_NAME:-ttc-cluster-restart-$$}
ROOT=${TTC_CLUSTER_RESTART_ROOT:-$(mktemp -d "${TMPDIR:-/tmp}/ttc-cluster-restart.XXXXXX")}
PORT_A=${TTC_CLUSTER_RESTART_PORT_A:-23583}
PORT_B=${TTC_CLUSTER_RESTART_PORT_B:-23593}
TOKEN=ttc-cluster-restart-$NAME
KEEP_ROOT=${TTC_CLUSTER_RESTART_KEEP_ROOT:-}

socket() { printf '%s/run-%s/ttcd.sock' "$ROOT" "$1"; }
client() { node=$1; shift; TTC_SOCKET=$(socket "$node") timeout 120s "$TTC" "$@"; }

start_node() {
	node=$1
	port=$2
	shift 2
	TTC_ROOT=$ROOT/shared TTC_LOCAL_ROOT=$ROOT/local-$node TTC_CLUSTER_TOKEN=$TOKEN \
		TTC_SOCKET=$(socket "$node") timeout 60s "$TTC" daemon start \
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
	# Neither start waits for full admission. Both processes remain fenced until
	# their persistent Raft group elects a leader and commits registration.
	start_a=$(start_node a "$PORT_A" --cluster-init)
	printf '%s\n' "$start_a" | grep -q 'waiting for cluster quorum'
	client a daemon status | jq -e '.state == "starting" and .waiting_for_cluster_quorum == true' >/dev/null
	start_b=$(start_node b "$PORT_B" --cluster-join "127.0.0.1:$PORT_A")
	printf '%s\n' "$start_b" | grep -q 'waiting for cluster quorum'
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

mkdir -p "$ROOT/shared" "$ROOT/local-a" "$ROOT/local-b" "$ROOT/run-a" "$ROOT/run-b"
chmod 700 "$ROOT/run-a" "$ROOT/run-b"
start_node a "$PORT_A" --cluster-init >/dev/null
start_node b "$PORT_B" --cluster-join "127.0.0.1:$PORT_A" >/dev/null

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
# Deliberately leave the remote tail unfaulted. Its first access happens only
# after two control-daemon crash/recovery cycles. The first cycle interrupts
# the live source and makes recovery establish immutable graph backing.
before=$(client a exec "$NAME@main" cat /counter)
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

# A second daemon crash must not kill the now-authoritative lazy relay.
pid_a=$(client a daemon status | jq -r .pid)
pid_b=$(client b daemon status | jq -r .pid)
stop_pid "$pid_a"
stop_pid "$pid_b"
pgrep -f "[/]ttcd _shared-memory-relay $ROOT/shared/snapshot-packs" >/dev/null
restart_cluster

test "$(client a exec "$NAME@main" sh -c 'dd if=/dev/shm/resident bs=12 count=1 2>/dev/null')" = RESTART-MARK
test "$(client b exec "$NAME@remote" sh -c 'dd if=/dev/shm/resident bs=1M skip=31 count=1 2>/dev/null | dd bs=9 count=1 2>/dev/null')" = TAIL-MARK
after=$(client a exec "$NAME@main" cat /counter)
test "$after" -ge "$before"

client a rm -f "$NAME" >/dev/null
client b node stop node-b --stop-all >/dev/null
test "$(client a node ls --json | jq 'length')" -eq 1
client a node stop node-a --stop-all >/dev/null

echo "TTC sequential whole-cluster restart passed (counter $before -> $after)"
