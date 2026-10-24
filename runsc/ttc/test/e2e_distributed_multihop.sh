#!/bin/sh

# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -eu

TTC=${TTC:-ttc}
TTCD=${TTCD:-ttcd}
IMAGE=${TTC_DISTRIBUTED_TEST_IMAGE:-busybox:1.36}
NAME=${TTC_DISTRIBUTED_TEST_NAME:-ttc-distributed-multihop-$$}
PORT_A=${TTC_DISTRIBUTED_TEST_PORT_A:-17683}
PORT_B=${TTC_DISTRIBUTED_TEST_PORT_B:-17693}
KEEP_ROOT=${TTC_DISTRIBUTED_TEST_KEEP_ROOT:-}
COMMAND_TIMEOUT=${TTC_E2E_COMMAND_TIMEOUT:-120s}

if test -n "${TTC_DISTRIBUTED_TEST_ROOT:-}"; then
	ROOT=$TTC_DISTRIBUTED_TEST_ROOT
	mkdir -p "$ROOT"
else
	ROOT=$(mktemp -d "${TMPDIR:-/tmp}/ttc-distributed-multihop.XXXXXX")
fi
SHARED=${TTC_DISTRIBUTED_TEST_SHARED_ROOT:-$ROOT/shared}
LOCAL_A=${TTC_DISTRIBUTED_TEST_LOCAL_A_ROOT:-$ROOT/local-a}
LOCAL_B=${TTC_DISTRIBUTED_TEST_LOCAL_B_ROOT:-$ROOT/local-b}
SOCKET_A=$LOCAL_A/ttcd.sock
SOCKET_B=$LOCAL_B/ttcd.sock
PID_A=
PID_B=

client_a() {
	TTC_LOCAL_ROOT=$LOCAL_A timeout "$COMMAND_TIMEOUT" "$TTC" "$@"
}

client_b() {
	TTC_LOCAL_ROOT=$LOCAL_B timeout "$COMMAND_TIMEOUT" "$TTC" "$@"
}

cleanup() {
	set +e
	if test -S "$SOCKET_A"; then
		TTC_LOCAL_ROOT=$LOCAL_A timeout 30s "$TTC" rm -f "$NAME" >/dev/null 2>&1
	fi
	if test -S "$SOCKET_B"; then
		TTC_LOCAL_ROOT=$LOCAL_B timeout 30s "$TTC" node stop node-b --stop-all >/dev/null 2>&1
	fi
	if test -S "$SOCKET_A"; then
		TTC_LOCAL_ROOT=$LOCAL_A timeout 30s "$TTC" node stop node-a --stop-all >/dev/null 2>&1
	fi
	if test -n "$PID_B"; then
		kill "$PID_B" >/dev/null 2>&1
		wait "$PID_B" >/dev/null 2>&1
	fi
	if test -n "$PID_A"; then
		kill "$PID_A" >/dev/null 2>&1
		wait "$PID_A" >/dev/null 2>&1
	fi
	if test -z "$KEEP_ROOT" && test -z "${TTC_DISTRIBUTED_TEST_ROOT:-}"; then
		rm -rf "$ROOT"
	else
		echo "TTC distributed test root retained at $ROOT" >&2
	fi
}
trap cleanup EXIT HUP INT TERM

wait_ready() {
	root=$1
	log=$2
	attempt=0
	while test "$attempt" -lt 600; do
		if TTC_LOCAL_ROOT=$root timeout 2s "$TTC" node ls >/dev/null 2>&1; then
			return 0
		fi
		attempt=$((attempt + 1))
		sleep 0.05
	done
	echo "TTC daemon of $root did not become ready" >&2
	tail -100 "$log" >&2 || true
	return 1
}

mkdir -p "$SHARED"
# A daemon's root holds its socket, and must be private.
mkdir -p -m 700 "$LOCAL_A" "$LOCAL_B"
TTC_ROOT=$SHARED TTC_LOCAL_ROOT=$LOCAL_A \
	"$TTCD" --node node-a \
	--listen "127.0.0.1:$PORT_A" --advertise "127.0.0.1:$PORT_A" \
	--memory-target 64GiB --unsafe-no-host-isolation >"$LOCAL_A/ttcd.log" 2>&1 &
PID_A=$!
wait_ready "$LOCAL_A" "$LOCAL_A/ttcd.log"

TTC_ROOT=$SHARED TTC_LOCAL_ROOT=$LOCAL_B \
	"$TTCD" --node node-b \
	--listen "127.0.0.1:$PORT_B" --advertise "127.0.0.1:$PORT_B" \
	--memory-target 64GiB --unsafe-no-host-isolation >"$LOCAL_B/ttcd.log" 2>&1 &
PID_B=$!
wait_ready "$LOCAL_B" "$LOCAL_B/ttcd.log"

