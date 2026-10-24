#!/bin/sh

# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Exercises background remote-filesystem materialization while the migrated
# guest keeps an inherited database open and writable.
set -eu

TTC=${TTC:-ttc}
TTCD=${TTCD:-ttcd}
IMAGE=${TTC_DISTRIBUTED_ACTIVE_FS_IMAGE:-python:3.12-slim}
NAME=${TTC_DISTRIBUTED_ACTIVE_FS_NAME:-ttc-distributed-active-fs-$$}
PORT_A=${TTC_DISTRIBUTED_ACTIVE_FS_PORT_A:-17783}
PORT_B=${TTC_DISTRIBUTED_ACTIVE_FS_PORT_B:-17793}
COMMAND_TIMEOUT=${TTC_E2E_COMMAND_TIMEOUT:-180s}
DAEMON_EXTRA=${TTC_DISTRIBUTED_ACTIVE_FS_DAEMON_ARGS---unsafe-no-host-isolation}
ROOT=${TTC_DISTRIBUTED_ACTIVE_FS_ROOT:-$(mktemp -d "${TMPDIR:-/tmp}/ttc-distributed-active-fs.XXXXXX")}
SHARED=${TTC_DISTRIBUTED_ACTIVE_FS_SHARED_ROOT:-$ROOT/shared}
LOCAL_A=$ROOT/local-a
LOCAL_B=$ROOT/local-b
SOCKET_A=$LOCAL_A/ttcd.sock
SOCKET_B=$LOCAL_B/ttcd.sock
PID_A=
PID_B=

client_a() { TTC_LOCAL_ROOT=$LOCAL_A timeout "$COMMAND_TIMEOUT" "$TTC" "$@"; }
client_b() { TTC_LOCAL_ROOT=$LOCAL_B timeout "$COMMAND_TIMEOUT" "$TTC" "$@"; }

cleanup() {
	set +e
	if test -S "$SOCKET_A"; then client_a rm -f "$NAME" >/dev/null 2>&1; fi
	if test -S "$SOCKET_B"; then client_b node stop node-b --stop-all >/dev/null 2>&1; fi
	if test -S "$SOCKET_A"; then client_a node stop node-a --stop-all >/dev/null 2>&1; fi
	if test -n "$PID_B"; then kill "$PID_B" >/dev/null 2>&1; wait "$PID_B" >/dev/null 2>&1; fi
	if test -n "$PID_A"; then kill "$PID_A" >/dev/null 2>&1; wait "$PID_A" >/dev/null 2>&1; fi
	if test -z "${TTC_DISTRIBUTED_ACTIVE_FS_KEEP_ROOT:-}"; then rm -rf "$ROOT"; fi
}
trap cleanup EXIT HUP INT TERM

wait_ready() {
	root=$1
	log=$2
	for unused in $(seq 1 600); do
		if TTC_LOCAL_ROOT=$root timeout 2s "$TTC" node ls >/dev/null 2>&1; then return 0; fi
		sleep 0.05
	done
	tail -100 "$log" >&2 || true
	return 1
}

mkdir -p "$SHARED"
# A daemon's root holds its socket, and must be private.
mkdir -p -m 700 "$LOCAL_A" "$LOCAL_B"
TTC_ROOT=$SHARED TTC_LOCAL_ROOT=$LOCAL_A \
	"$TTCD" --node node-a \
	--listen "127.0.0.1:$PORT_A" --advertise "127.0.0.1:$PORT_A" \
	--memory-target 64GiB $DAEMON_EXTRA >"$LOCAL_A/ttcd.log" 2>&1 &
PID_A=$!
wait_ready "$LOCAL_A" "$LOCAL_A/ttcd.log"
TTC_ROOT=$SHARED TTC_LOCAL_ROOT=$LOCAL_B \
	"$TTCD" --node node-b \
	--listen "127.0.0.1:$PORT_B" --advertise "127.0.0.1:$PORT_B" \
	--memory-target 64GiB $DAEMON_EXTRA >"$LOCAL_B/ttcd.log" 2>&1 &
PID_B=$!
wait_ready "$LOCAL_B" "$LOCAL_B/ttcd.log"

client_a pull "$IMAGE" >/dev/null
client_a run -d --node node-a --name "$NAME" "$IMAGE" python -c '
import hashlib, os, sqlite3, time
memory = bytearray(128 << 20)
for offset in range(0, len(memory), 4096):
    memory[offset] = (offset // 4096) % 251
db = sqlite3.connect("/workload.db", timeout=30)
db.execute("pragma journal_mode=wal")
db.execute("create table if not exists heartbeat(t integer, value text)")
while True:
    db.execute("insert into heartbeat values (?, ?)", (time.time_ns(), hashlib.sha256(memory[:4096]).hexdigest()))
    db.commit()
    time.sleep(.002)
' >/dev/null

client_a exec "$NAME@main" python -c 'import sqlite3; print(sqlite3.connect("/workload.db").execute("pragma integrity_check").fetchone()[0])' | grep -Fx ok >/dev/null
client_a migrate --node node-b "$NAME@main" >/dev/null

# Keep accessing both inherited image files and the actively mutated database
# long enough for asynchronous graph publication and filesystem drain to run.
for iteration in $(seq 1 40); do
	client_b exec "$NAME@main" python -c 'import sqlite3; print(sqlite3.connect("/workload.db").execute("pragma integrity_check").fetchone()[0])' | grep -Fx ok >/dev/null
	client_b exec "$NAME@main" sh -c 'test -x /usr/local/bin/python && test -s /workload.db'
	sleep 0.25
done

client_a rm -f "$NAME" >/dev/null
client_b node stop node-b --stop-all >/dev/null
wait "$PID_B"
PID_B=
client_a node stop node-a --stop-all >/dev/null
wait "$PID_A"
PID_A=

echo "TTC active-filesystem sparse migration passed"
