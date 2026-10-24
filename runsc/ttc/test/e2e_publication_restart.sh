#!/bin/sh

# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Regression coverage for lifecycle operations that race asynchronous cold
# publication. A fresh start or rehydration must fence the exact immutable
# publication before executing guest instructions; a late worker must never
# unplace or overwrite the newer runtime incarnation.
set -eu

TTC=${TTC:-ttc}
TTCD=${TTCD:-ttcd}
IMAGE=${TTC_PUBLICATION_TEST_IMAGE:-busybox:1.36}
NAME=${TTC_PUBLICATION_TEST_NAME:-ttc-publication-restart-$$}
ROOT=${TTC_PUBLICATION_TEST_ROOT:-$(mktemp -d "${TMPDIR:-/tmp}/ttc-publication-restart.XXXXXX")}
SHARED=$ROOT/shared
LOCAL=$ROOT/local
SOCKET=$LOCAL/ttcd.sock
PORT=${TTC_PUBLICATION_TEST_PORT:-29983}
COMMAND_TIMEOUT=${TTC_E2E_COMMAND_TIMEOUT:-120s}
KEEP_ROOT=${TTC_PUBLICATION_TEST_KEEP_ROOT:-}
PID=

client() { TTC_LOCAL_ROOT=$LOCAL timeout "$COMMAND_TIMEOUT" "$TTC" "$@"; }

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
	if test -S "$SOCKET"; then client rm -f "$NAME" >/dev/null 2>&1; fi
	if test -S "$SOCKET"; then client node stop node-a --stop-all >/dev/null 2>&1; fi
	stop_session "$PID"
	if test -z "$KEEP_ROOT" && test -z "${TTC_PUBLICATION_TEST_ROOT:-}"; then
		rm -rf "$ROOT"
	else
		echo "TTC publication/restart root retained at $ROOT" >&2
	fi
}
trap cleanup EXIT HUP INT TERM

branch_field() {
	field=$1
	client ls --json | jq -r --arg name "$NAME" --arg field "$field" \
		'.[] | select(.name == $name or .id == $name) | .branches[] | select(.id == "main") | .[$field]'
}

wait_state() {
	want=$1
	attempt=0
	while test "$attempt" -lt 600; do
		if test "$(branch_field state 2>/dev/null || true)" = "$want"; then return 0; fi
		attempt=$((attempt + 1))
		sleep 0.05
	done
	client inspect "$NAME" >&2 || true
	return 1
}

wait_active() {
	attempt=0
	while test "$attempt" -lt 600; do
		state=$(branch_field state 2>/dev/null || true)
		case "$state" in
		running) return 0 ;;
		paused) client resume "$NAME@main" >/dev/null; return 0 ;;
		esac
		attempt=$((attempt + 1))
		sleep 0.05
	done
	client inspect "$NAME" >&2 || true
	return 1
}

mkdir -p "$SHARED"
# A daemon's root holds its socket, and must be private.
mkdir -p -m 700 "$LOCAL"
TTC_ROOT=$SHARED TTC_LOCAL_ROOT=$LOCAL \
	setsid "$TTCD" --node node-a --listen "127.0.0.1:$PORT" \
	--advertise "127.0.0.1:$PORT" --unsafe-no-host-isolation \
	>"$LOCAL/ttcd.log" 2>&1 &
PID=$!
attempt=0
while test "$attempt" -lt 300; do
	if client node ls >/dev/null 2>&1; then break; fi
	attempt=$((attempt + 1))
	sleep 0.05
done
client node ls >/dev/null
client pull "$IMAGE" >/dev/null

# The file is deliberately large enough that publication remains pending on
# ordinary hosts while start immediately fences it.
client run -d --node node-a --name "$NAME" --shm-size 64m "$IMAGE" sh -c \
	'dd if=/dev/zero of=/cold-input bs=1M count=96 2>/dev/null; printf ROOT-MARK | dd of=/cold-input conv=notrunc 2>/dev/null; dd if=/dev/zero of=/dev/shm/live bs=1M count=32 2>/dev/null; printf MEM-MARK | dd of=/dev/shm/live conv=notrunc 2>/dev/null; exec sleep 3600' >/dev/null
attempt=0
while test "$attempt" -lt 600; do
	if test "$(client exec "$NAME@main" dd if=/cold-input bs=9 count=1 2>/dev/null || true)" = ROOT-MARK &&
		test "$(client exec "$NAME@main" dd if=/dev/shm/live bs=8 count=1 2>/dev/null || true)" = MEM-MARK; then
		break
	fi
	attempt=$((attempt + 1))
	sleep 0.05
done
test "$attempt" -lt 600

client stop "$NAME@main" >/dev/null
client start "$NAME@main" >/dev/null
wait_state running
attempt=0
while test "$attempt" -lt 600; do
	if test "$(client exec "$NAME@main" dd if=/cold-input bs=9 count=1 2>/dev/null || true)" = ROOT-MARK &&
		test "$(client exec "$NAME@main" dd if=/dev/shm/live bs=8 count=1 2>/dev/null || true)" = MEM-MARK; then
		break
	fi
	attempt=$((attempt + 1))
	sleep 0.05
done
test "$attempt" -lt 600
# Give any superseded worker enough time to expose an unfenced late commit.
sleep 2
wait_state running
client exec "$NAME@main" true

client dehydrate "$NAME@main" >/dev/null
client rehydrate "$NAME@main" >/dev/null
wait_active
test "$(client exec "$NAME@main" dd if=/cold-input bs=9 count=1 2>/dev/null)" = ROOT-MARK
test "$(client exec "$NAME@main" dd if=/dev/shm/live bs=8 count=1 2>/dev/null)" = MEM-MARK
sleep 2
wait_state running
client exec "$NAME@main" true

client rm -f "$NAME" >/dev/null
client node stop node-a --stop-all >/dev/null
wait "$PID"
PID=

echo "TTC start/rehydrate publication fencing passed"