client_a pull "$IMAGE" >/dev/null
client_a run -d --node node-a --name "$NAME" --shm-size 64m "$IMAGE" sh -c \
	'dd if=/dev/zero of=/dev/shm/resident bs=1M count=32 2>/dev/null; printf MEMORY-MARK | dd of=/dev/shm/resident conv=notrunc 2>/dev/null; while :; do date +%s%N > /state; sleep 0.02; done' >/dev/null

client_a fork --node node-b "$NAME@main" remote1 >/dev/null
client_b exec "$NAME@remote1" sh -c "printf REMOTE-1 >/identity"
test "$(client_b exec "$NAME@remote1" cat /identity)" = REMOTE-1
test "$(client_b exec "$NAME@remote1" sh -c 'dd if=/dev/shm/resident bs=11 count=1 2>/dev/null')" = MEMORY-MARK

client_b rename "$NAME@remote1" renamed
# Renaming releases the old visible name immediately. Reusing it must create a
# distinct branch, and transaction-private physical branches must never appear
# in cluster inventory.
client_a fork --node node-b "$NAME@main" remote1 >/dev/null
client_b exec "$NAME@remote1" sh -c "printf REUSED-NAME >/identity"
test "$(client_b exec "$NAME@renamed" cat /identity)" = REMOTE-1
test "$(client_b exec "$NAME@remote1" cat /identity)" = REUSED-NAME
visible_branches=$(client_a ls --json | jq -r --arg name "$NAME" '.[] | select(.name == $name) | .branches[].id' | sort)
test "$visible_branches" = "$(printf '%s\n' main remote1 renamed | sort)"
if printf '%s\n' "$visible_branches" | grep -q '^fork-'; then
	echo "private distributed branch leaked through ls: $visible_branches" >&2
	exit 1
fi

client_b migrate --node node-a "$NAME@renamed" >/dev/null
test "$(client_a exec "$NAME@renamed" cat /identity)" = REMOTE-1
test "$(client_a exec "$NAME@renamed" sh -c 'dd if=/dev/shm/resident bs=11 count=1 2>/dev/null')" = MEMORY-MARK
test "$(client_a exec "$NAME@renamed" sh -c 'wc -c </dev/shm/resident')" = 33554432

client_a fork --node node-b "$NAME@renamed" remote2 remote3 >/dev/null
test "$(client_b exec "$NAME@remote2" cat /identity)" = REMOTE-1
test "$(client_b exec "$NAME@remote2" sh -c 'dd if=/dev/shm/resident bs=11 count=1 2>/dev/null')" = MEMORY-MARK
test "$(client_b exec "$NAME@remote2" sh -c 'wc -c </dev/shm/resident')" = 33554432
test "$(client_b exec "$NAME@remote3" cat /identity)" = REMOTE-1
test "$(client_b exec "$NAME@remote3" sh -c 'dd if=/dev/shm/resident bs=11 count=1 2>/dev/null')" = MEMORY-MARK
client_b exec "$NAME@remote2" sh -c "printf REMOTE-2 >/identity"
test "$(client_a exec "$NAME@renamed" cat /identity)" = REMOTE-1
test "$(client_b exec "$NAME@remote2" cat /identity)" = REMOTE-2
test "$(client_b exec "$NAME@remote3" cat /identity)" = REMOTE-1
test "$(client_a ls --json | jq -r --arg name "$NAME" '.[] | select(.name == $name) | .branches[] | select(.id == "remote2") | .parent')" = renamed

# Live handoff descriptors are carried directly over the cluster protocol.
# Creating a branch or migrating it must not synchronously stage a legacy
# filesystem bootstrap object on the shared filesystem.
if test -d "$SHARED/cold/inflight" && find "$SHARED/cold/inflight" -type f -print -quit | grep -q .; then
	echo "live handoff created a shared cold-pack intent" >&2
	find "$SHARED/cold/inflight" -type f -print >&2
	exit 1
fi
if test -e "$SHARED/cold/packs"; then
	echo "live handoff recreated the removed legacy cold-pack path" >&2
	find "$SHARED/cold/packs" -print >&2
	exit 1
fi

# Optional maintenance settling is useful for stress jobs that need to observe
# publication, handoff, compaction, and GC after execution has gone idle.
if test -n "${TTC_DISTRIBUTED_TEST_SETTLE:-}"; then
	sleep "$TTC_DISTRIBUTED_TEST_SETTLE"
fi

# Family deletion is part of the distributed lifecycle being tested. Keep it
# on the asserted path rather than relying on the best-effort EXIT trap: a
# branch-local repository cleanup failure must not leave Sentries, placements,
# or shared family metadata behind while this test still reports success.
client_a rm -f "$NAME" >/dev/null
if client_a ps -a --format '{{.Container}}' | grep -Fx "$NAME" >/dev/null; then
	echo "distributed family $NAME remained visible after rm" >&2
	exit 1
fi
client_b node stop node-b --stop-all >/dev/null
wait "$PID_B"
PID_B=
client_a node stop node-a --stop-all >/dev/null
wait "$PID_A"
PID_A=

echo "TTC A-to-B-to-A-to-B sparse migration/fork lifecycle passed"
